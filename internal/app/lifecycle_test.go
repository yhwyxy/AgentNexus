package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/app"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

const testTimeout = 5 * time.Second

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRegistry 记录调用并返回可注入的错误；用于隔离 Lifecycle 自身的编排行为。
// 调用记录会被后台 sync worker 与测试 goroutine 同时访问，因此加锁
// （否则 -race 下会出现测试自身的竞态）。
type fakeRegistry struct {
	mu   sync.Mutex
	srv  server.Server
	list []server.Server
	err  error
	// writeErr 只影响 Update/SetEnabled/SetDesiredState，用来区分「读失败」与「写失败」。
	writeErr error
	listErr  error
	// writes 只统计真正发生的状态写入，幂等调用不计数（与 server.Service 一致）。
	writes int

	inputs       []server.RegisterInput
	gets         []server.ID
	lists        int
	updates      []updateCall
	enabledCalls []bool
	desiredCalls []server.DesiredState
}

func (f *fakeRegistry) Register(_ context.Context, in server.RegisterInput) (server.Server, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()

	if f.err != nil {
		return server.Server{}, f.err
	}
	return f.srv, nil
}

func (f *fakeRegistry) Get(_ context.Context, id server.ID) (server.Server, error) {
	f.mu.Lock()
	f.gets = append(f.gets, id)
	f.mu.Unlock()

	if f.err != nil {
		return server.Server{}, f.err
	}
	return f.srv, nil
}

// registeredInputs / fetchedIDs 返回调用记录的副本，避免测试读到正在追加的切片。
func (f *fakeRegistry) registeredInputs() []server.RegisterInput {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]server.RegisterInput(nil), f.inputs...)
}

func (f *fakeRegistry) fetchedIDs() []server.ID {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]server.ID(nil), f.gets...)
}

// List 返回注入的列表视图；未注入时退化为单元素视图，便于断言纯委托。
func (f *fakeRegistry) List(context.Context) ([]server.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list != nil {
		return append([]server.Server(nil), f.list...), nil
	}
	return []server.Server{f.srv}, nil
}

// Update 记录乐观锁参数并递增 Revision——与 server.Service.Update 的可见效果一致。
func (f *fakeRegistry) Update(_ context.Context, id server.ID, expectedRevision int64, in server.UpdateInput) (server.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updates = append(f.updates, updateCall{id: id, expectedRevision: expectedRevision, input: in})
	if f.writeErr != nil {
		return server.Server{}, f.writeErr
	}
	f.srv.ID = id
	f.srv.DisplayName = in.DisplayName
	f.srv.Revision++
	f.writes++

	return f.srv, nil
}

// SetEnabled 复刻 server.Service.setState 的幂等语义：已是目标值时跳过写入并返回当前快照。
func (f *fakeRegistry) SetEnabled(_ context.Context, id server.ID, enabled bool) (server.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.enabledCalls = append(f.enabledCalls, enabled)
	if f.writeErr != nil {
		return server.Server{}, f.writeErr
	}
	f.srv.ID = id
	if f.srv.Enabled != enabled {
		f.srv.Enabled = enabled
		f.writes++
	}

	return f.srv, nil
}

// SetDesiredState 同样复刻幂等语义：值未变化时不写库。
func (f *fakeRegistry) SetDesiredState(_ context.Context, id server.ID, state server.DesiredState) (server.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.desiredCalls = append(f.desiredCalls, state)
	if f.writeErr != nil {
		return server.Server{}, f.writeErr
	}
	f.srv.ID = id
	if f.srv.Spec.DesiredState != state {
		f.srv.Spec.DesiredState = state
		f.writes++
	}

	return f.srv, nil
}

// setPhase 模拟运行态收敛写回观察状态，用于断言「停止后必须重新读取快照」。
func (f *fakeRegistry) setPhase(phase server.Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.srv.Status.Phase = phase
}

func (f *fakeRegistry) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.writes
}

func (f *fakeRegistry) updateCalls() []updateCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]updateCall(nil), f.updates...)
}

func (f *fakeRegistry) desiredStates() []server.DesiredState {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]server.DesiredState(nil), f.desiredCalls...)
}

func (f *fakeRegistry) enabledValues() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]bool(nil), f.enabledCalls...)
}

// updateCall 是一次 Update 的完整参数，断言乐观锁透传用。
type updateCall struct {
	id               server.ID
	expectedRevision int64
	input            server.UpdateInput
}

