package e2e_test

// Scratch evidence driver for kagent-dev/kagent#3391. Not part of the PR.
//
// It holds one Go ADK turn at the model, stops the controller, releases the
// turn into the outage, restores the controller, and records what the turn,
// its scheduled-run execution and a follow-up message do afterwards. It does
// not fail on the outcome, so the same driver documents a runtime without the
// fix and one with it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

var (
	evidenceLabel  = flag.String("evidence-label", "", "before or after; empty skips TestTaskStoreOutageEvidence")
	evidenceDir    = flag.String("evidence-dir", "", "directory for the JSON report")
	evidenceOutage = flag.Duration("evidence-outage", 30*time.Second, "how long the controller stays scaled to zero")
	evidenceUser   = flag.String("evidence-user", "admin@kagent.dev", "owner of the scheduled run, so the UI lists it")
	evidenceHold   = flag.Duration("evidence-hold", 0, "keep the agent and mock model alive this long after the report, until <dir>/<label>.release exists")
)

type outageEvent struct {
	At   string `json:"at"`
	Wall string `json:"wall"`
	What string `json:"what"`
}

type outageReport struct {
	Label               string        `json:"label"`
	RuntimeImage        string        `json:"runtime_image"`
	Agent               string        `json:"agent"`
	ScheduleID          string        `json:"schedule_id"`
	ExecutionID         string        `json:"execution_id"`
	SessionID           string        `json:"session_id"`
	TaskID              string        `json:"task_id"`
	Outage              string        `json:"outage"`
	ControllerDownFor   string        `json:"controller_down_for"`
	FinalExecution      string        `json:"final_execution_state"`
	FailureReason       string        `json:"failure_reason,omitempty"`
	FinalTaskState      string        `json:"final_task_state"`
	FinalTaskText       string        `json:"final_task_text"`
	ModelCallsForTurn   int32         `json:"model_calls_for_turn"`
	FollowUpAccepted    bool          `json:"follow_up_accepted"`
	FollowUpState       string        `json:"follow_up_state,omitempty"`
	FollowUpText        string        `json:"follow_up_text,omitempty"`
	FollowUpError       string        `json:"follow_up_error,omitempty"`
	Events              []outageEvent `json:"events"`
}

var evidenceControllerLabels = ctrlclient.MatchingLabels{"app.kubernetes.io/component": "controller"}

