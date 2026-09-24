package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

func TestNewManagerRejectsInvalidProviders(t *testing.T) {
	repo := newManagerRepo(testServer("server-1", 1))
	provider := &testProvider{runtimeType: server.RuntimeRemote}

	cases := []struct {
		name      string
		repo      server.Repository
		providers []Provider
	}{
		{name: "nil repository", providers: []Provider{provider}},
		{name: "no providers", repo: repo},
		{name: "nil provider", repo: repo, providers: []Provider{nil}},
		{name: "duplicate provider type", repo: repo, providers: []Provider{provider, &testProvider{runtimeType: server.RuntimeRemote}}},
		{name: "empty provider type", repo: repo, providers: []Provider{&testProvider{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewManager(tc.repo, tc.providers...); err == nil {
				t.Fatal("NewManager() error = nil, want error")
			}
		})
	}
}

func TestEnsureReadyReloadsCurrentServerAndReplacesRevision(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{runtimeType: server.RuntimeRemote}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	stale := srv
	stale.Revision = 99
	first, err := manager.EnsureReady(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if first.ObservedRevision != 1 || provider.ensureCount() != 1 {
		t.Fatalf("first instance = %#v; Ensure calls = %d", first, provider.ensureCount())
	}

	if _, err := manager.EnsureReady(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if provider.ensureCount() != 1 {
		t.Fatalf("same revision caused %d Ensure calls, want 1", provider.ensureCount())
	}

	repo.update("server-1", func(current *server.Server) { current.Revision = 2 })
	second, err := manager.EnsureReady(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if second.ObservedRevision != 2 || provider.ensureCount() != 2 || provider.stopCount() != 1 {
		t.Fatalf("after revision change: instance=%#v Ensure=%d Stop=%d", second, provider.ensureCount(), provider.stopCount())
	}
	stored := repo.get("server-1")
	if stored.Status.Phase != server.PhaseStarting || stored.Status.ObservedRevision != 2 || stored.Status.LastSuccessAt == nil {
		t.Fatalf("persisted status = %#v", stored.Status)
	}
}

func TestEnsureReadySerializesSameServerAndAllowsDifferentServers(t *testing.T) {
	ctx := context.Background()
	first := testServer("server-1", 1)
	second := testServer("server-2", 1)
	repo := newManagerRepo(first, second)
	entered := make(chan server.ID, 2)
	release := make(chan struct{})
	provider := &testProvider{
		runtimeType: server.RuntimeRemote,
		ensure: func(ctx context.Context, srv server.Server) (Instance, error) {
			entered <- srv.ID
			select {
			case <-release:
				return testInstance(srv), nil
			case <-ctx.Done():
				return Instance{}, ctx.Err()
			}
		},
	}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	call := func(srv server.Server) {
		defer wg.Done()
		_, err := manager.EnsureReady(ctx, srv)
		errs <- err
	}
	wg.Add(1)
	go call(first)
	if id := receiveID(t, entered); id != first.ID {
		t.Fatalf("first provider call server = %q, want %q", id, first.ID)
	}
	wg.Add(1)
	go call(first)
	wg.Add(1)
	go call(second)
	if id := receiveID(t, entered); id != second.ID {
		t.Fatalf("second concurrent provider call server = %q, want %q", id, second.ID)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := provider.ensureCount(); got != 2 {
		t.Fatalf("Ensure calls = %d, want one per Server", got)
	}
}

func TestEnsureReadyRetriesStatusWriteWithoutEnsuringTwice(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	repo.failStatusWrites = 1
	provider := &testProvider{runtimeType: server.RuntimeRemote}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.EnsureReady(ctx, srv); err == nil {
		t.Fatal("first EnsureReady() error = nil, want status persistence error")
	}
	if _, err := manager.EnsureReady(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if provider.ensureCount() != 1 || provider.inspectCount() != 1 {
		t.Fatalf("Ensure calls = %d, Inspect calls = %d; want 1 each", provider.ensureCount(), provider.inspectCount())
	}
	if got := repo.get("server-1").Status.Phase; got != server.PhaseStarting {
		t.Fatalf("status phase = %q, want starting", got)
	}
}

// EnsureReady 只负责运行态：它不得声称 ready（ready 由 Tool 快照刷新写入），
// 也不得把已发布的 ready 打回 starting，否则 tools/call 路径每次 Ensure 都会抖动。
func TestEnsureReadyPhaseTransitions(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		before server.Status
		want   server.Phase
	}{
		{"pending becomes starting", server.Status{Phase: server.PhasePending}, server.PhaseStarting},
		{"stopped becomes starting", server.Status{Phase: server.PhaseStopped, ObservedRevision: 1}, server.PhaseStarting},
		{"failed becomes starting", server.Status{Phase: server.PhaseFailed, ObservedRevision: 1}, server.PhaseStarting},
		{"starting stays starting", server.Status{Phase: server.PhaseStarting, ObservedRevision: 1}, server.PhaseStarting},
		{"ready stays ready", server.Status{Phase: server.PhaseReady, ObservedRevision: 1}, server.PhaseReady},
		{"degraded stays degraded", server.Status{Phase: server.PhaseDegraded, ObservedRevision: 1}, server.PhaseDegraded},
		{"new revision restarts from starting", server.Status{Phase: server.PhaseReady, ObservedRevision: 0}, server.PhaseStarting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer("server-1", 1)
			srv.Status = tc.before
			repo := newManagerRepo(srv)
			manager, err := NewManager(repo, &testProvider{runtimeType: server.RuntimeRemote})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.EnsureReady(ctx, srv); err != nil {
				t.Fatal(err)
			}
			stored := repo.get("server-1").Status
			if stored.Phase != tc.want {
				t.Fatalf("phase = %q, want %q", stored.Phase, tc.want)
			}
			if stored.ObservedRevision != 1 || stored.LastSuccessAt == nil || stored.ConsecutiveFailures != 0 {
				t.Fatalf("observed status = %#v", stored)
			}
		})
	}
}

func TestEnsureReadyRejectsPolicyAndDoesNotContactProvider(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{runtimeType: server.RuntimeRemote}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	repo.update("server-1", func(current *server.Server) { current.Enabled = false })
	if _, err := manager.EnsureReady(ctx, srv); !errors.Is(err, ErrServerDisabled) {
		t.Fatalf("disabled EnsureReady() error = %v, want ErrServerDisabled", err)
	}
	repo.update("server-1", func(current *server.Server) {
		current.Enabled = true
		current.Spec.DesiredState = server.DesiredStopped
	})
	if _, err := manager.EnsureReady(ctx, srv); !errors.Is(err, ErrDesiredStateStopped) {
		t.Fatalf("stopped EnsureReady() error = %v, want ErrDesiredStateStopped", err)
	}
	if provider.ensureCount() != 0 || repo.get("server-1").Status.Phase != server.PhasePending {
		t.Fatalf("provider Ensure calls = %d, status = %#v", provider.ensureCount(), repo.get("server-1").Status)
	}
}

func TestEnsureReadyFailureStoresGenericDegradedStatus(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{
		runtimeType: server.RuntimeRemote,
		ensure: func(context.Context, server.Server) (Instance, error) {
			return Instance{}, errors.New("connect https://user:secret@example.test failed")
		},
	}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.EnsureReady(ctx, srv); err == nil {
		t.Fatal("EnsureReady() error = nil, want provider error")
	}
	stored := repo.get("server-1").Status
	if stored.Phase != server.PhaseDegraded || stored.ConsecutiveFailures != 1 || stored.Message != "runtime ensure failed" {
		t.Fatalf("persisted failure status = %#v", stored)
	}
	if strings.Contains(stored.Message, "secret") || strings.Contains(stored.Message, "example.test") {
		t.Fatalf("status leaked provider error: %q", stored.Message)
	}
}

func TestStopRetainsFailedInstanceAndReconcileHonorsDesiredState(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	stopCalls := 0
	provider := &testProvider{
		runtimeType: server.RuntimeRemote,
		stop: func(context.Context, Instance) error {
			stopCalls++
			if stopCalls == 1 {
				return errors.New("temporary stop failure")
			}
			return nil
		},
	}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.EnsureReady(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx, srv.ID); err == nil {
		t.Fatal("first Stop() error = nil, want provider failure")
	}
	if got := repo.get("server-1").Status.Phase; got != server.PhaseDegraded {
		t.Fatalf("status after failed Stop = %q, want degraded", got)
	}

	if err := manager.Stop(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if stopCalls != 2 || repo.get("server-1").Status.Phase != server.PhaseStopped {
		t.Fatalf("Stop calls = %d, status = %#v", stopCalls, repo.get("server-1").Status)
	}

	repo.update("server-1", func(current *server.Server) { current.Spec.DesiredState = server.DesiredStopped })
	if err := manager.Reconcile(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if provider.ensureCount() != 1 || repo.get("server-1").Status.Phase != server.PhaseStopped {
		t.Fatalf("Ensure calls = %d, status = %#v", provider.ensureCount(), repo.get("server-1").Status)
	}
}

func TestStopRetriesStatusWriteWithoutStoppingTwice(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{runtimeType: server.RuntimeRemote}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.EnsureReady(ctx, srv); err != nil {
		t.Fatal(err)
	}
	repo.failStatusWrites = 1
	if err := manager.Stop(ctx, srv.ID); err == nil {
		t.Fatal("first Stop() error = nil, want status persistence error")
	}
	if err := manager.Stop(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if provider.stopCount() != 1 || repo.get("server-1").Status.Phase != server.PhaseStopped {
		t.Fatalf("Stop calls = %d, status = %#v", provider.stopCount(), repo.get("server-1").Status)
	}
}

func TestEnsureReadyStopsOldProviderBeforeReportingMissingReplacement(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{runtimeType: server.RuntimeRemote}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.EnsureReady(ctx, srv); err != nil {
		t.Fatal(err)
	}
	repo.update("server-1", func(current *server.Server) {
		current.Revision++
		current.Spec.Runtime.Type = server.RuntimeDocker
	})
	if _, err := manager.EnsureReady(ctx, srv); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("EnsureReady() error = %v, want ErrProviderNotFound", err)
	}
	if provider.stopCount() != 1 || repo.get("server-1").Status.Phase != server.PhaseDegraded {
		t.Fatalf("old Provider stops = %d, status = %#v", provider.stopCount(), repo.get("server-1").Status)
	}
}

func TestEnsureReadyCleansUpInvalidProviderResult(t *testing.T) {
	ctx := context.Background()
	srv := testServer("server-1", 1)
	repo := newManagerRepo(srv)
	provider := &testProvider{
		runtimeType: server.RuntimeRemote,
		ensure: func(context.Context, server.Server) (Instance, error) {
			return Instance{ID: "invalid", ServerID: "another-server", Provider: string(server.RuntimeRemote), Phase: runtimePhaseRunning}, nil
		},
	}
	manager, err := NewManager(repo, provider)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.EnsureReady(ctx, srv); !errors.Is(err, ErrInvalidInstance) {
		t.Fatalf("EnsureReady() error = %v, want ErrInvalidInstance", err)
	}
	if provider.stopCount() != 1 || repo.get("server-1").Status.Phase != server.PhaseDegraded {
		t.Fatalf("cleanup Stop calls = %d, status = %#v", provider.stopCount(), repo.get("server-1").Status)
	}
}

func receiveID(t *testing.T, ch <-chan server.ID) server.ID {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Provider.Ensure")
		return ""
	}
}

func testServer(id server.ID, revision int64) server.Server {
	return server.Server{
		ID:       id,
		Enabled:  true,
		Revision: revision,
		Spec: server.Spec{
			Runtime:      server.RuntimeSpec{Type: server.RuntimeRemote},
			DesiredState: server.DesiredRunning,
		},
		Status: server.Status{Phase: server.PhasePending},
	}
}

func testInstance(srv server.Server) Instance {
	return Instance{
		ID:               fmt.Sprintf("%s-%d", srv.ID, srv.Revision),
		ServerID:         srv.ID,
		Provider:         string(server.RuntimeRemote),
		Phase:            runtimePhaseRunning,
		ObservedRevision: srv.Revision,
	}
}

type testProvider struct {
	runtimeType server.RuntimeType
	ensure      func(context.Context, server.Server) (Instance, error)
	inspect     func(context.Context, Instance) (Instance, error)
	stop        func(context.Context, Instance) error

	mu          sync.Mutex
	ensures     int
	inspections int
	stops       int
}

func (p *testProvider) Type() server.RuntimeType { return p.runtimeType }

func (p *testProvider) Ensure(ctx context.Context, srv server.Server) (Instance, error) {
	p.mu.Lock()
	p.ensures++
	fn := p.ensure
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, srv)
	}
	return testInstance(srv), nil
}

func (p *testProvider) Stop(ctx context.Context, instance Instance) error {
	p.mu.Lock()
	p.stops++
	fn := p.stop
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, instance)
	}
	return nil
}

func (p *testProvider) Inspect(ctx context.Context, instance Instance) (Instance, error) {
	p.mu.Lock()
	p.inspections++
	fn := p.inspect
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, instance)
	}
	return instance, nil
}

