package e2e_test

// Scratch evidence driver for the review of kagent-dev/kagent#3397. Not part of any PR.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/mockllm"
	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

func rvRecovery(t *testing.T, label string, session *apiv1.Session) {
	t.Helper()
	recovery, _ := protojson.Marshal(session.GetRecovery())
	t.Logf("RV3397 %s state=%s operation=%s recovery=%s", label, session.GetState(), session.GetOperation(), recovery)
}

func TestRV3397CrashRevert(t *testing.T) {
	target := interactionTarget(t)
	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	cfg, err := mockllm.LoadConfigFromFile("mocks/invoke_agent.json", interactionMocks)
	require.NoError(t, err)
	upstream, err := url.Parse(startMockLLMConfig(t, cfg))
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var blockNext atomic.Bool
	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && blockNext.CompareAndSwap(true, false) {
			blocked <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	require.NoError(t, gate.Listener.Close())
	gate.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	gate.Start()
	t.Cleanup(gate.Close)
	fixture := newInteractionFixture(t, harness, target, reachableModelURL(t, gate.URL))
	defer func() {
		close(release)
		gate.CloseClientConnections()
	}()
	observe := func(label string) *apiv1.Session {
		t.Helper()
		response, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		rvRecovery(t, "GetSession "+label, response.Session)
		return response.Session
	}
	listed := func(label string) *apiv1.Session {
		t.Helper()
		for token := ""; ; {
			page, err := fixture.sessions.ListSessions(fixture.ctx, &apiv1.ListSessionsRequest{Page: &apiv1.PageRequest{Limit: 100, PageToken: token}})
			require.NoError(t, err)
			for _, session := range page.Sessions {
				if session.Id == fixture.sessionID {
					rvRecovery(t, "ListSessions "+label, session)
					return session
				}
			}
			token = page.GetPage().GetNextPageToken()
			require.NotEmpty(t, token, "session missing from ListSessions")
		}
	}

	first, _, task := fixture.send(t, "What is 2+2?")
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	t.Logf("RV3397 first turn task=%s message=%s (completed, then quiesced into a tagged snapshot)", task.ID, first.ID)
	assertActorSuspended(t, fixture)
	require.Equal(t, new(false), observe("healthy").GetRecovery().Needed)

	session := observe("before second turn")
	namespace, name := session.Agent.Namespace, substrate.ActorName(fixture.sessionID)
	blockNext.Store(true)
	_, turn := newMessageRequest(t, "What is 2+2?")
	turn.Tenant, turn.Message.ContextId = fixture.tenant, fixture.sessionID
	t.Logf("RV3397 second turn message=%s (held at the model, then its worker is deleted)", turn.Message.MessageId)
	turnCtx, stopTurn := context.WithCancel(fixture.ctx)
	defer stopTurn()
	sendDone := make(chan error, 1)
	go func() { _, err := sendMessageWithRetry(turnCtx, fixture.client, turn); sendDone <- err }()
	select {
	case <-blocked:
	case <-time.After(time.Minute):
		t.Fatal("second turn never reached the blocking model")
	}
	running, err := findSubstrateActor(fixture.ctx, fixture.system, namespace, name)
	require.NoError(t, err)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, running.GetStatus().GetState())
	t.Logf("RV3397 actor snapshot=%s", substrate.SnapshotURI(substrate.LatestDurableActorSnapshot(running)))
	require.Equal(t, new(false), observe("running").GetRecovery().Needed)
	_, err = fixture.sessions.RevertSession(fixture.ctx, &apiv1.RevertSessionRequest{SessionId: fixture.sessionID})
	t.Logf("RV3397 RevertSession on running actor: %v", err)
	require.Error(t, err)

	assignment := running.GetStatus().GetWorkerAssignment()
	kube := interactionKubeClient(t)
	pod := &corev1.Pod{}
	require.NoError(t, kube.Get(fixture.ctx, ctrlclient.ObjectKey{Namespace: assignment.GetWorkerNamespace(), Name: assignment.GetWorkerPod()}, pod))
	require.NoError(t, kube.Delete(fixture.ctx, pod, &ctrlclient.DeleteOptions{
		GracePeriodSeconds: new(int64(0)), Preconditions: &metav1.Preconditions{UID: &pod.UID},
	}))
	t.Logf("RV3397 deleted worker pod %s/%s", assignment.GetWorkerNamespace(), assignment.GetWorkerPod())
	require.NoError(t, wait.PollUntilContextTimeout(fixture.ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		actor, err := findSubstrateActor(ctx, fixture.system, namespace, name)
		return actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED, err
	}))
	crashed := observe("crashed")
	require.Equal(t, new(true), crashed.GetRecovery().Needed, "the observation offers a revert")
	listedCrashed := listed("crashed")

	reverted, err := fixture.sessions.RevertSession(fixture.ctx, &apiv1.RevertSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err, "Revert admits exactly what the observation offered")
	rvRecovery(t, "RevertSession response", reverted.Session)
	after := observe("after revert")
	require.Equal(t, new(false), after.GetRecovery().Needed)
	listedAfter := listed("after revert")
	failed := after.GetRecovery().GetFailedMessages()
	require.Len(t, failed, 1)
	require.Equal(t, turn.Message.MessageId, failed[0].MessageId, "only the input after the tagged snapshot is marked")
	require.NotEqual(t, first.ID, failed[0].MessageId)
	if os.Getenv("RV_EXPECT_STRIPPED") == "1" {
		require.Nil(t, listedCrashed.Recovery)
		require.Nil(t, listedAfter.Recovery)
		require.Nil(t, reverted.Session.Recovery)
	}

	stopTurn()
	select {
	case <-sendDone:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted caller did not stop")
	}
	_, _, next := fixture.send(t, "What is 2+2?")
	require.Equal(t, a2a.TaskStateCompleted, next.Status.State)
	t.Logf("RV3397 next turn task=%s completed after recovery", next.ID)
	observe("after next turn")
}

