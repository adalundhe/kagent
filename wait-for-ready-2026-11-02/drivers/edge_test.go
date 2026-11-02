package taskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	sdktaskstore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

type evidenceServer struct {
	runtimev1.UnimplementedTaskStoreServiceServer
	get    func(context.Context, *runtimev1.TaskStoreServiceGetTaskRequest) (*runtimev1.TaskStoreServiceGetTaskResponse, error)
	update func(context.Context, *runtimev1.TaskStoreServiceUpdateTaskRequest) (*runtimev1.TaskStoreServiceUpdateTaskResponse, error)
}

func (s *evidenceServer) GetTask(ctx context.Context, req *runtimev1.TaskStoreServiceGetTaskRequest) (*runtimev1.TaskStoreServiceGetTaskResponse, error) {
	return s.get(ctx, req)
}

func (s *evidenceServer) UpdateTask(ctx context.Context, req *runtimev1.TaskStoreServiceUpdateTaskRequest) (*runtimev1.TaskStoreServiceUpdateTaskResponse, error) {
	return s.update(ctx, req)
}

func evidenceStartServer(t *testing.T, listener *atomic.Pointer[bufconn.Listener], api *evidenceServer) func() {
	t.Helper()
	serving := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	runtimev1.RegisterTaskStoreServiceServer(server, api)
	listener.Store(serving)
	go func() { _ = server.Serve(serving) }()
	stop := sync.OnceFunc(server.Stop)
	t.Cleanup(stop)
	return stop
}

func evidenceNewStore(t *testing.T, listener *atomic.Pointer[bufconn.Listener], interceptor grpc.UnaryClientInterceptor) *Store {
	t.Helper()
	options := []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.Load().DialContext(ctx)
	})}
	if interceptor != nil {
		options = append(options, grpc.WithUnaryInterceptor(interceptor))
	}
	client, err := controllerclient.New(controllerclient.Config{APIURL: "http://controller.test", DialOptions: options})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	identity := filepath.Join(t.TempDir(), "name")
	require.NoError(t, os.WriteFile(identity, []byte("session-"+uuid.NewString()), 0o600))
	return New(client, identity)
}

func TestEvidenceReadyChannelBackendUnavailable(t *testing.T) {
	var listener atomic.Pointer[bufconn.Listener]
	var blockedUntil atomic.Int64
	var calls, readyCalls atomic.Int32
	wire, err := pbconv.ToProtoTask(&a2a.Task{ID: "synthetic-task", ContextID: "synthetic-conversation", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})
	require.NoError(t, err)
	evidenceStartServer(t, &listener, &evidenceServer{get: func(context.Context, *runtimev1.TaskStoreServiceGetTaskRequest) (*runtimev1.TaskStoreServiceGetTaskResponse, error) {
		calls.Add(1)
		if time.Now().UnixNano() < blockedUntil.Load() {
			return nil, status.Error(codes.Unavailable, "synthetic backend unavailable while channel stays ready")
		}
		return &runtimev1.TaskStoreServiceGetTaskResponse{Stored: &runtimev1.StoredTask{Task: wire, Version: 1}}, nil
	}})
	store := evidenceNewStore(t, &listener, func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if cc.GetState() == connectivity.Ready {
			readyCalls.Add(1)
		}
		return invoke(ctx, method, req, reply, cc, opts...)
	})
	// Use the RPC directly for warmup; the edge concerns transport status rather
	// than decoding a successful stored task.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rpcCtx, rpcCancel := store.callContext(ctx)
	_, err = store.client.TaskStoreService().GetTask(rpcCtx, &runtimev1.TaskStoreServiceGetTaskRequest{}, grpc.WaitForReady(true))
	rpcCancel()
	require.NoError(t, err)
	beforeCalls, beforeReady := calls.Load(), readyCalls.Load()
	blockedUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	var logs bytes.Buffer
	ctx = logging.IntoContext(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
	started := time.Now()
	_, err = store.Get(ctx, "synthetic-task")
	elapsed := time.Since(started)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, int32(4), calls.Load()-beforeCalls)
	require.Equal(t, int32(4), readyCalls.Load()-beforeReady)
	require.Less(t, elapsed, 2*time.Second)
	var exhausted struct {
		Message  string `json:"msg"`
		Attempts int    `json:"attempts"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &exhausted))
	require.Equal(t, "taskstore retries exhausted", exhausted.Message)
	require.Equal(t, 4, exhausted.Attempts)
	t.Logf("EVIDENCE case=ready_backend_unavailable configured_error_window=2s attempts=4 ready_attempts=4 elapsed=%s final_code=%s exhaustion_log=%s", elapsed, status.Code(err), bytes.TrimSpace(logs.Bytes()))
	if wait := time.Until(time.Unix(0, blockedUntil.Load())); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			t.Fatal("backend recovery wait expired")
		}
	}
	stored, err := store.Get(ctx, "synthetic-task")
	require.NoError(t, err)
	require.Equal(t, sdktaskstore.TaskVersion(1), stored.Version)
	t.Logf("EVIDENCE case=ready_backend_recovered elapsed=%s version=1", time.Since(started))
}

func TestEvidenceInFlightTransportLossReplaysRequest(t *testing.T) {
	var listener atomic.Pointer[bufconn.Listener]
	var mu sync.Mutex
	var requests []*runtimev1.TaskStoreServiceUpdateTaskRequest
	entered := make(chan struct{}, 1)
	first := &evidenceServer{update: func(ctx context.Context, req *runtimev1.TaskStoreServiceUpdateTaskRequest) (*runtimev1.TaskStoreServiceUpdateTaskResponse, error) {
		mu.Lock()
		requests = append(requests, proto.Clone(req).(*runtimev1.TaskStoreServiceUpdateTaskRequest))
		mu.Unlock()
		entered <- struct{}{}
		<-ctx.Done()
		return nil, status.Error(codes.Unavailable, "synthetic controller stop during RPC")
	}}
	stop := evidenceStartServer(t, &listener, first)
	store := evidenceNewStore(t, &listener, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	task := &a2a.Task{ID: "synthetic-task", ContextID: "synthetic-conversation", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	type result struct {
		version sdktaskstore.TaskVersion
		err     error
	}
	done := make(chan result, 1)
	started := time.Now()
	go func() {
		version, err := store.Update(ctx, &sdktaskstore.UpdateRequest{Task: task, PrevVersion: 1, Event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)})
		done <- result{version, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("initial RPC did not enter the server")
	}
	stop()
	downAt := time.Now()
	select {
	case outcome := <-done:
		t.Fatalf("in-flight update returned during outage: %v", outcome.err)
	case <-time.After(2 * time.Second):
	}
	evidenceStartServer(t, &listener, &evidenceServer{update: func(ctx context.Context, req *runtimev1.TaskStoreServiceUpdateTaskRequest) (*runtimev1.TaskStoreServiceUpdateTaskResponse, error) {
		mu.Lock()
		requests = append(requests, proto.Clone(req).(*runtimev1.TaskStoreServiceUpdateTaskRequest))
		mu.Unlock()
		return &runtimev1.TaskStoreServiceUpdateTaskResponse{Version: 2}, nil
	}})
	downtime := time.Since(downAt)
	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, sdktaskstore.TaskVersion(2), outcome.version)
	case <-ctx.Done():
		t.Fatal("update did not recover")
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 2)
	require.True(t, proto.Equal(requests[0], requests[1]))
	t.Logf("EVIDENCE case=in_flight_transport_loss downtime=%s total=%s server_requests=%d identical_request=true acknowledged_version=2", downtime, time.Since(started), len(requests))
}
