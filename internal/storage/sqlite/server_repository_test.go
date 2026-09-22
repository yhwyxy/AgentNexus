package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func newTestRepo(t *testing.T) (*sqlite.ServerRepository, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewServerRepository(db), ctx
}

func remoteServer() server.Server {
	return server.Server{
		ID:        "srv-1",
		Namespace: "default",
		Name:      "weather",
		Enabled:   true,
		Revision:  1,
		Labels:    map[string]string{"team": "ops"},
		Spec: server.Spec{
			Transport: server.TransportStreamableHTTP,
			Runtime: server.RuntimeSpec{
				Type: server.RuntimeRemote,
				Remote: &server.RemoteSpec{
					Endpoint: "http://weather:8080/mcp",
					Headers:  map[string]string{"X-Env": "test"},
				},
			},
			Timeouts:     server.TimeoutSpec{Connect: 5 * time.Second, List: 10 * time.Second, Call: 60 * time.Second},
			Limits:       server.LimitSpec{MaxInFlight: 16},
			DesiredState: server.DesiredRunning,
		},
	}
}

func TestServerRepositoryCreateAndGet(t *testing.T) {
	repo, ctx := newTestRepo(t)
	want := remoteServer()

	if err := repo.Create(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	assertEqualServer(t, want, got)

	got, err = repo.GetByName(ctx, want.Namespace, want.Name)
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	assertEqualServer(t, want, got)
}

func TestServerRepositoryCreateDuplicate(t *testing.T) {
	repo, ctx := newTestRepo(t)
	s := remoteServer()

	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("first create: %v", err)
	}

	err := repo.Create(ctx, s)
	if !errors.Is(err, server.ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
}

func TestServerRepositoryGetMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	if _, err := repo.GetByID(ctx, "no-such-id"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("GetByID err = %v, want ErrNotFound", err)
	}

	if _, err := repo.GetByName(ctx, "default", "ghost"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("GetByName err = %v, want ErrNotFound", err)
	}
}

func TestServerRepositoryListEnabled(t *testing.T) {
	repo, ctx := newTestRepo(t)

	a := remoteServer() // enabled
	b := remoteServer()
	b.ID, b.Name = "srv-2", "search"
	c := remoteServer()
	c.ID, c.Name, c.Enabled = "srv-3", "hidden", false

	for i, s := range []server.Server{a, b, c} {
		if err := repo.Create(ctx, s); err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
	}

	got, err := repo.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListEnabled len = %d, want 2", len(got))
	}
	// ORDER BY namespace, name: search 在 weather 前
	if got[0].Name != "search" || got[1].Name != "weather" {
		t.Fatalf("ListEnabled order = [%s, %s], want [search, weather]", got[0].Name, got[1].Name)
	}
}

func TestServerRepositoryUpdateSpec(t *testing.T) {
	repo, ctx := newTestRepo(t)
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	repo.WithClock(now)

	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}
	advance(time.Minute)

	updated, err := repo.UpdateSpec(ctx, "srv-1", 1, func(spec *server.Spec) error {
		spec.Runtime.Remote.Endpoint = "http://weather-v2:8080/mcp"
		spec.Timeouts.Call = 90 * time.Second
		spec.DesiredState = server.DesiredStopped
		return nil
	})
	if err != nil {
		t.Fatalf("update spec: %v", err)
	}

	if updated.Revision != 2 {
		t.Errorf("returned Revision = %d, want 2", updated.Revision)
	}
	if !updated.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("returned UpdatedAt = %v, want %v", updated.UpdatedAt, t0.Add(time.Minute))
	}
	if !updated.CreatedAt.Equal(t0) {
		t.Errorf("returned CreatedAt = %v, want %v (must not change)", updated.CreatedAt, t0)
	}

	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Revision != 2 {
		t.Errorf("persisted Revision = %d, want 2", got.Revision)
	}
	if got.Spec.Runtime.Remote.Endpoint != "http://weather-v2:8080/mcp" {
		t.Errorf("persisted Endpoint = %q, want %q", got.Spec.Runtime.Remote.Endpoint, "http://weather-v2:8080/mcp")
	}
	if got.Spec.Timeouts.Call != 90*time.Second {
		t.Errorf("persisted Call timeout = %v, want 90s", got.Spec.Timeouts.Call)
	}
	if got.Spec.DesiredState != server.DesiredStopped {
		t.Errorf("persisted DesiredState = %q, want %q", got.Spec.DesiredState, server.DesiredStopped)
	}
	if !reflect.DeepEqual(got.Spec, updated.Spec) {
		t.Errorf("returned Spec = %+v, persisted Spec = %+v", updated.Spec, got.Spec)
	}
}

