package e2e_test

// Scratch evidence driver for the kagent-dev/kagent#3395 review. Not part of any PR.
//
// Admission goes straight through database.Client.BeginCrashedSessionRecovery,
// the same call RevertSession makes, so the recovery is admitted without the
// request that would also execute it inline. That reproduces the state a
// RevertSession leaves when its first execution attempt does not complete.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/mockllm"
	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

func rvLog(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("[rv3395 %s] "+format, append([]any{time.Now().UTC().Format("15:04:05.000")}, args...)...)
}

// rvAdmit admits crash recovery exactly as RevertSession does after it has
// observed the Actor. The helper runs beside the controller, which holds the
// database client certificate.
func rvAdmit(t *testing.T, sessionID string, actor *ateapipb.Actor) {
	t.Helper()
	snapshot := substrate.LatestDurableActorSnapshot(actor)
	out, err := exec.CommandContext(t.Context(), "bash", os.Getenv("RV_ADMIT"), sessionID, actor.GetMetadata().GetUid(),
		actor.GetMetadata().GetAtespace(), substrate.SnapshotURI(snapshot), snapshot.GetFidelity().String()).CombinedOutput()
	rvLog(t, "admit for Session %s (Actor uid %s, state %s): %s", sessionID, actor.GetMetadata().GetUid(), actor.GetStatus().GetState(), strings.TrimSpace(string(out)))
	require.NoError(t, err)
}

func rvScaleAteAPI(t *testing.T, kube ctrlclient.Client, replicas int32) {
	t.Helper()
	ctx := t.Context()
	deploy := &appsv1.Deployment{}
	require.NoError(t, kube.Get(ctx, ctrlclient.ObjectKey{Namespace: "ate-system", Name: "ate-api-server"}, deploy))
	deploy.Spec.Replicas = &replicas
	require.NoError(t, kube.Update(ctx, deploy))
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pods, ctrlclient.InNamespace("ate-system"), ctrlclient.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
			return false, err
		}
		ready := 0
		for _, pod := range pods.Items {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready++
				}
			}
		}
		if replicas == 0 {
			return len(pods.Items) == 0, nil
		}
		return ready >= int(replicas), nil
	}))
	rvLog(t, "ate-api scaled to %d", replicas)
}

func rvSession(t *testing.T, fixture *interactionFixture) *apiv1.Session {
	t.Helper()
	response, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	return response.Session
}

// Item 3: a recovery whose first attempt cannot read the Actor must not block Delete.
func TestRV3395DeleteAfterFailedRecoveryAttempt(t *testing.T) {
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	cfg, err := mockllm.LoadConfigFromFile("mocks/invoke_agent.json", interactionMocks)
	require.NoError(t, err)
	fixture := newInteractionFixture(t, harness, target, reachableModelURL(t, startMockLLMConfig(t, cfg)))
	_, _, task := fixture.send(t, "What is 2+2?")
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	assertActorSuspended(t, fixture)
	session := rvSession(t, fixture)
	actor, err := findSubstrateActor(fixture.ctx, fixture.system, session.Agent.Namespace, substrate.ActorName(fixture.sessionID))
	require.NoError(t, err)
	rvLog(t, "Session %s READY with Actor %s", fixture.sessionID, actor.GetStatus().GetState())

	// Substrate is unreachable for the whole first attempt.
	rvScaleAteAPI(t, kube, 0)
	restored := false
	defer func() {
		if !restored {
			rvScaleAteAPI(t, kube, 2)
		}
	}()
	rvAdmit(t, fixture.sessionID, actor)
	time.Sleep(12 * time.Second) // two recovery scans at the fleet's 5s interval
	current := rvSession(t, fixture)
	rvLog(t, "after failed recovery scans: state=%s operation=%s failure=%q", current.State, current.Operation, current.GetFailure().GetReason())
	_, err = fixture.sessions.DeleteSession(fixture.ctx, &apiv1.DeleteSessionRequest{SessionId: fixture.sessionID})
	rvLog(t, "DeleteSession while Substrate is down: code=%s err=%v", status.Code(err), err)

	rvScaleAteAPI(t, kube, 2)
	restored = true
	// The controller's ate-api channel needs a moment to resolve the new Pods.
	attempts := 0
	for deadline := time.Now().Add(2 * time.Minute); ; {
		attempts++
		_, err = fixture.sessions.DeleteSession(fixture.ctx, &apiv1.DeleteSessionRequest{SessionId: fixture.sessionID})
		if err == nil || time.Now().After(deadline) {
			break
		}
		rvLog(t, "DeleteSession attempt %d after Substrate returns: code=%s", attempts, status.Code(err))
		time.Sleep(2 * time.Second)
	}
	rvLog(t, "DeleteSession after Substrate returns: attempts=%d code=%s err=%v", attempts, status.Code(err), err)
	response, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
	rvLog(t, "final GetSession: code=%s state=%s operation=%s", status.Code(err), response.GetSession().GetState(), response.GetSession().GetOperation())
}