// fakeConverger 记录同步收敛调用。Reconcile/Stop 的次数与顺序就是
// 「同步回收 vs 排队重建」的判据；hook 用来模拟收敛写回的 phase。
type fakeConverger struct {
	err         error
	onReconcile func(server.ID)
	onStop      func(server.ID)

	mu         sync.Mutex
	reconciles []server.ID
	stops      []server.ID
}

func (f *fakeConverger) Reconcile(_ context.Context, id server.ID) error {
	f.mu.Lock()
	f.reconciles = append(f.reconciles, id)
	f.mu.Unlock()

	if f.onReconcile != nil {
		f.onReconcile(id)
	}
	return f.err
}

func (f *fakeConverger) Stop(_ context.Context, id server.ID) error {
	f.mu.Lock()
	f.stops = append(f.stops, id)
	f.mu.Unlock()

	if f.onStop != nil {
		f.onStop(id)
	}
	return f.err
}

func (f *fakeConverger) reconcileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.reconciles)
}

func (f *fakeConverger) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.stops)
}

// fakeCatalog 统计缓存失效次数：可见性变化与队列收敛是两个独立后果，必须分别断言。
type fakeCatalog struct {
	mu            sync.Mutex
	invalidations int
}

func (f *fakeCatalog) Invalidate() {
	f.mu.Lock()
	f.invalidations++
	f.mu.Unlock()
}

func (f *fakeCatalog) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.invalidations
}

// fakeSyncer 可阻塞、可按 Server 注入失败，并记录每次被消费的 ID。
type fakeSyncer struct {
	block    chan struct{}
	started  chan server.ID
	canceled chan server.ID
	fail     map[server.ID]error

	mu    sync.Mutex
	calls []server.ID
}

func (f *fakeSyncer) Sync(ctx context.Context, id server.ID) (tool.SyncResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	f.mu.Unlock()

	if f.started != nil {
		f.started <- id
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			if f.canceled != nil {
				f.canceled <- id
			}
			return tool.SyncResult{}, ctx.Err()
		}
	}
	if err := f.fail[id]; err != nil {
		return tool.SyncResult{}, err
	}
	return tool.SyncResult{
		Snapshot: tool.Snapshot{
			ID: "snap-" + string(id), ServerID: id, Generation: 1, ToolCount: 2, State: tool.SnapshotActive,
		},
		Changed: true,
	}, nil
}

func (f *fakeSyncer) recorded() []server.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]server.ID(nil), f.calls...)
}

func waitForID(t *testing.T, ch chan server.ID, want server.ID) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("sync triggered for %q, want %q", got, want)
		}
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for sync of %q", want)
	}
}

func waitForCalls(t *testing.T, syncer *fakeSyncer, want int) []server.ID {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if calls := syncer.recorded(); len(calls) >= want {
			return calls
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d sync calls, got %v", want, syncer.recorded())
	return nil
}

func newTestLifecycle(t *testing.T, syncer *fakeSyncer, registry *fakeRegistry, opts ...func(*app.LifecycleOptions)) *app.Lifecycle {
	t.Helper()
	options := app.LifecycleOptions{
		Registry: registry, Syncer: syncer, Auditor: &recordingAuditor{}, Logger: testLogger(),
		Runtime: &fakeConverger{}, Catalog: &fakeCatalog{},
	}
	for _, apply := range opts {
		apply(&options)
	}
	lifecycle, err := app.NewLifecycle(options)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}
	return lifecycle
}

// withConverger / withCatalog 让单个测试拿到默认 fakes 的句柄做断言。
func withConverger(converger app.RuntimeConverger) func(*app.LifecycleOptions) {
	return func(opts *app.LifecycleOptions) { opts.Runtime = converger }
}

func withCatalog(catalog app.CatalogInvalidator) func(*app.LifecycleOptions) {
	return func(opts *app.LifecycleOptions) { opts.Catalog = catalog }
}

// settleQueue 给后台队列一个确定的窗口，用于断言「没有发生排队收敛」。
func settleQueue() { time.Sleep(20 * time.Millisecond) }

func closeLifecycle(t *testing.T, lifecycle *app.Lifecycle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := lifecycle.Close(ctx); err != nil {
		t.Fatalf("close lifecycle: %v", err)
	}
}