func TestServerRepositoryUpdateSpecStaleRevision(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	called := false
	_, err := repo.UpdateSpec(ctx, "srv-1", 7, func(spec *server.Spec) error {
		called = true
		spec.Runtime.Remote.Endpoint = "http://stale:8080/mcp"
		return nil
	})
	if !errors.Is(err, server.ErrConflict) {
		t.Fatalf("UpdateSpec err = %v, want ErrConflict", err)
	}
	if called {
		t.Error("mutate was called despite revision mismatch")
	}

	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Revision != 1 || got.Spec.Runtime.Remote.Endpoint != "http://weather:8080/mcp" {
		t.Errorf("server changed after conflict: revision=%d endpoint=%q",
			got.Revision, got.Spec.Runtime.Remote.Endpoint)
	}
}

func TestServerRepositoryUpdateSpecMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	_, err := repo.UpdateSpec(ctx, "no-such-id", 1, func(*server.Spec) error { return nil })
	if !errors.Is(err, server.ErrConflict) {
		t.Fatalf("UpdateSpec err = %v, want ErrConflict", err)
	}
}

func TestServerRepositoryUpdateSpecMutateError(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	errRejected := errors.New("rejected by caller")
	_, err := repo.UpdateSpec(ctx, "srv-1", 1, func(spec *server.Spec) error {
		spec.Timeouts.Call = 90 * time.Second
		return errRejected
	})
	if !errors.Is(err, errRejected) {
		t.Fatalf("UpdateSpec err = %v, want %v", err, errRejected)
	}

	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Revision != 1 || got.Spec.Timeouts.Call != 60*time.Second {
		t.Errorf("server changed after mutate error: revision=%d call=%v",
			got.Revision, got.Spec.Timeouts.Call)
	}
}

func TestServerRepositoryUpdateStatus(t *testing.T) {
	repo, ctx := newTestRepo(t)
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	repo.WithClock(now)

	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}
	advance(time.Minute)

	healthAt := t0.Add(30 * time.Second)
	successAt := t0.Add(20 * time.Second)
	err := repo.UpdateStatus(ctx, "srv-1", server.CreateStatusInput{
		Phase:               server.PhaseDegraded,
		Message:             "health check failed",
		ObservedRevision:    1,
		LastHealthAt:        &healthAt,
		LastSuccessAt:       &successAt,
		ConsecutiveFailures: 2,
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Status.Phase != server.PhaseDegraded {
		t.Errorf("Phase = %q, want %q", got.Status.Phase, server.PhaseDegraded)
	}
	if got.Status.Message != "health check failed" {
		t.Errorf("Message = %q, want %q", got.Status.Message, "health check failed")
	}
	if got.Status.ObservedRevision != 1 {
		t.Errorf("ObservedRevision = %d, want 1", got.Status.ObservedRevision)
	}
	if got.Status.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", got.Status.ConsecutiveFailures)
	}
	if got.Status.LastHealthAt == nil || !got.Status.LastHealthAt.Equal(healthAt) {
		t.Errorf("LastHealthAt = %v, want %v", got.Status.LastHealthAt, healthAt)
	}
	if got.Status.LastSuccessAt == nil || !got.Status.LastSuccessAt.Equal(successAt) {
		t.Errorf("LastSuccessAt = %v, want %v", got.Status.LastSuccessAt, successAt)
	}

	// 观测状态更新不得触碰配置版本、Spec 或配置时间戳。
	if got.Revision != 1 {
		t.Errorf("Revision = %d, want 1 (status update must not bump revision)", got.Revision)
	}
	if !reflect.DeepEqual(got.Spec, remoteServer().Spec) {
		t.Errorf("Spec changed by status update: %+v", got.Spec)
	}
	if !got.UpdatedAt.Equal(t0) {
		t.Errorf("UpdatedAt = %v, want %v (status update must not touch config timestamp)", got.UpdatedAt, t0)
	}
}

