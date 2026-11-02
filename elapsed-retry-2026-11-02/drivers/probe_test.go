package taskstore

// Scratch adversarial probes for kagent-dev/kagent#3391. Loaded with -overlay;
// never committed. Each probe logs PROBE lines, so the same file documents the
// PR head and the patched retry loop side by side.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
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

// The message AgentGateway returned in the deployed run when the controller
// Service had no endpoints, over a connection that stayed up.
const proxyRefused = "upstream call failed: Connect: Connection refused (os error 111)"

// probeServer stands in for the egress proxy plus controller: while down is
// set it answers every call with the proxy's Unavailable, as AgentGateway does.
type probeServer struct {
	runtimev1.UnimplementedTaskStoreServiceServer
	downUntil atomic.Int64 // unix nanos; forever when math.MaxInt64
	mu        sync.Mutex
	updates   []*runtimev1.TaskStoreServiceUpdateTaskRequest
	okUpdates int
	settles   int
}

func (s *probeServer) down() bool { return time.Now().UnixNano() < s.downUntil.Load() }

func (s *probeServer) UpdateTask(_ context.Context, request *runtimev1.TaskStoreServiceUpdateTaskRequest) (*runtimev1.TaskStoreServiceUpdateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, proto.Clone(request).(*runtimev1.TaskStoreServiceUpdateTaskRequest))
	if s.down() {
		return nil, status.Error(codes.Unavailable, proxyRefused)
	}
	s.okUpdates++
	return &runtimev1.TaskStoreServiceUpdateTaskResponse{Version: request.ExpectedVersion + 1}, nil
}

func (s *probeServer) SettleTask(context.Context, *runtimev1.TaskStoreServiceSettleTaskRequest) (*runtimev1.TaskStoreServiceSettleTaskResponse, error) {
	if s.down() {
		return nil, status.Error(codes.Unavailable, proxyRefused)
	}
	s.mu.Lock()
	s.settles++
	s.mu.Unlock()
	return &runtimev1.TaskStoreServiceSettleTaskResponse{}, nil
}

type probeEnv struct {
	server   *probeServer
	store    *Store
	dialable *atomic.Bool
	attempts atomic.Int32
	ready    atomic.Int32 // attempts that started on a READY channel
	logs     *bytes.Buffer
	logMu    *sync.Mutex
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	env := &probeEnv{server: &probeServer{}, dialable: &atomic.Bool{}, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	env.dialable.Store(true)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	runtimev1.RegisterTaskStoreServiceServer(server, env.server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := controllerclient.New(controllerclient.Config{APIURL: "http://controller.test", DialOptions: []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			if !env.dialable.Load() {
				return nil, errors.New("proxy pod unreachable")
			}
			return listener.DialContext(ctx)
		}),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if strings.Contains(method, "TaskStoreService/") {
				env.attempts.Add(1)
				if cc.GetState() == connectivity.Ready {
					env.ready.Add(1)
				}
			}
			return invoke(ctx, method, req, reply, cc, opts...)
		}),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	identity := filepath.Join(t.TempDir(), "name")
	require.NoError(t, os.WriteFile(identity, []byte("session-"+uuid.NewString()), 0o600))
	env.store = New(client, identity)
	// Warm the channel so it is READY before the outage, as a long-lived runtime's is.
	env.server.downUntil.Store(0)
	_, err = env.store.Update(env.ctx(t.Context()), env.update())
	require.NoError(t, err)
	env.attempts.Store(0)
	env.ready.Store(0)
	env.server.mu.Lock()
	env.server.updates, env.server.okUpdates = nil, 0
	env.server.mu.Unlock()
	return env
}

func (env *probeEnv) ctx(parent context.Context) context.Context {
	return logging.IntoContext(parent, slog.New(slog.NewTextHandler(lockedWriter{env.logMu, env.logs}, nil)))
}

func (env *probeEnv) logText() string {
	env.logMu.Lock()
	defer env.logMu.Unlock()
	return strings.TrimSpace(env.logs.String())
}

func (env *probeEnv) update() *sdktaskstore.UpdateRequest {
	task := &a2a.Task{ID: "probe-task", ContextID: "probe-context", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	return &sdktaskstore.UpdateRequest{Task: task, PrevVersion: 1, Event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)}
}

func (env *probeEnv) identical(t *testing.T) bool {
	env.server.mu.Lock()
	defer env.server.mu.Unlock()
	for _, request := range env.server.updates[1:] {
		if !proto.Equal(env.server.updates[0], request) {
			return false
		}
	}
	return len(env.server.updates) > 0
}

// The deployed failure: the runtime's connection to the proxy stays READY while
// the controller is down, so every attempt fails at once with Unavailable.
func TestProbeReadyProxyOutageThenRecovery(t *testing.T) {
	env := newProbeEnv(t)
	env.server.downUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	started := time.Now()
	version, err := env.store.Update(env.ctx(t.Context()), env.update())
	elapsed := time.Since(started)
	t.Logf("PROBE case=ready_proxy_2s_outage err=%v version=%d elapsed=%s attempts=%d ready_attempts=%d identical_requests=%t log=%q",
		err, version, elapsed.Round(time.Millisecond), env.attempts.Load(), env.ready.Load(), env.identical(t), env.logText())
	require.NoError(t, err, "a save must outlast a 2s controller outage behind a healthy proxy")
	require.Equal(t, sdktaskstore.TaskVersion(2), version)
	require.True(t, env.identical(t), "every attempt must replay the same save")
}