func TestLifecycleSyncsAfterRegister(t *testing.T) {
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1", Name: "weather"}}
	syncer := &fakeSyncer{started: make(chan server.ID, 1)}
	lifecycle := newTestLifecycle(t, syncer, registry)
	defer closeLifecycle(t, lifecycle)

	created, err := lifecycle.Register(context.Background(), server.RegisterInput{Name: "weather"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if created.ID != "srv-1" {
		t.Fatalf("register returned %#v", created)
	}
	inputs := registry.registeredInputs()
	if len(inputs) != 1 || inputs[0].Name != "weather" {
		t.Fatalf("registry inputs = %#v", inputs)
	}
	waitForID(t, syncer.started, "srv-1")

	got, err := lifecycle.Get(context.Background(), "srv-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// gets 可能多于一次：同步结束后的审计事件会再读一次资产名（见 Lifecycle.assetName）。
	gets := registry.fetchedIDs()
	if got.ID != "srv-1" || len(gets) == 0 || gets[0] != "srv-1" {
		t.Fatalf("get delegated to %v returning %#v", gets, got)
	}
}

func TestLifecycleDoesNotSyncWhenRegisterFails(t *testing.T) {
	registry := &fakeRegistry{err: server.ErrAlreadyExists}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, registry)

	if _, err := lifecycle.Register(context.Background(), server.RegisterInput{Name: "weather"}); !errors.Is(err, server.ErrAlreadyExists) {
		t.Fatalf("register error = %v, want ErrAlreadyExists", err)
	}
	// Close 会等待 worker 退出，因此此刻断言"没有同步"是确定的。
	closeLifecycle(t, lifecycle)
	if calls := syncer.recorded(); len(calls) != 0 {
		t.Fatalf("sync calls after failed register = %v, want none", calls)
	}
}

func TestLifecycleDeduplicatesQueuedTriggers(t *testing.T) {
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1"}}
	syncer := &fakeSyncer{started: make(chan server.ID, 8), block: make(chan struct{})}
	lifecycle := newTestLifecycle(t, syncer, registry)
	defer closeLifecycle(t, lifecycle)

	lifecycle.Trigger("srv-1")
	waitForID(t, syncer.started, "srv-1") // worker 已阻塞在 srv-1 上

	lifecycle.Trigger("srv-2")
	lifecycle.Trigger("srv-2")
	close(syncer.block)
	lifecycle.Trigger("srv-3")

	calls := waitForCalls(t, syncer, 3)
	want := []server.ID{"srv-1", "srv-2", "srv-3"}
	for i, id := range want {
		if calls[i] != id {
			t.Fatalf("sync calls = %v, want %v", calls, want)
		}
	}
}

func TestLifecycleCloseCancelsInFlightSyncAndStopsQueue(t *testing.T) {
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1"}}
	syncer := &fakeSyncer{
		started:  make(chan server.ID, 1),
		canceled: make(chan server.ID, 1),
		block:    make(chan struct{}),
	}
	lifecycle := newTestLifecycle(t, syncer, registry)

	lifecycle.Trigger("srv-1")
	waitForID(t, syncer.started, "srv-1")

	closeLifecycle(t, lifecycle)
	select {
	case id := <-syncer.canceled:
		if id != "srv-1" {
			t.Fatalf("canceled sync = %q, want srv-1", id)
		}
	case <-time.After(testTimeout):
		t.Fatal("in-flight sync was not canceled by Close")
	}

	lifecycle.Trigger("srv-2")
	closeLifecycle(t, lifecycle) // 重复 Close 幂等
	if calls := syncer.recorded(); len(calls) != 1 {
		t.Fatalf("sync calls after Close = %v, want only the canceled one", calls)
	}
}

func TestLifecycleContinuesAfterSyncFailure(t *testing.T) {
	registry := &fakeRegistry{srv: server.Server{ID: "srv-1"}}
	syncer := &fakeSyncer{fail: map[server.ID]error{"srv-1": errors.New("backend unreachable")}}
	lifecycle := newTestLifecycle(t, syncer, registry)
	defer closeLifecycle(t, lifecycle)

	lifecycle.Trigger("srv-1")
	lifecycle.Trigger("srv-2")

	calls := waitForCalls(t, syncer, 2)
	if calls[0] != "srv-1" || calls[1] != "srv-2" {
		t.Fatalf("sync calls = %v, want failed srv-1 followed by srv-2", calls)
	}
}

// 管理方法的失败不产生任何副作用：不失效缓存、不排队、不做同步收敛。
func TestLifecycleManagementMethodsPropagateRegistryErrors(t *testing.T) {
	sentinel := errors.New("registry exploded")

	tests := []struct {
		name string
		call func(context.Context, *app.Lifecycle) error
	}{
		{"List", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.List(ctx)
			return err
		}},
		{"Update", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.Update(ctx, "srv-1", 1, server.UpdateInput{DisplayName: "weather"})
			return err
		}},
		{"SetEnabled", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.SetEnabled(ctx, "srv-1", false)
			return err
		}},
		{"Start", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.Start(ctx, "srv-1")
			return err
		}},
		{"Stop", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.Stop(ctx, "srv-1")
			return err
		}},
		{"Restart", func(ctx context.Context, l *app.Lifecycle) error {
			_, err := l.Restart(ctx, "srv-1")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &fakeRegistry{
				srv:      runnableServer("srv-1", server.PhaseReady, 1, 1, 0),
				err:      sentinel,
				writeErr: sentinel,
				listErr:  sentinel,
			}
			syncer := &fakeSyncer{}
			converger := &fakeConverger{}
			catalog := &fakeCatalog{}
			lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
			defer closeLifecycle(t, lifecycle)

			if err := tt.call(context.Background(), lifecycle); !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want the registry error unchanged", err)
			}
			if got := catalog.count(); got != 0 {
				t.Fatalf("catalog invalidations = %d, want 0 on a failed write", got)
			}
			if got := converger.reconcileCount() + converger.stopCount(); got != 0 {
				t.Fatalf("converger calls = %d, want 0 on a failed write", got)
			}
			settleQueue()
			if calls := syncer.recorded(); len(calls) != 0 {
				t.Fatalf("sync calls = %v, want none on a failed write", calls)
			}
		})
	}
}