func (p *testProvider) Logs(context.Context, Instance, LogOptions) (io.ReadCloser, error) {
	return nil, errors.New("logs not implemented by test provider")
}

func (p *testProvider) ensureCount() int  { p.mu.Lock(); defer p.mu.Unlock(); return p.ensures }
func (p *testProvider) inspectCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.inspections }
func (p *testProvider) stopCount() int    { p.mu.Lock(); defer p.mu.Unlock(); return p.stops }

type managerRepo struct {
	mu               sync.Mutex
	servers          map[server.ID]server.Server
	failStatusWrites int
}

func newManagerRepo(servers ...server.Server) *managerRepo {
	repo := &managerRepo{servers: make(map[server.ID]server.Server, len(servers))}
	for _, srv := range servers {
		repo.servers[srv.ID] = srv
	}
	return repo
}

func (r *managerRepo) Create(_ context.Context, srv server.Server) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.servers[srv.ID]; exists {
		return server.ErrAlreadyExists
	}
	r.servers[srv.ID] = srv
	return nil
}

func (r *managerRepo) GetByID(_ context.Context, id server.ID) (server.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	srv, ok := r.servers[id]
	if !ok {
		return server.Server{}, server.ErrNotFound
	}
	return srv, nil
}

func (r *managerRepo) GetByName(_ context.Context, namespace, name string) (server.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, srv := range r.servers {
		if srv.Namespace == namespace && srv.Name == name {
			return srv, nil
		}
	}
	return server.Server{}, server.ErrNotFound
}