// Settlement reaches the controller through the same proxy.
func TestProbeSettlementThroughReadyProxyOutage(t *testing.T) {
	env := newProbeEnv(t)
	wrapper := env.store.WrapExecutor(a2asrv.AgentExecutorFunc(nil), "", nil)
	state := &execution{ready: make(chan struct{})}
	state.boundary.Store(2)
	ctx := context.WithValue(env.ctx(t.Context()), executionKey{}, state)
	wrapper.track(ctx, "probe-task")
	env.server.downUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	started := time.Now()
	wrapper.Cleanup(ctx, &a2asrv.ExecutorContext{TaskID: "probe-task"}, nil, nil)
	env.server.mu.Lock()
	settles := env.server.settles
	env.server.mu.Unlock()
	t.Logf("PROBE case=settle_ready_proxy_2s_outage settled=%d elapsed=%s attempts=%d log=%q", settles, time.Since(started).Round(time.Millisecond), env.attempts.Load(), env.logText())
	require.Equal(t, 1, settles, "settlement must land once the controller returns")
}

// A permanent outage behind a healthy proxy: how long, how many attempts, and
// what error and log the caller gets.
func TestProbeReadyProxyPermanentOutage(t *testing.T) {
	env := newProbeEnv(t)
	env.server.downUntil.Store(time.Now().Add(time.Hour).UnixNano())
	started := time.Now()
	_, err := env.store.Update(env.ctx(t.Context()), env.update())
	elapsed := time.Since(started)
	t.Logf("PROBE case=ready_proxy_permanent elapsed=%s attempts=%d err=%q code=%s is_ctx_deadline=%t log=%q",
		elapsed.Round(time.Millisecond), env.attempts.Load(), err, status.Code(err), errors.Is(err, context.DeadlineExceeded), env.logText())
	require.Error(t, err)
}

// The caller gives up during a retry wait. That is not retry exhaustion.
func TestProbeCallerCancelDuringOutage(t *testing.T) {
	env := newProbeEnv(t)
	env.server.downUntil.Store(time.Now().Add(time.Hour).UnixNano())
	ctx, cancel := context.WithCancel(env.ctx(t.Context()))
	time.AfterFunc(250*time.Millisecond, cancel)
	started := time.Now()
	_, err := env.store.Update(ctx, env.update())
	t.Logf("PROBE case=caller_cancel elapsed=%s attempts=%d err=%q logged_exhausted=%t log=%q",
		time.Since(started).Round(time.Millisecond), env.attempts.Load(), err, strings.Contains(env.logText(), "retries exhausted"), env.logText())
	require.ErrorIs(t, err, context.Canceled)
}

// The caller's own deadline ends the operation before any retry budget.
func TestProbeCallerDeadlineDuringOutage(t *testing.T) {
	env := newProbeEnv(t)
	env.server.downUntil.Store(time.Now().Add(time.Hour).UnixNano())
	ctx, cancel := context.WithTimeout(env.ctx(t.Context()), 1500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := env.store.Update(ctx, env.update())
	t.Logf("PROBE case=caller_deadline_1500ms elapsed=%s attempts=%d err=%q logged_exhausted=%t log=%q",
		time.Since(started).Round(time.Millisecond), env.attempts.Load(), err, strings.Contains(env.logText(), "retries exhausted"), env.logText())
	require.Error(t, err)
}

// The proxy itself is unreachable for 2s (its Pod restarts), then the
// controller behind it is still down for 2s more: both halves of a restart.
func TestProbeProxyRestartThenControllerOutage(t *testing.T) {
	env := newProbeEnv(t)
	env.server.downUntil.Store(time.Now().Add(4 * time.Second).UnixNano())
	env.dialable.Store(false)
	// Break the current connection, as a proxy restart does.
	env.store.client.Close()
	t.Log("PROBE note=closing the client forces a fresh channel below")
	env2 := newProbeEnvFromServer(t, env)
	env2.dialable.Store(false)
	time.AfterFunc(2*time.Second, func() { env2.dialable.Store(true) })
	started := time.Now()
	version, err := env2.store.Update(env2.ctx(t.Context()), env2.update())
	t.Logf("PROBE case=proxy_restart_2s_then_controller_down_2s err=%v version=%d elapsed=%s attempts=%d ready_attempts=%d log=%q",
		err, version, time.Since(started).Round(time.Millisecond), env2.attempts.Load(), env2.ready.Load(), env2.logText())
	require.NoError(t, err)
}

// newProbeEnvFromServer builds a fresh client against a new listener that
// serves the same probeServer state.
func newProbeEnvFromServer(t *testing.T, base *probeEnv) *probeEnv {
	t.Helper()
	env := &probeEnv{server: base.server, dialable: &atomic.Bool{}, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	runtimev1.RegisterTaskStoreServiceServer(server, base.server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := controllerclient.New(controllerclient.Config{APIURL: "http://controller.test", DialOptions: []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			if !env.dialable.Load() {
				return nil, errors.New("proxy pod unreachable")
			}
			return listener.DialContext(ctx)
		}),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if strings.Contains(method, "TaskStoreService/") {
				env.attempts.Add(1)
				if cc.GetState() == connectivity.Ready {
					env.ready.Add(1)
				}
			}
			return invoke(ctx, method, req, reply, cc, opts...)
		}),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	identity := filepath.Join(t.TempDir(), "name")
	require.NoError(t, os.WriteFile(identity, []byte("session-"+uuid.NewString()), 0o600))
	env.store = New(client, identity)
	return env
}