func TestLifecycleListDelegatesToRegistry(t *testing.T) {
	want := []server.Server{{ID: "srv-1"}, {ID: "srv-2"}}
	registry := &fakeRegistry{list: want}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, registry)
	defer closeLifecycle(t, lifecycle)

	got, err := lifecycle.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(want) || got[0].ID != want[0].ID || got[1].ID != want[1].ID {
		t.Fatalf("list = %#v, want %#v", got, want)
	}
}

func TestLifecycleUpdateInvalidatesCatalogAndQueuesSync(t *testing.T) {
	registry := &fakeRegistry{srv: runnableServer("srv-1", server.PhaseReady, 1, 1, 0)}
	syncer := &fakeSyncer{}
	converger := &fakeConverger{}
	catalog := &fakeCatalog{}
	lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
	defer closeLifecycle(t, lifecycle)

	in := server.UpdateInput{DisplayName: "Weather v2"}
	srv, err := lifecycle.Update(context.Background(), "srv-1", 1, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if srv.Revision != 2 {
		t.Fatalf("revision = %d, want 2 after update", srv.Revision)
	}
	updates := registry.updateCalls()
	if len(updates) != 1 || updates[0].id != "srv-1" || updates[0].expectedRevision != 1 {
		t.Fatalf("update calls = %#v, want one call with expectedRevision 1", updates)
	}
	if got := catalog.count(); got != 1 {
		t.Fatalf("catalog invalidations = %d, want exactly 1", got)
	}
	if got := converger.reconcileCount() + converger.stopCount(); got != 0 {
		t.Fatalf("converger calls = %d, want 0 (update converges through the queue)", got)
	}
	if calls := waitForCalls(t, syncer, 1); calls[0] != "srv-1" {
		t.Fatalf("sync calls = %v, want srv-1", calls)
	}
}

// SetEnabled：启用走排队收敛，停用走同步回收并返回收敛后的快照。
func TestLifecycleSetEnabledRoutesConvergence(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		srv           server.Server
		wantWrites    int
		wantReconcile int
		wantSync      bool
		wantPhase     server.Phase
	}{
		{
			name:       "enable a runnable server queues convergence",
			enabled:    true,
			srv:        server.Server{ID: "srv-1", Enabled: false, Spec: server.Spec{DesiredState: server.DesiredRunning}, Status: server.Status{Phase: server.PhaseStopped}},
			wantWrites: 1, wantSync: true, wantPhase: server.PhaseStopped,
		},
		{
			name:       "enable a stopped server only flips the flag",
			enabled:    true,
			srv:        server.Server{ID: "srv-1", Enabled: false, Spec: server.Spec{DesiredState: server.DesiredStopped}, Status: server.Status{Phase: server.PhaseStopped}},
			wantWrites: 1, wantPhase: server.PhaseStopped,
		},
		{
			name:       "disable reconciles synchronously and returns the observed phase",
			enabled:    false,
			srv:        runnableServer("srv-1", server.PhaseReady, 1, 1, 0),
			wantWrites: 1, wantReconcile: 1, wantPhase: server.PhaseStopped,
		},
		{
			name:          "disable is idempotent but still reconciles",
			enabled:       false,
			srv:           server.Server{ID: "srv-1", Enabled: false, Spec: server.Spec{DesiredState: server.DesiredStopped}, Status: server.Status{Phase: server.PhaseStopped}},
			wantReconcile: 1, wantPhase: server.PhaseStopped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &fakeRegistry{srv: tt.srv}
			syncer := &fakeSyncer{}
			converger := &fakeConverger{onReconcile: func(server.ID) { registry.setPhase(server.PhaseStopped) }}
			catalog := &fakeCatalog{}
			lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
			defer closeLifecycle(t, lifecycle)

			srv, err := lifecycle.SetEnabled(context.Background(), "srv-1", tt.enabled)
			if err != nil {
				t.Fatalf("set enabled: %v", err)
			}
			if srv.Enabled != tt.enabled {
				t.Fatalf("enabled = %t, want %t", srv.Enabled, tt.enabled)
			}
			if got := registry.writeCount(); got != tt.wantWrites {
				t.Fatalf("registry writes = %d, want %d", got, tt.wantWrites)
			}
			if got := converger.reconcileCount(); got != tt.wantReconcile {
				t.Fatalf("reconcile calls = %d, want %d", got, tt.wantReconcile)
			}
			if srv.Status.Phase != tt.wantPhase {
				t.Fatalf("returned phase = %q, want %q", srv.Status.Phase, tt.wantPhase)
			}
			if got := catalog.count(); got != 1 {
				t.Fatalf("catalog invalidations = %d, want exactly 1", got)
			}
			if tt.wantSync {
				if calls := waitForCalls(t, syncer, 1); calls[0] != "srv-1" {
					t.Fatalf("sync calls = %v, want srv-1", calls)
				}
				return
			}
			settleQueue()
			if calls := syncer.recorded(); len(calls) != 0 {
				t.Fatalf("sync calls = %v, want none", calls)
			}
		})
	}
}

