package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/app"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// fakeLister 提供确定性的 Server 视图；failFirst 让首次巡检的列表调用失败。
type fakeLister struct {
	servers   []server.Server
	failFirst bool
	mu        sync.Mutex
	calls     int
}

func (f *fakeLister) ListEnabled(context.Context) ([]server.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failFirst && f.calls == 1 {
		return nil, errors.New("list failed")
	}
	return append([]server.Server(nil), f.servers...), nil
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func withLister(lister app.ServerLister, interval time.Duration) func(*app.LifecycleOptions) {
	return func(opts *app.LifecycleOptions) {
		opts.Lister = lister
		opts.ReconcileInterval = interval
	}
}

func runnableServer(id server.ID, phase server.Phase, revision, observed int64, failures int) server.Server {
	return server.Server{
		ID:       id,
		Enabled:  true,
		Revision: revision,
		Spec:     server.Spec{DesiredState: server.DesiredRunning},
		Status: server.Status{
			Phase:               phase,
			ObservedRevision:    observed,
			ConsecutiveFailures: failures,
		},
	}
}

func countOf(calls []server.ID, id server.ID) int {
	count := 0
	for _, call := range calls {
		if call == id {
			count++
		}
	}
	return count
}

// 启动重放（§9.2 触发时机 4 / §3.4）：无条件覆盖全部 Runnable 的 Server。
// 运行态实例缓存不落库，重启后 DB 里的 ready 不代表实例存在，因此连 ready 的也要重放。
func TestLifecycleReplaysRunnableServersOnStart(t *testing.T) {
	disabled := runnableServer("srv-disabled", server.PhaseFailed, 1, 0, 3)
	disabled.Enabled = false
	stopped := runnableServer("srv-stopped", server.PhaseStopped, 1, 0, 0)
	stopped.Spec.DesiredState = server.DesiredStopped

	lister := &fakeLister{servers: []server.Server{
		runnableServer("srv-ready", server.PhaseReady, 1, 1, 0),
		runnableServer("srv-degraded", server.PhaseDegraded, 1, 0, 1),
		runnableServer("srv-pending", server.PhasePending, 1, 0, 0),
		disabled,
		stopped,
	}}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 0))
	waitForCalls(t, syncer, 3)
	closeLifecycle(t, lifecycle)

	calls := syncer.recorded()
	if len(calls) != 3 {
		t.Fatalf("replay sync calls = %v, want exactly the three runnable servers", calls)
	}
	for _, id := range []server.ID{"srv-ready", "srv-degraded", "srv-pending"} {
		if countOf(calls, id) != 1 {
			t.Errorf("sync calls for %q = %d, want 1", id, countOf(calls, id))
		}
	}
	if countOf(calls, "srv-disabled") != 0 || countOf(calls, "srv-stopped") != 0 {
		t.Errorf("non-runnable servers were synced: %v", calls)
	}
}

// 周期巡检只挑未收敛的 Server：已 ready 且 observedRevision 跟上的不再重复 tools/list。
func TestLifecycleSweepRetriesOnlyUnconvergedServers(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{
		runnableServer("srv-ready", server.PhaseReady, 1, 1, 0),
		runnableServer("srv-degraded", server.PhaseDegraded, 1, 0, 1),
	}}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 5*time.Millisecond))
	// 重放 2 次 + 至少一次巡检重试。
	waitForCalls(t, syncer, 4)
	closeLifecycle(t, lifecycle)

	calls := syncer.recorded()
	if countOf(calls, "srv-ready") != 1 {
		t.Errorf("converged server synced %d times, want 1 (replay only): %v", countOf(calls, "srv-ready"), calls)
	}
	if countOf(calls, "srv-degraded") < 2 {
		t.Errorf("unconverged server synced %d times, want repeated retries: %v", countOf(calls, "srv-degraded"), calls)
	}
}

// observedRevision 落后于当前 revision 同样属于未收敛（配置已变更但运行态未应用）。
func TestLifecycleSweepRetriesStaleRevision(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{
		runnableServer("srv-stale", server.PhaseReady, 2, 1, 0),
	}}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 5*time.Millisecond))
	waitForCalls(t, syncer, 2)
	closeLifecycle(t, lifecycle)

	if calls := syncer.recorded(); countOf(calls, "srv-stale") < 2 {
		t.Fatalf("stale-revision server synced %d times, want retries: %v", countOf(calls, "srv-stale"), calls)
	}
}

// interval=0 关闭周期巡检：只做启动重放，未收敛的 Server 不再自动重试。
func TestLifecycleSweepDisabledWhenIntervalZero(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{
		runnableServer("srv-1", server.PhaseDegraded, 1, 0, 1),
	}}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 0))
	waitForCalls(t, syncer, 1)
	time.Sleep(50 * time.Millisecond)
	closeLifecycle(t, lifecycle)

	if calls := syncer.recorded(); len(calls) != 1 {
		t.Fatalf("sync calls = %v, want only the startup replay", calls)
	}
}

