package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// recoverSessionPage checks accepted turns and admitted recoveries in ID order.
// A quiet runtime or a failed read cannot authorize recovery.
func (w *ActorWorkflow) recoverSessionPage(ctx context.Context, afterID string) (string, error) {
	const pageSize = 100
	sessions, err := w.store.ListSessionRecoveryCandidates(ctx, afterID, pageSize)
	if err != nil {
		return afterID, err
	}
	for _, session := range sessions {
		attemptCtx, cancel := context.WithTimeout(ctx, database.RuntimeOperationTimeout)
		err := w.recoverSessionCandidate(attemptCtx, session)
		cancel()
		if err != nil && !errors.Is(err, database.ErrConflict) && ctx.Err() == nil {
			logging.FromContext(ctx).ErrorContext(ctx, "recover crashed session", "session_id", session.Id, "error", err)
		}
		afterID = session.Id
		if ctx.Err() != nil {
			return afterID, ctx.Err()
		}
	}
	if len(sessions) < pageSize {
		afterID = ""
	}
	return afterID, nil
}

// recoverSessionCandidate admits recovery only for the recorded crashed Actor.
func (w *ActorWorkflow) recoverSessionCandidate(ctx context.Context, session *apiv1.Session) error {
	_, err := w.Revert(ctx, session)
	if errors.Is(err, database.ErrFailedPrecondition) {
		return nil
	}
	return err
}

// Revert restores a confirmed crashed runtime without replaying its interrupted turn.
// Retries join the same durable operation and preserve the Actor identity.
func (w *ActorWorkflow) Revert(ctx context.Context, session *apiv1.Session) (*apiv1.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, database.RuntimeOperationTimeout)
	defer cancel()
	if session.State == apiv1.RuntimeState_RUNTIME_STATE_FAILED && session.Failure.GetReason() == database.SessionRuntimeCrashed {
		return w.run(ctx, session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_RESUME)
	}
	if session.State != apiv1.RuntimeState_RUNTIME_STATE_READY || session.Operation != apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return nil, database.ErrConflict
	}
	revision, err := w.store.GetRuntimeRevision(ctx, session.PreparedRevision)
	if err != nil {
		return nil, err
	}
	actor, err := w.actors.GetActor(ctx, revision.ActorTemplateAtespace, substrate.ActorName(session.Id))
	if err != nil {
		return nil, err
	}
	if err := w.verifyActor(ctx, session, revision, actor); err != nil {
		return nil, err
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_CRASHED {
		return nil, fmt.Errorf("runtime is not crashed: %w", database.ErrFailedPrecondition)
	}
	snapshot := actor.GetStatus().GetExternalSnapshot()
	operation, err := w.store.BeginCrashedSessionRecovery(ctx, session.Id, actor.GetMetadata().GetUid(), database.SessionTaskSnapshot{
		Atespace: actor.GetMetadata().GetAtespace(), URI: snapshot.GetSnapshotUri(),
		ContentScope: strings.TrimPrefix(snapshot.GetContentScope().String(), "SNAPSHOT_CONTENT_SCOPE_"),
	})
	if err != nil {
		return nil, err
	}
	return w.execute(ctx, operation)
}

// recoverCrashedSession uses the ordinary bounded lifecycle claim.
// Retries retain the Actor identity and reconcile lost revert responses.
func (w *ActorWorkflow) recoverCrashedSession(ctx context.Context, operation *database.SessionOperation) (_ *apiv1.Session, err error) {
	session := operation.Instance
	if session.Operation != apiv1.RuntimeOperation_RUNTIME_OPERATION_RESUME {
		return nil, database.ErrConflict
	}
	executorID := uuid.New()
	claimed, err := w.store.ClaimSessionOperation(ctx, session.Id, operation.ID, executorID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		current, err := w.store.GetSessionOperation(ctx, session.Id, operation.ID)
		if err != nil {
			return nil, err
		}
		return operationOutcome(current)
	}
	defer func() {
		if err != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = errors.Join(err, w.store.ReleaseRuntimeOperation(releaseCtx, session.Id, operation.ID, executorID))
		}
	}()
	revision, err := w.store.GetRuntimeRevision(ctx, session.PreparedRevision)
	if err != nil {
		return nil, err
	}
	atespace, name := revision.ActorTemplateAtespace, substrate.ActorName(session.Id)
	actor, err := w.actors.GetActor(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	if err := w.verifyActor(ctx, session, revision, actor); err != nil {
		return nil, err
	}
	before := actor
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_CRASHED, ateapipb.ActorState_ACTOR_STATE_REVERTING:
		actor, err = w.actors.RevertActor(ctx, atespace, name)
		if err != nil {
			return nil, fmt.Errorf("revert crashed Actor %s/%s: %w", atespace, name, err)
		}
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
	default:
		return nil, fmt.Errorf("crashed Actor %s/%s has unexpected state %s", atespace, name, actor.GetStatus().GetState())
	}
	if !validActorIdentity(actor, revision, name) || actor.GetMetadata().GetUid() != before.GetMetadata().GetUid() || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED || !proto.Equal(before.GetStatus().GetExternalSnapshot(), actor.GetStatus().GetExternalSnapshot()) {
		return nil, fmt.Errorf("revert Actor %s/%s returned unexpected identity or state", atespace, name)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.store.FinishSessionOperation(finishCtx, session.Id, operation.ID, executorID, "", actor.GetMetadata().GetUid(), "")
}
