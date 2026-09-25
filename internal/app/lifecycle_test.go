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
	mu     sync.Mutex
	srv    server.Server
	err    error
	inputs []server.RegisterInput
	gets   []server.ID
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