func (r *managerRepo) ListEnabled(context.Context) ([]server.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var servers []server.Server
	for _, srv := range r.servers {
		if srv.Enabled {
			servers = append(servers, srv)
		}
	}
	return servers, nil
}

func (r *managerRepo) UpdateSpec(_ context.Context, id server.ID, expectedRevision int64, mutate func(*server.Spec) error) (server.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	srv, ok := r.servers[id]
	if !ok {
		return server.Server{}, server.ErrConflict
	}
	if srv.Revision != expectedRevision {
		return server.Server{}, server.ErrConflict
	}
	if err := mutate(&srv.Spec); err != nil {
		return server.Server{}, err
	}
	srv.Revision++
	r.servers[id] = srv
	return srv, nil
}

func (r *managerRepo) UpdateStatus(_ context.Context, id server.ID, input server.CreateStatusInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failStatusWrites > 0 {
		r.failStatusWrites--
		return errors.New("injected status write failure")
	}
	srv, ok := r.servers[id]
	if !ok {
		return server.ErrNotFound
	}
	srv.Status = server.Status{
		Phase:               input.Phase,
		Message:             input.Message,
		ObservedRevision:    input.ObservedRevision,
		LastHealthAt:        input.LastHealthAt,
		LastSuccessAt:       input.LastSuccessAt,
		ConsecutiveFailures: input.ConsecutiveFailures,
	}
	r.servers[id] = srv
	return nil
}

func (r *managerRepo) SetEnabled(_ context.Context, id server.ID, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	srv, ok := r.servers[id]
	if !ok {
		return server.ErrNotFound
	}
	srv.Enabled = enabled
	r.servers[id] = srv
	return nil
}

func (r *managerRepo) update(id server.ID, mutate func(*server.Server)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	srv := r.servers[id]
	mutate(&srv)
	r.servers[id] = srv
}

func (r *managerRepo) get(id server.ID) server.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.servers[id]
}

var _ server.Repository = (*managerRepo)(nil)