// Items 1 and 2: Start's own ticker retries an admitted recovery with no request
// in flight, and the revert runs through the shared execute path.
func TestRV3395StartScanRevertsAdmittedCrash(t *testing.T) {
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
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
	_, _, task := fixture.send(t, "What is 2+2?")
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	assertActorSuspended(t, fixture)
	session := rvSession(t, fixture)
	namespace, name := session.Agent.Namespace, substrate.ActorName(fixture.sessionID)
	blockNext.Store(true)
	_, turn := newMessageRequest(t, "What is 2+2?")
	turn.Tenant, turn.Message.ContextId = fixture.tenant, fixture.sessionID
	turnCtx, stopTurn := context.WithCancel(fixture.ctx)
	defer stopTurn()
	go func() { _, _ = sendMessageWithRetry(turnCtx, fixture.client, turn) }()
	select {
	case <-blocked:
	case <-time.After(time.Minute):
		t.Fatal("second turn never reached the blocking model")
	}
	running, err := findSubstrateActor(fixture.ctx, fixture.system, namespace, name)
	require.NoError(t, err)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, running.GetStatus().GetState())
	assignment := running.GetStatus().GetWorkerAssignment()
	pod := &corev1.Pod{}
	require.NoError(t, kube.Get(fixture.ctx, ctrlclient.ObjectKey{Namespace: assignment.GetWorkerNamespace(), Name: assignment.GetWorkerPod()}, pod))
	require.NoError(t, kube.Delete(fixture.ctx, pod, &ctrlclient.DeleteOptions{
		GracePeriodSeconds: new(int64(0)), Preconditions: &metav1.Preconditions{UID: &pod.UID},
	}))
	rvLog(t, "deleted worker Pod %s/%s under running Actor %s", pod.Namespace, pod.Name, running.GetMetadata().GetUid())
	var crashed *ateapipb.Actor
	require.NoError(t, wait.PollUntilContextTimeout(fixture.ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		actor, err := findSubstrateActor(ctx, fixture.system, namespace, name)
		crashed = actor
		return actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED, err
	}))
	stopTurn()
	rvAdmit(t, fixture.sessionID, crashed)
	admittedAt := time.Now()
	var recovered *apiv1.Session
	require.NoError(t, wait.PollUntilContextTimeout(fixture.ctx, 250*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		recovered = rvSession(t, fixture)
		return recovered.State == apiv1.RuntimeState_RUNTIME_STATE_READY && recovered.Operation == apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE, nil
	}))
	actor, err := findSubstrateActor(fixture.ctx, fixture.system, namespace, name)
	require.NoError(t, err)
	rvLog(t, "background scan recovered the Session %.1fs after admission, with no RevertSession call: state=%s; Actor uid %s state %s",
		time.Since(admittedAt).Seconds(), recovered.State, actor.GetMetadata().GetUid(), actor.GetStatus().GetState())
	require.Equal(t, running.GetMetadata().GetUid(), actor.GetMetadata().GetUid())
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, actor.GetStatus().GetState())
	_, _, next := fixture.send(t, "What is 2+2?")
	rvLog(t, "next turn after recovery: %s %q", next.Status.State, taskText(next))
	require.Equal(t, a2a.TaskStateCompleted, next.Status.State)
}
