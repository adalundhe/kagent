package taskstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEvidenceUnavailableTransportBoundaries(t *testing.T) {
	for _, mode := range []string{"caller-cancel", "caller-deadline", "exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var available atomic.Bool
			var attempts atomic.Int32
			var cancelledAt atomic.Int64
			var parentDeadline time.Time
			store := newReadinessStore(t, &readinessServer{}, &available,
				func(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
					n := attempts.Add(1)
					start := time.Now()
					if mode != "caller-cancel" {
						deadline, ok := ctx.Deadline()
						require.True(t, ok)
						t.Logf("EVIDENCE configured_deadline mode=%s attempt=%d remaining_seconds=%.6f", mode, n, deadline.Sub(start).Seconds())
						if mode == "caller-deadline" {
							require.True(t, deadline.Equal(parentDeadline), "the RPC must retain the earlier caller deadline")
						} else {
							require.LessOrEqual(t, deadline.Sub(start), 30*time.Second)
						}
					}
					err := invoke(ctx, method, req, reply, conn, options...)
					t.Logf("EVIDENCE boundary_rpc mode=%s attempt=%d elapsed_seconds=%.6f code=%s", mode, n, time.Since(start).Seconds(), status.Code(err))
					return err
				})
			ctx := t.Context()
			if mode == "caller-cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				time.AfterFunc(250*time.Millisecond, func() {
					cancelledAt.Store(time.Now().UnixNano())
					cancel()
				})
			}
			if mode == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer cancel()
				parentDeadline, _ = ctx.Deadline()
			}
			started := time.Now()
			_, err := store.Get(ctx, "task")
			elapsed := time.Since(started)
			t.Logf("EVIDENCE boundary_result mode=%s elapsed_seconds=%.6f attempts=%d error=%q", mode, elapsed.Seconds(), attempts.Load(), err)
			require.Error(t, err)
			if mode == "caller-cancel" {
				require.Equal(t, codes.Canceled, status.Code(err))
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.EqualValues(t, 1, attempts.Load())
				t.Logf("EVIDENCE cancellation_callback_after_seconds=%.6f return_after_callback_seconds=%.6f", time.Unix(0, cancelledAt.Load()).Sub(started).Seconds(), time.Since(time.Unix(0, cancelledAt.Load())).Seconds())
			} else if mode == "caller-deadline" {
				require.True(t, errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded)
				require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
				require.EqualValues(t, 1, attempts.Load())
			} else {
				require.Equal(t, codes.DeadlineExceeded, status.Code(err))
				require.EqualValues(t, 4, attempts.Load())
				// Observe completion time without assuming real-time scheduling.
				// RPC deadlines and the test timeout still bound the experiment.
				require.GreaterOrEqual(t, elapsed.Seconds(), 120.0)
			}
		})
	}
}