func TestTaskStoreOutageEvidence(t *testing.T) {
	if *evidenceLabel == "" {
		t.Skip("set -evidence-label")
	}
	require.NotEmpty(t, *evidenceDir)
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	scale := evidenceScaler(t, kube)
	require.NoError(t, scale(t.Context(), 1), "restore a controller a previous run left at zero")
	require.NoError(t, evidenceWaitControllerReady(t.Context(), kube))

	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	modelURL, started, release, calls := startScheduledRecoveryModel(t)
	defer release()
	template := createInteractionTemplate(t, harness, modelURL)
	tenant := "kagent/" + template

	report := &outageReport{Label: *evidenceLabel, Agent: tenant, Outage: evidenceOutage.String(), RuntimeImage: os.Getenv("KAGENT_E2E_RUNTIME_IMAGE")} //nolint:forbidigo // scratch evidence driver
	begin := time.Now()
	note := func(format string, args ...any) {
		event := outageEvent{At: time.Since(begin).Round(100 * time.Millisecond).String(), Wall: time.Now().UTC().Format(time.RFC3339Nano), What: fmt.Sprintf(format, args...)}
		report.Events = append(report.Events, event)
		t.Logf("[%8s] %s", event.At, event.What)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-user-id", *evidenceUser)
	conn := newControllerConn(t, target)
	schedules, tasks := apiv1.NewScheduledRunServiceClient(conn), a2apb.NewA2AServiceClient(conn)

	created, err := schedules.CreateScheduledRun(ctx, &apiv1.CreateScheduledRunRequest{
		RequestId: uuid.NewString(),
		Agent:     &apiv1.ResourceReference{Namespace: "kagent", Name: template},
		Config: &apiv1.ScheduledRunConfig{
			Name: "controller-restart-" + *evidenceLabel, Schedule: "* * * * *", TimeZone: "UTC",
			Prompt: "What is 2+2?", Paused: true, ExecutionTimeout: durationpb.New(15 * time.Minute),
		},
	})
	require.NoError(t, err)
	report.ScheduleID = created.GetScheduledRun().GetId()
	triggered, err := schedules.TriggerScheduledRun(ctx, &apiv1.TriggerScheduledRunRequest{ScheduledRunId: report.ScheduleID, RequestId: uuid.NewString()})
	require.NoError(t, err)
	report.ExecutionID = triggered.GetExecution().GetId()
	note("scheduled run %s triggered as %s; prompt %q", report.ScheduleID, *evidenceUser, "What is 2+2?")
	select {
	case <-started:
	case <-time.After(3 * time.Minute):
		t.Fatal("the agent did not reach the model")
	}
	note("the agent reached the mock model; the turn is held there")

	var execution *apiv1.ScheduledRunExecution
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		result, err := schedules.GetScheduledRunExecution(ctx, &apiv1.GetScheduledRunExecutionRequest{ExecutionId: report.ExecutionID})
		if err != nil {
			return false, nil
		}
		execution = result.GetExecution()
		return execution.GetState() == apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING && execution.GetTaskId() != "", nil
	}))
	report.SessionID, report.TaskID = execution.GetSessionId(), execution.GetTaskId()
	note("execution %s is RUNNING: session %s, task %s", report.ExecutionID, report.SessionID, report.TaskID)

	// The outage: no controller Pod is left before the turn is released, so the
	// turn's final save can only reach a controller that is gone.
	stopping := time.Now()
	require.NoError(t, scale(ctx, 0))
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pods, ctrlclient.InNamespace("kagent"), evidenceControllerLabels); err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	}))
	release()
	note("controller scaled to zero and its Pod gone after %s; the model is released, so the agent answers and saves now", time.Since(stopping).Round(time.Second))
	time.Sleep(*evidenceOutage)
	note("outage held for %s; scaling the controller back to one replica", *evidenceOutage)
	require.NoError(t, scale(ctx, 1))
	require.NoError(t, evidenceWaitControllerReady(ctx, kube))
	report.ControllerDownFor = time.Since(stopping).Round(time.Second).String()
	note("controller Ready again; it was unavailable for %s in total", report.ControllerDownFor)

	conn = newControllerConn(t, target)
	schedules, tasks = apiv1.NewScheduledRunServiceClient(conn), a2apb.NewA2AServiceClient(conn)
	getTask := func(ctx context.Context) (*a2atype.Task, error) {
		request, err := pbconv.ToProtoGetTaskRequest(&a2atype.GetTaskRequest{Tenant: tenant, ID: a2atype.TaskID(report.TaskID)})
		if err != nil {
			return nil, err
		}
		result, err := tasks.GetTask(ctx, request)
		if err != nil {
			return nil, err
		}
		return pbconv.FromProtoTask(result)
	}
	var lastExecution, lastTask string
	var task *a2atype.Task
	observeUntil := time.Now().Add(3 * time.Minute)
	for time.Now().Before(observeUntil) {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, execErr := schedules.GetScheduledRunExecution(callCtx, &apiv1.GetScheduledRunExecutionRequest{ExecutionId: report.ExecutionID})
		current, taskErr := getTask(callCtx)
		cancel()
		if execErr == nil {
			execution = result.GetExecution()
			if state := execution.GetState().String(); state != lastExecution {
				lastExecution = state
				note("execution state: %s %s", state, execution.GetFailureReason())
			}
		} else if code := status.Code(execErr); code != codes.Unavailable && code != codes.DeadlineExceeded {
			note("read execution: %v", execErr)
		}
		if taskErr == nil {
			task = current
			if state := string(task.Status.State); state != lastTask {
				lastTask = state
				note("task state: %s", state)
			}
		}
		if execution.GetCompletedAt() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if execution.GetCompletedAt() == nil {
		note("after 3 minutes the execution is still %s and the task still %s", lastExecution, lastTask)
	}
	report.FinalExecution, report.FailureReason, report.FinalTaskState = lastExecution, execution.GetFailureReason(), lastTask
	if task != nil {
		report.FinalTaskText = taskText(task)
	}
	report.ModelCallsForTurn = calls.Load()

	// A Session whose turn never settled refuses the next send.
	_, request := newMessageRequest(t, "What is 2+2?")
	request.Message.ContextId = report.SessionID
	request.Tenant = tenant
	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	response, err := sendMessageWithRetry(sendCtx, tasks, request)
	cancel()
	if err != nil {
		report.FollowUpError = err.Error()
		note("follow-up message on the same session refused: %v", err)
	} else {
		report.FollowUpAccepted = true
		if decoded, err := pbconv.FromProtoSendMessageResponse(response); err == nil {
			if next, ok := decoded.(*a2atype.Task); ok {
				report.FollowUpState, report.FollowUpText = string(next.Status.State), taskText(next)
			}
		}
		note("follow-up message on the same session accepted: %s %q", report.FollowUpState, report.FollowUpText)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(*evidenceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(*evidenceDir, *evidenceLabel+".json"), data, 0o644))
	t.Logf("report:\n%s", data)

	if *evidenceHold > 0 {
		marker := filepath.Join(*evidenceDir, *evidenceLabel+".release")
		t.Logf("holding up to %s for screenshots; create %s to finish", *evidenceHold, marker)
		_ = wait.PollUntilContextTimeout(t.Context(), 2*time.Second, *evidenceHold, true, func(context.Context) (bool, error) {
			_, err := os.Stat(marker)
			return err == nil, nil
		})
	}
}

func evidenceScaler(t *testing.T, kube ctrlclient.Client) func(context.Context, int32) error {
	t.Helper()
	deployments := &appsv1.DeploymentList{}
	require.NoError(t, kube.List(t.Context(), deployments, ctrlclient.InNamespace("kagent"), evidenceControllerLabels))
	require.Len(t, deployments.Items, 1)
	key := ctrlclient.ObjectKeyFromObject(&deployments.Items[0])
	return func(ctx context.Context, count int32) error {
		current := &appsv1.Deployment{}
		if err := kube.Get(ctx, key, current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Spec.Replicas = &count
		return kube.Patch(ctx, current, ctrlclient.MergeFrom(base))
	}
}

func evidenceWaitControllerReady(ctx context.Context, kube ctrlclient.Client) error {
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pods, ctrlclient.InNamespace("kagent"), evidenceControllerLabels); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
		}
		return false, nil
	})
}