func TestLifecycleStartRouting(t *testing.T) {
	tests := []struct {
		name       string
		srv        server.Server
		wantErr    error
		wantWrites int
		wantSync   bool
	}{
		{
			name:    "disabled server is rejected without writing",
			srv:     server.Server{ID: "srv-1", Enabled: false, Spec: server.Spec{DesiredState: server.DesiredStopped}},
			wantErr: server.ErrNotRunnable,
		},
		{
			name:       "stopped server is written and queued",
			srv:        server.Server{ID: "srv-1", Enabled: true, Spec: server.Spec{DesiredState: server.DesiredStopped}, Status: server.Status{Phase: server.PhaseStopped}},
			wantWrites: 1,
			wantSync:   true,
		},
		{
			name:     "already running stays idempotent but still retries",
			srv:      runnableServer("srv-1", server.PhaseDegraded, 1, 1, 3),
			wantSync: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &fakeRegistry{srv: tt.srv}
			syncer := &fakeSyncer{}
			converger := &fakeConverger{}
			catalog := &fakeCatalog{}
			lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
			defer closeLifecycle(t, lifecycle)

			srv, err := lifecycle.Start(context.Background(), "srv-1")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("start error = %v, want %v", err, tt.wantErr)
				}
				if got := catalog.count(); got != 0 {
					t.Fatalf("catalog invalidations = %d, want 0 on a rejected start", got)
				}
				settleQueue()
				if calls := syncer.recorded(); len(calls) != 0 {
					t.Fatalf("sync calls = %v, want none on a rejected start", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if srv.Spec.DesiredState != server.DesiredRunning {
				t.Fatalf("desired state = %q, want running", srv.Spec.DesiredState)
			}
			if got := registry.writeCount(); got != tt.wantWrites {
				t.Fatalf("registry writes = %d, want %d", got, tt.wantWrites)
			}
			if got := converger.reconcileCount(); got != 0 {
				t.Fatalf("reconcile calls = %d, want 0 (start converges through the queue)", got)
			}
			if got := catalog.count(); got != 1 {
				t.Fatalf("catalog invalidations = %d, want exactly 1", got)
			}
			if tt.wantSync {
				if calls := waitForCalls(t, syncer, 1); calls[0] != "srv-1" {
					t.Fatalf("sync calls = %v, want srv-1", calls)
				}
			}
		})
	}
}