func rvShell(t *testing.T, variable string) {
	t.Helper()
	command := os.Getenv(variable)
	require.NotEmpty(t, command, variable)
	output, err := exec.Command("bash", "-c", command).CombinedOutput()
	t.Logf("RV3397 %s: %s", variable, output)
	require.NoError(t, err)
}

func TestRV3397MCPLatency(t *testing.T) {
	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	fixture := newInteractionFixture(t, harness, interactionTarget(t), startInteractionMock(t))
	endpoint := mcpEndpoint(t)
	handle := mcpInvoke(t, endpoint, fixture.sessionID, "What is 2+2?", true)["taskId"].(string)
	waitMCPTask(t, endpoint, handle, "completed")
	assertActorSuspended(t, fixture)
	measure := func(phase string) {
		for attempt := 1; attempt <= 3; attempt++ {
			start := time.Now()
			result := mcpCall(t, endpoint, "tasks/get", map[string]any{"taskId": handle}, true)["result"].(map[string]any)
			t.Logf("RV3397 latency phase=%s call=mcp:tasks/get attempt=%d duration_ms=%d status=%v", phase, attempt, time.Since(start).Milliseconds(), result["status"])
		}
		start := time.Now()
		response, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		recovery, _ := protojson.Marshal(response.Session.GetRecovery())
		t.Logf("RV3397 latency phase=%s call=GetSession duration_ms=%d recovery=%s", phase, time.Since(start).Milliseconds(), recovery)
	}
	measure("substrate-api-up")
	rvShell(t, "RV_PAUSE")
	resumed := false
	defer func() {
		if !resumed {
			rvShell(t, "RV_RESUME")
		}
	}()
	measure("substrate-api-paused")
	rvShell(t, "RV_RESUME")
	resumed = true
}