func TestServerRepositoryUpdateStatusClearsOptionalTimes(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	if err := repo.UpdateStatus(ctx, "srv-1", server.CreateStatusInput{
		Phase:         server.PhaseReady,
		LastHealthAt:  &at,
		LastSuccessAt: &at,
	}); err != nil {
		t.Fatalf("first update status: %v", err)
	}

	if err := repo.UpdateStatus(ctx, "srv-1", server.CreateStatusInput{
		Phase: server.PhaseStopped,
	}); err != nil {
		t.Fatalf("second update status: %v", err)
	}

	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Status.LastHealthAt != nil {
		t.Errorf("LastHealthAt = %v, want nil", got.Status.LastHealthAt)
	}
	if got.Status.LastSuccessAt != nil {
		t.Errorf("LastSuccessAt = %v, want nil", got.Status.LastSuccessAt)
	}
}

func TestServerRepositoryUpdateStatusMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	err := repo.UpdateStatus(ctx, "no-such-id", server.CreateStatusInput{Phase: server.PhaseReady})
	if !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("UpdateStatus err = %v, want ErrNotFound", err)
	}
}

func TestServerRepositorySetEnabled(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.SetEnabled(ctx, "srv-1", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true after SetEnabled(false)")
	}
	listed, err := repo.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("ListEnabled len = %d after disable, want 0", len(listed))
	}

	if err := repo.SetEnabled(ctx, "srv-1", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	listed, err = repo.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("ListEnabled len = %d after enable, want 1", len(listed))
	}
}

func TestServerRepositorySetEnabledMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	if err := repo.SetEnabled(ctx, "no-such-id", false); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("SetEnabled err = %v, want ErrNotFound", err)
	}
}

// fixedClock 返回可手动推进的确定性时钟，用于断言 Repository 写入的时间戳。
func fixedClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	current := start
	return func() time.Time { return current },
		func(d time.Duration) { current = current.Add(d) }
}

// assertEqualServer 比较调用方写入的字段，并检查 Create 自行填充的
// 时间戳与初始 Status。want 的 CreatedAt/UpdatedAt/Status 为零值，
// 由 Repository 负责填充，因此不做整体 DeepEqual。
func assertEqualServer(t *testing.T, want, got server.Server) {
	t.Helper()

	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.Namespace != want.Namespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, want.Namespace)
	}
	if got.Name != want.Name {
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	}
	if got.DisplayName != want.DisplayName {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, want.DisplayName)
	}
	if got.Description != want.Description {
		t.Errorf("Description = %q, want %q", got.Description, want.Description)
	}
	if !reflect.DeepEqual(got.Labels, want.Labels) {
		t.Errorf("Labels = %v, want %v", got.Labels, want.Labels)
	}
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.Revision != want.Revision {
		t.Errorf("Revision = %d, want %d", got.Revision, want.Revision)
	}
	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Errorf("Spec = %+v, want %+v", got.Spec, want.Spec)
	}

	if got.Status.Phase != server.PhasePending {
		t.Errorf("Status.Phase = %q, want %q", got.Status.Phase, server.PhasePending)
	}
	if got.Status.ObservedRevision != 0 {
		t.Errorf("Status.ObservedRevision = %d, want 0", got.Status.ObservedRevision)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want set by Create")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero, want set by Create")
	}
}
