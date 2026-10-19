package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

// TestSessionRevertCrashedRuntime deletes an exclusive worker for an idle Session.
// The active-turn recovery scan cannot repair this Session before the explicit RPC.
func TestSessionRevertCrashedRuntime(t *testing.T) {
	target := interactionTarget(t)
	forEachHarness(t, func(t *testing.T, harness testHarness) {
		fixture := newInteractionFixture(t, harness, target, startInteractionMock(t))
		_, _, task := fixture.send(t, "What is 2+2?")
		require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
		assertActorSuspended(t, fixture)
		getTask := &a2apb.GetTaskRequest{Tenant: fixture.tenant, Id: string(task.ID)}
		before, err := fixture.client.GetTask(fixture.ctx, getTask)
		require.NoError(t, err)
		session, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		namespace, name := session.Session.Agent.Namespace, substrate.ActorName(fixture.sessionID)
		require.NoError(t, wait.PollUntilContextTimeout(fixture.ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			_, err := fixture.sessions.SuspendSession(ctx, &apiv1.SuspendSessionRequest{SessionId: fixture.sessionID})
			if status.Code(err) == codes.FailedPrecondition {
				return false, nil
			}
			return err == nil, err
		}))
		_, err = fixture.sessions.ResumeSession(fixture.ctx, &apiv1.ResumeSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		running, err := findSubstrateActor(fixture.ctx, fixture.system, namespace, name)
		require.NoError(t, err)
		require.NotNil(t, running)
		require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, running.GetStatus().GetState())
		require.NotEmpty(t, running.GetStatus().GetExternalSnapshot().GetSnapshotUri())
		request := &apiv1.RevertSessionRequest{SessionId: fixture.sessionID}
		_, err = fixture.sessions.RevertSession(fixture.ctx, request)
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "healthy runtimes cannot revert")
		assignment := running.GetStatus().GetWorkerAssignment()
		require.Equal(t, namespace, assignment.GetWorkerNamespace())
		require.NotEmpty(t, assignment.GetWorkerPod())
		for token := ""; ; {
			page, err := fixture.system.ListSubstrateActors(fixture.ctx, &apiv1.ListSubstrateActorsRequest{
				Page: &apiv1.PageRequest{Limit: 100, PageToken: token},
			})
			require.NoError(t, err)
			require.Empty(t, page.GetAteApiError())
			for _, actor := range page.GetActors() {
				other := actor.GetStatus().GetWorkerAssignment()
				if actor.GetMetadata().GetUid() != running.GetMetadata().GetUid() {
					require.False(t, other.GetWorkerNamespace() == assignment.GetWorkerNamespace() && other.GetWorkerPod() == assignment.GetWorkerPod(),
						"refuse to delete a worker shared with another Actor")
				}
			}
			token = page.GetPage().GetNextPageToken()
			if token == "" {
				break
			}
		}
		kube := interactionKubeClient(t)
		pod := &corev1.Pod{}
		require.NoError(t, kube.Get(fixture.ctx, ctrlclient.ObjectKey{
			Namespace: assignment.GetWorkerNamespace(), Name: assignment.GetWorkerPod(),
		}, pod))
		require.NotEmpty(t, pod.UID)
		require.NoError(t, kube.Delete(fixture.ctx, pod, &ctrlclient.DeleteOptions{
			GracePeriodSeconds: new(int64(0)), Preconditions: &metav1.Preconditions{UID: &pod.UID},
		}))
		err = wait.PollUntilContextTimeout(fixture.ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			actor, err := findSubstrateActor(ctx, fixture.system, namespace, name)
			return actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED, err
		})
		require.NoError(t, err)
		reverted, err := fixture.sessions.RevertSession(fixture.ctx, request)
		require.NoError(t, err)
		require.Equal(t, apiv1.RuntimeState_RUNTIME_STATE_READY, reverted.Session.State)
		recovered, err := findSubstrateActor(fixture.ctx, fixture.system, namespace, name)
		require.NoError(t, err)
		require.Equal(t, running.GetMetadata().GetUid(), recovered.GetMetadata().GetUid())
		require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, recovered.GetStatus().GetState())
		require.True(t, proto.Equal(running.GetStatus().GetExternalSnapshot(), recovered.GetStatus().GetExternalSnapshot()))
		retained, err := fixture.client.GetTask(fixture.ctx, getTask)
		require.NoError(t, err)
		require.True(t, proto.Equal(before, retained), "recovery preserves the completed task and its history")
		_, _, next := fixture.send(t, "What is 2+2?")
		require.Equal(t, a2a.TaskStateCompleted, next.Status.State)
		require.Contains(t, taskText(next), "4")
	})
}
