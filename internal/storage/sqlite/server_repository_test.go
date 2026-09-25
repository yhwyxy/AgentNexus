package sqlite_test

import (
	"context"
	"database/sql"
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
	repo, _, ctx := newTestRepoWithDB(t)
	return repo, ctx
}

// newTestRepoWithDB 额外返回底层连接，供需要预置关联行（如 credentials 外键）的用例使用。
func newTestRepoWithDB(t *testing.T) (*sqlite.ServerRepository, *sql.DB, context.Context) {
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
	return sqlite.NewServerRepository(db), db, ctx
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

func TestServerRepositoryUpdate(t *testing.T) {
	repo, db, ctx := newTestRepoWithDB(t)
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	repo.WithClock(now)

	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// mcp_servers.credential_id 有指向 credentials 的外键，先落一行可引用的凭据。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO credentials (id, name, type, secret_ref, created_at, updated_at)
		VALUES ('cred-1', 'weather-key', 'bearer', 'env:WEATHER_KEY', '2026-09-20T10:00:00Z', '2026-09-20T10:00:00Z')`,
	); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	advance(time.Minute)

	credID := "cred-1"
	updated, err := repo.Update(ctx, "srv-1", 1, server.UpdateInput{
		DisplayName: "Weather v2",
		Description: "updated",
		// 空 labels 必须落回 '{}' 并读成非 nil 空 map。
		Labels: map[string]string{},
		Runtime: server.RuntimeSpec{
			Type:   server.RuntimeDocker,
			Docker: &server.DockerSpec{Image: "weather:2", Port: 8080},
		},
		CredentialID: &credID,
		Timeouts:     server.TimeoutSpec{Connect: 2 * time.Second, List: 3 * time.Second, Call: 4 * time.Second},
		Limits:       server.LimitSpec{MaxInFlight: 8},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
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

	// Update 是全量替换：元数据、runtime、凭据、超时、限流都被输入覆盖。
	if got.DisplayName != "Weather v2" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Weather v2")
	}
	if got.Description != "updated" {
		t.Errorf("Description = %q, want %q", got.Description, "updated")
	}
	if got.Labels == nil || len(got.Labels) != 0 {
		t.Errorf("Labels = %#v, want non-nil empty map", got.Labels)
	}
	if got.Spec.Runtime.Type != server.RuntimeDocker {
		t.Errorf("Runtime.Type = %q, want %q", got.Spec.Runtime.Type, server.RuntimeDocker)
	}
	if got.Spec.Runtime.Docker == nil || got.Spec.Runtime.Docker.Image != "weather:2" {
		t.Errorf("Runtime.Docker = %+v, want image weather:2", got.Spec.Runtime.Docker)
	}
	if got.Spec.CredentialID == nil || *got.Spec.CredentialID != "cred-1" {
		t.Errorf("CredentialID = %v, want cred-1", got.Spec.CredentialID)
	}
	wantTimeouts := server.TimeoutSpec{Connect: 2 * time.Second, List: 3 * time.Second, Call: 4 * time.Second}
	if got.Spec.Timeouts != wantTimeouts {
		t.Errorf("Timeouts = %+v, want %+v", got.Spec.Timeouts, wantTimeouts)
	}
	if got.Spec.Limits.MaxInFlight != 8 {
		t.Errorf("MaxInFlight = %d, want 8", got.Spec.Limits.MaxInFlight)
	}
	if got.Revision != 2 {
		t.Errorf("persisted Revision = %d, want 2", got.Revision)
	}
	if !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("persisted UpdatedAt = %v, want %v", got.UpdatedAt, t0.Add(time.Minute))
	}

	// transport 与 desired_state 不属于 Update 的可变集合，必须保持原值。
	if got.Spec.Transport != server.TransportStreamableHTTP {
		t.Errorf("Transport = %q, want unchanged %q", got.Spec.Transport, server.TransportStreamableHTTP)
	}
	if got.Spec.DesiredState != server.DesiredRunning {
		t.Errorf("DesiredState = %q, want unchanged %q", got.Spec.DesiredState, server.DesiredRunning)
	}
	if !reflect.DeepEqual(got, updated) {
		t.Errorf("returned server = %+v, persisted = %+v", updated, got)
	}
}

func TestServerRepositoryUpdateStaleRevision(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	before, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}

	_, err = repo.Update(ctx, "srv-1", 7, server.UpdateInput{
		DisplayName: "should not apply",
		Runtime: server.RuntimeSpec{
			Type:   server.RuntimeRemote,
			Remote: &server.RemoteSpec{Endpoint: "http://stale:8080/mcp"},
		},
		Timeouts: server.TimeoutSpec{Connect: time.Second, List: time.Second, Call: time.Second},
		Limits:   server.LimitSpec{MaxInFlight: 4},
	})
	if !errors.Is(err, server.ErrConflict) {
		t.Fatalf("Update err = %v, want ErrConflict", err)
	}

	after, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	// 乐观锁失败必须整行不变：revision、时间戳、可变态都不能被触碰。
	if !reflect.DeepEqual(before, after) {
		t.Errorf("row changed after conflict:\nbefore = %+v\nafter  = %+v", before, after)
	}
}

func TestServerRepositoryUpdateMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	// 契约：目标行不存在的表现与 revision 过期一致，都是 ErrConflict。
	_, err := repo.Update(ctx, "no-such-id", 1, server.UpdateInput{
		Runtime: server.RuntimeSpec{
			Type:   server.RuntimeRemote,
			Remote: &server.RemoteSpec{Endpoint: "http://ghost:8080/mcp"},
		},
		Timeouts: server.TimeoutSpec{Connect: time.Second, List: time.Second, Call: time.Second},
		Limits:   server.LimitSpec{MaxInFlight: 4},
	})
	if !errors.Is(err, server.ErrConflict) {
		t.Fatalf("Update err = %v, want ErrConflict", err)
	}
}

func TestServerRepositoryList(t *testing.T) {
	repo, ctx := newTestRepo(t)

	// 乱序插入、横跨两个 namespace，且含 disabled 行：List 返回全部
	// （不按 enabled 过滤），并按 namespace、name 排序。
	mk := func(id, namespace, name string, enabled bool) server.Server {
		s := remoteServer()
		s.ID, s.Namespace, s.Name, s.Enabled = server.ID(id), namespace, name, enabled
		return s
	}
	for _, s := range []server.Server{
		mk("srv-3", "zeta", "alpha", true),
		mk("srv-1", "alpha", "beta", true),
		mk("srv-4", "zeta", "zulu", false),
		mk("srv-2", "alpha", "alpha", false),
	} {
		if err := repo.Create(ctx, s); err != nil {
			t.Fatalf("create %s: %v", s.ID, err)
		}
	}

	got, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha/alpha", "alpha/beta", "zeta/alpha", "zeta/zulu"}
	if len(got) != len(want) {
		t.Fatalf("List len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if id := got[i].Namespace + "/" + got[i].Name; id != w {
			t.Errorf("List[%d] = %s, want %s", i, id, w)
		}
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

	disabled, err := repo.SetEnabled(ctx, "srv-1", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true after SetEnabled(false)")
	}
	// 返回的快照必须与随后 GET 一致；enabled 是运行意图，不递增 revision。
	if !reflect.DeepEqual(disabled, got) {
		t.Errorf("SetEnabled snapshot = %+v, want %+v", disabled, got)
	}
	if got.Revision != 1 {
		t.Errorf("Revision = %d, want 1 (enabled must not bump revision)", got.Revision)
	}
	listed, err := repo.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("ListEnabled len = %d after disable, want 0", len(listed))
	}

	if _, err := repo.SetEnabled(ctx, "srv-1", true); err != nil {
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

func TestServerRepositorySetDesiredState(t *testing.T) {
	repo, ctx := newTestRepo(t)
	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	stopped, err := repo.SetDesiredState(ctx, "srv-1", server.DesiredStopped)
	if err != nil {
		t.Fatalf("set desired state: %v", err)
	}
	got, err := repo.GetByID(ctx, "srv-1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Spec.DesiredState != server.DesiredStopped {
		t.Errorf("DesiredState = %q, want %q", got.Spec.DesiredState, server.DesiredStopped)
	}
	// 返回的快照必须与随后 GET 一致；desiredState 是运行意图，不递增 revision。
	if !reflect.DeepEqual(stopped, got) {
		t.Errorf("SetDesiredState snapshot = %+v, want %+v", stopped, got)
	}
	if got.Revision != 1 {
		t.Errorf("Revision = %d, want 1 (desiredState must not bump revision)", got.Revision)
	}
}

func TestServerRepositorySetEnabledMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	if _, err := repo.SetEnabled(ctx, "no-such-id", false); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("SetEnabled err = %v, want ErrNotFound", err)
	}
}

// 两个运行意图写路径都属于"调用方发起的资产变更"：刷新 UpdatedAt，但绝不递增 Revision。
// 与 UpdateStatus（系统心跳）相反——那里的用例断言 UpdatedAt 保持不变。
func TestServerRepositoryRuntimeIntentRefreshesUpdatedAt(t *testing.T) {
	repo, ctx := newTestRepo(t)
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	repo.WithClock(now)

	if err := repo.Create(ctx, remoteServer()); err != nil {
		t.Fatalf("create: %v", err)
	}

	advance(time.Minute)
	disabled, err := repo.SetEnabled(ctx, "srv-1", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !disabled.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("UpdatedAt after SetEnabled = %v, want %v", disabled.UpdatedAt, t0.Add(time.Minute))
	}
	if disabled.Revision != 1 {
		t.Errorf("Revision after SetEnabled = %d, want 1", disabled.Revision)
	}

	advance(time.Minute)
	stopped, err := repo.SetDesiredState(ctx, "srv-1", server.DesiredStopped)
	if err != nil {
		t.Fatalf("set desired state: %v", err)
	}
	if !stopped.UpdatedAt.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("UpdatedAt after SetDesiredState = %v, want %v", stopped.UpdatedAt, t0.Add(2*time.Minute))
	}
	if stopped.Revision != 1 {
		t.Errorf("Revision after SetDesiredState = %d, want 1", stopped.Revision)
	}
	if !stopped.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want %v", stopped.CreatedAt, t0)
	}
}

func TestServerRepositorySetDesiredStateMissing(t *testing.T) {
	repo, ctx := newTestRepo(t)

	if _, err := repo.SetDesiredState(ctx, "no-such-id", server.DesiredStopped); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("SetDesiredState err = %v, want ErrNotFound", err)
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