// 列表失败不能让进程停摆：记日志并等下一个 tick。
func TestLifecycleSweepRecoversAfterListFailure(t *testing.T) {
	lister := &fakeLister{
		servers:   []server.Server{runnableServer("srv-1", server.PhasePending, 1, 0, 0)},
		failFirst: true,
	}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 5*time.Millisecond))
	waitForCalls(t, syncer, 1)
	closeLifecycle(t, lifecycle)

	if lister.callCount() < 2 {
		t.Fatalf("list calls = %d, want the failed startup replay plus a retry", lister.callCount())
	}
}

// 正在同步的 Server 不被重复入队：否则一次长同步会在每个 tick 后立刻被再消费一次。
func TestLifecycleSkipsInFlightServer(t *testing.T) {
	registry := &fakeRegistry{srv: runnableServer("srv-1", server.PhasePending, 1, 0, 0)}
	block := make(chan struct{})
	started := make(chan server.ID, 1)
	syncer := &fakeSyncer{block: block, started: started}
	lifecycle := newTestLifecycle(t, syncer, registry)

	lifecycle.Trigger("srv-1")
	waitForID(t, started, "srv-1")
	lifecycle.Trigger("srv-1")
	lifecycle.Trigger("srv-1")
	close(block)

	time.Sleep(50 * time.Millisecond)
	closeLifecycle(t, lifecycle)

	if calls := syncer.recorded(); len(calls) != 1 {
		t.Fatalf("sync calls = %v, want 1 (triggers during an in-flight sync are dropped)", calls)
	}
}

func TestRefreshToolsEnqueuesSync(t *testing.T) {
	registry := &fakeRegistry{srv: runnableServer("srv-1", server.PhaseReady, 1, 1, 0)}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, registry)

	srv, err := lifecycle.RefreshTools(context.Background(), "srv-1")
	if err != nil {
		t.Fatalf("refresh tools: %v", err)
	}
	if srv.ID != "srv-1" {
		t.Fatalf("returned server = %q, want srv-1", srv.ID)
	}
	if calls := waitForCalls(t, syncer, 1); calls[0] != "srv-1" {
		t.Fatalf("sync calls = %v, want srv-1", calls)
	}
	closeLifecycle(t, lifecycle)
}

func TestRefreshToolsRejectsNotRunnable(t *testing.T) {
	disabled := runnableServer("srv-1", server.PhaseReady, 1, 1, 0)
	disabled.Enabled = false
	stopped := runnableServer("srv-2", server.PhaseStopped, 1, 1, 0)
	stopped.Spec.DesiredState = server.DesiredStopped

	tests := []struct {
		name string
		srv  server.Server
	}{
		{"disabled", disabled},
		{"desired stopped", stopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &fakeRegistry{srv: tt.srv}
			syncer := &fakeSyncer{}
			lifecycle := newTestLifecycle(t, syncer, registry)

			if _, err := lifecycle.RefreshTools(context.Background(), tt.srv.ID); !errors.Is(err, server.ErrNotRunnable) {
				t.Fatalf("refresh tools error = %v, want ErrNotRunnable", err)
			}
			closeLifecycle(t, lifecycle)
			if calls := syncer.recorded(); len(calls) != 0 {
				t.Fatalf("sync calls = %v, want none for a non-runnable server", calls)
			}
		})
	}
}

func TestRefreshToolsPropagatesNotFound(t *testing.T) {
	registry := &fakeRegistry{err: server.ErrNotFound}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, registry)

	if _, err := lifecycle.RefreshTools(context.Background(), "ghost"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("refresh tools error = %v, want ErrNotFound", err)
	}
	closeLifecycle(t, lifecycle)
	if calls := syncer.recorded(); len(calls) != 0 {
		t.Fatalf("sync calls = %v, want none for a missing server", calls)
	}
}

func TestLifecycleCloseStopsReconciler(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{
		runnableServer("srv-1", server.PhaseDegraded, 1, 0, 1),
	}}
	syncer := &fakeSyncer{}
	lifecycle := newTestLifecycle(t, syncer, &fakeRegistry{}, withLister(lister, 5*time.Millisecond))
	waitForCalls(t, syncer, 2)
	closeLifecycle(t, lifecycle)

	before := len(syncer.recorded())
	time.Sleep(30 * time.Millisecond)
	if after := len(syncer.recorded()); after != before {
		t.Fatalf("sync calls after Close = %d, want Reconciler stopped at %d", after, before)
	}
	lifecycle.Trigger("srv-1")
	time.Sleep(20 * time.Millisecond)
	if after := len(syncer.recorded()); after != before {
		t.Fatalf("sync calls after Trigger on closed lifecycle = %d, want %d", after, before)
	}
}

func TestNewLifecycleRequiresDependencies(t *testing.T) {
	tests := []struct {
		name string
		opts app.LifecycleOptions
	}{
		{"missing registry", app.LifecycleOptions{Syncer: &fakeSyncer{}, Auditor: &recordingAuditor{}}},
		{"missing syncer", app.LifecycleOptions{Registry: &fakeRegistry{}, Auditor: &recordingAuditor{}}},
		{"missing auditor", app.LifecycleOptions{Registry: &fakeRegistry{}, Syncer: &fakeSyncer{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := app.NewLifecycle(tt.opts); err == nil {
				t.Fatal("NewLifecycle accepted incomplete options")
			}
		})
	}
}