func TestLifecycleStopReconcilesSynchronously(t *testing.T) {
	registry := &fakeRegistry{srv: runnableServer("srv-1", server.PhaseReady, 1, 1, 0)}
	syncer := &fakeSyncer{}
	converger := &fakeConverger{onReconcile: func(server.ID) { registry.setPhase(server.PhaseStopped) }}
	catalog := &fakeCatalog{}
	lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
	defer closeLifecycle(t, lifecycle)

	srv, err := lifecycle.Stop(context.Background(), "srv-1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if srv.Spec.DesiredState != server.DesiredStopped {
		t.Fatalf("desired state = %q, want stopped", srv.Spec.DesiredState)
	}
	if srv.Status.Phase != server.PhaseStopped {
		t.Fatalf("returned phase = %q, want stopped (stop must re-read after reconcile)", srv.Status.Phase)
	}
	if got := registry.writeCount(); got != 1 {
		t.Fatalf("registry writes = %d, want 1", got)
	}
	if got := converger.reconcileCount(); got != 1 {
		t.Fatalf("reconcile calls = %d, want 1", got)
	}
	if got := catalog.count(); got != 1 {
		t.Fatalf("catalog invalidations = %d, want exactly 1", got)
	}
	settleQueue()
	if calls := syncer.recorded(); len(calls) != 0 {
		t.Fatalf("sync calls = %v, want none (stop is synchronous)", calls)
	}
}

func TestLifecycleRestartKeepsRevision(t *testing.T) {
	tests := []struct {
		name      string
		srv       server.Server
		wantErr   error
		wantStops int
		wantSync  bool
	}{
		{
			name:      "runnable server stops the old instance and queues a rebuild",
			srv:       runnableServer("srv-1", server.PhaseReady, 7, 7, 0),
			wantStops: 1,
			wantSync:  true,
		},
		{
			name:    "non-runnable server is rejected",
			srv:     server.Server{ID: "srv-1", Enabled: true, Spec: server.Spec{DesiredState: server.DesiredStopped}},
			wantErr: server.ErrNotRunnable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &fakeRegistry{srv: tt.srv}
			syncer := &fakeSyncer{}
			converger := &fakeConverger{}
			catalog := &fakeCatalog{}
			lifecycle := newTestLifecycle(t, syncer, registry, withConverger(converger), withCatalog(catalog))
			defer closeLifecycle(t, lifecycle)

			srv, err := lifecycle.Restart(context.Background(), "srv-1")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("restart error = %v, want %v", err, tt.wantErr)
				}
				if got := catalog.count(); got != 0 {
					t.Fatalf("catalog invalidations = %d, want 0 on a rejected restart", got)
				}
				if got := converger.stopCount(); got != 0 {
					t.Fatalf("stop calls = %d, want 0 on a rejected restart", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("restart: %v", err)
			}
			if srv.Revision != tt.srv.Revision {
				t.Fatalf("revision = %d, want %d unchanged by restart", srv.Revision, tt.srv.Revision)
			}
			if got := registry.writeCount(); got != 0 {
				t.Fatalf("registry writes = %d, want 0 (restart changes no config)", got)
			}
			if got := converger.stopCount(); got != tt.wantStops {
				t.Fatalf("stop calls = %d, want %d", got, tt.wantStops)
			}
			if got := converger.reconcileCount(); got != 0 {
				t.Fatalf("reconcile calls = %d, want 0 (restart rebuilds through the queue)", got)
			}
			if got := catalog.count(); got != 1 {
				t.Fatalf("catalog invalidations = %d, want exactly 1", got)
			}
			if tt.wantSync {
				if calls := waitForCalls(t, syncer, 1); calls[0] != "srv-1" {
					t.Fatalf("sync calls = %v, want srv-1", calls)
				}
			}
		})
	}
}
