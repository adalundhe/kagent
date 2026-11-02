package taskstore

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// A proxy that takes 300ms to discover the controller is gone.
func TestScratchSlowProxyBudget(t *testing.T) {
	api := &readinessServer{}
	store := newReadinessStore(t, api, nil, func(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
		err := invoke(ctx, method, req, reply, conn, options...)
		t.Logf("attempt code=%v", status.Code(err))
		return err
	})
	store.budget = 1 * time.Second
	api.rejectCalls(math.MaxInt)
	_, err := store.Update(t.Context(), completedUpdate())
	t.Logf("final=%v", err)
}
