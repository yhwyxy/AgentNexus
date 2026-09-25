package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeRepo 只记录 Create 调用；其余方法保持 nil 接口，误调用会 panic，
// 从而暴露 Register 越出"构造 → 校验 → 持久化"的编排边界。
type fakeRepo struct {
	Repository
	created   []Server
	createErr error
}

func (f *fakeRepo) Create(_ context.Context, s Server) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, s)
	return nil
}

func remoteInput() RegisterInput {
	return RegisterInput{
		Name:      "weather",
		Transport: TransportStreamableHTTP,
		Runtime: RuntimeSpec{
			Type:   RuntimeRemote,
			Remote: &RemoteSpec{Endpoint: "http://weather-mcp:8080/mcp"},
		},
	}
}

func newTestService(repo Repository) (*Service, time.Time) {
	t0 := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	svc := NewService(repo).
		WithClock(func() time.Time { return t0 }).
		WithIDGenerator(func() string { return "id-1" })
	return svc, t0
}

func TestServiceRegisterAppliesDefaults(t *testing.T) {
	repo := &fakeRepo{}
	svc, t0 := newTestService(repo)

	got, err := svc.Register(context.Background(), remoteInput())
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	want := Server{
		ID:        "id-1",
		Namespace: "default",
		Name:      "weather",
		Labels:    map[string]string{},
		Enabled:   true,
		Revision:  1,
		Spec: Spec{
			Transport: TransportStreamableHTTP,
			Runtime: RuntimeSpec{
				Type:   RuntimeRemote,
				Remote: &RemoteSpec{Endpoint: "http://weather-mcp:8080/mcp"},
			},
			Timeouts: TimeoutSpec{
				Connect: 5 * time.Second,
				List:    10 * time.Second,
				Call:    60 * time.Second,
			},
			Limits:       LimitSpec{MaxInFlight: 16},
			DesiredState: DesiredRunning,
		},
		Status:    Status{Phase: PhasePending},
		CreatedAt: t0,
		UpdatedAt: t0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Register() =\n%+v\nwant\n%+v", got, want)
	}

	if len(repo.created) != 1 {
		t.Fatalf("Create called %d times, want 1", len(repo.created))
	}
	if !reflect.DeepEqual(repo.created[0], got) {
		t.Errorf("persisted server differs from returned server:\n%+v\n%+v", repo.created[0], got)
	}
}

func TestServiceRegisterKeepsExplicitValues(t *testing.T) {
	repo := &fakeRepo{}
	svc, _ := newTestService(repo)

	disabled := false
	cred := "cred-1"
	in := remoteInput()
	in.Namespace = "team-a"
	in.DisplayName = "Weather MCP"
	in.Description = "Weather tools"
	in.Labels = map[string]string{"category": "utility"}
	in.Enabled = &disabled
	in.CredentialID = &cred
	in.Timeouts = TimeoutSpec{Connect: time.Second, List: 2 * time.Second, Call: 3 * time.Second}
	in.Limits = LimitSpec{MaxInFlight: 4}
	in.DesiredState = DesiredStopped

	got, err := svc.Register(context.Background(), in)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if got.Namespace != "team-a" {
		t.Errorf("Namespace = %q, want %q", got.Namespace, "team-a")
	}
	if got.DisplayName != "Weather MCP" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Weather MCP")
	}
	if got.Description != "Weather tools" {
		t.Errorf("Description = %q, want %q", got.Description, "Weather tools")
	}
	if !reflect.DeepEqual(got.Labels, map[string]string{"category": "utility"}) {
		t.Errorf("Labels = %v, want %v", got.Labels, map[string]string{"category": "utility"})
	}
	if got.Enabled {
		t.Error("Enabled = true, want explicit false to be kept")
	}
	if got.Spec.CredentialID == nil || *got.Spec.CredentialID != "cred-1" {
		t.Errorf("CredentialID = %v, want %q", got.Spec.CredentialID, "cred-1")
	}
	wantTimeouts := TimeoutSpec{Connect: time.Second, List: 2 * time.Second, Call: 3 * time.Second}
	if got.Spec.Timeouts != wantTimeouts {
		t.Errorf("Timeouts = %+v, want %+v", got.Spec.Timeouts, wantTimeouts)
	}
	if got.Spec.Limits.MaxInFlight != 4 {
		t.Errorf("MaxInFlight = %d, want 4", got.Spec.Limits.MaxInFlight)
	}
	if got.Spec.DesiredState != DesiredStopped {
		t.Errorf("DesiredState = %q, want %q", got.Spec.DesiredState, DesiredStopped)
	}
}

func TestServiceRegisterRejectsInvalidInput(t *testing.T) {
	repo := &fakeRepo{}
	svc, _ := newTestService(repo)

	in := remoteInput()
	in.Name = "Weather_Server"

	_, err := svc.Register(context.Background(), in)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register err = %v, want ErrInvalid", err)
	}
	if len(repo.created) != 0 {
		t.Errorf("invalid server was persisted: %+v", repo.created)
	}
}

func TestServiceRegisterPropagatesRepositoryError(t *testing.T) {
	repo := &fakeRepo{createErr: ErrAlreadyExists}
	svc, _ := newTestService(repo)

	_, err := svc.Register(context.Background(), remoteInput())
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Register err = %v, want ErrAlreadyExists", err)
	}
}

func TestServiceRegisterNormalizesTimestampsToUTC(t *testing.T) {
	repo := &fakeRepo{}
	local := time.FixedZone("UTC+8", 8*60*60)
	svc := NewService(repo).
		WithClock(func() time.Time { return time.Date(2026, 9, 22, 16, 0, 0, 0, local) }).
		WithIDGenerator(func() string { return "id-1" })

	got, err := svc.Register(context.Background(), remoteInput())
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// 存储层统一按 UTC 写入并读回；服务层返回值必须使用同一表示，
	// 否则注册响应与后续查询会把同一时刻渲染成两种字符串。
	const want = "2026-09-22T08:00:00Z"
	if s := got.CreatedAt.Format(time.RFC3339Nano); s != want {
		t.Errorf("CreatedAt = %s, want %s", s, want)
	}
	if s := got.UpdatedAt.Format(time.RFC3339Nano); s != want {
		t.Errorf("UpdatedAt = %s, want %s", s, want)
	}
}

func TestServiceRegisterDefaultGenerators(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)

	first, err := svc.Register(context.Background(), remoteInput())
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	second, err := svc.Register(context.Background(), remoteInput())
	if err != nil {
		t.Fatalf("second register: %v", err)
	}

	if first.ID == "" || second.ID == "" {
		t.Errorf("IDs = %q, %q; want non-empty", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Errorf("both registrations got ID %q; want distinct IDs", first.ID)
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v / %v; want set by default clock", first.CreatedAt, first.UpdatedAt)
	}
}

// stubRepo 记录 Update/SetEnabled/SetDesiredState 的调用，并按存储层语义把
// UpdateInput 应用到当前快照上返回；其余方法保持 nil 接口，误调用会 panic，
// 从而暴露 Update 越出"读取 → 校验 → 持久化"的编排边界。
type stubRepo struct {
	Repository
	current   Server
	getErr    error
	updateErr error
	writeErr  error

	updated       []UpdateInput
	enabledWrites []bool
	desiredWrites []DesiredState
}

func (r *stubRepo) GetByID(_ context.Context, _ ID) (Server, error) {
	if r.getErr != nil {
		return Server{}, r.getErr
	}
	return r.current, nil
}

func (r *stubRepo) Update(_ context.Context, _ ID, _ int64, in UpdateInput) (Server, error) {
	r.updated = append(r.updated, in)
	if r.updateErr != nil {
		return Server{}, r.updateErr
	}
	next := r.current
	next.DisplayName = in.DisplayName
	next.Description = in.Description
	next.Labels = in.Labels
	next.Spec.Runtime = in.Runtime
	next.Spec.CredentialID = in.CredentialID
	next.Spec.Timeouts = in.Timeouts
	next.Spec.Limits = in.Limits
	next.Revision++
	return next, nil
}

func (r *stubRepo) SetEnabled(_ context.Context, _ ID, enabled bool) (Server, error) {
	r.enabledWrites = append(r.enabledWrites, enabled)
	if r.writeErr != nil {
		return Server{}, r.writeErr
	}
	next := r.current
	next.Enabled = enabled
	return next, nil
}

func (r *stubRepo) SetDesiredState(_ context.Context, _ ID, state DesiredState) (Server, error) {
	r.desiredWrites = append(r.desiredWrites, state)
	if r.writeErr != nil {
		return Server{}, r.writeErr
	}
	next := r.current
	next.Spec.DesiredState = state
	return next, nil
}

// stubPolicy 记录宿主机资源策略的判定次数，并按测试需要返回错误。
type stubPolicy struct {
	err    error
	called int
}

func (p *stubPolicy) CheckDocker(DockerSpec) error {
	p.called++
	return p.err
}

// stubCurrent 是 Update 测试的当前快照：transport=streamable_http、
// desiredState=stopped，用于验证 Update 不会改写这两个不可变字段。
func stubCurrent() Server {
	return Server{
		ID:        "id-1",
		Namespace: "default",
		Name:      "weather",
		Enabled:   true,
		Revision:  3,
		Spec: Spec{
			Transport:    TransportStreamableHTTP,
			Runtime:      RuntimeSpec{Type: RuntimeRemote, Remote: &RemoteSpec{Endpoint: "http://old:8080/mcp"}},
			Timeouts:     TimeoutSpec{Connect: time.Second, List: 2 * time.Second, Call: 3 * time.Second},
			Limits:       LimitSpec{MaxInFlight: 4},
			DesiredState: DesiredStopped,
		},
	}
}

func TestServiceUpdateAppliesDefaultsAndKeepsImmutableFields(t *testing.T) {
	current := stubCurrent()
	repo := &stubRepo{current: current}
	svc, _ := newTestService(repo)

	got, err := svc.Update(context.Background(), "id-1", 3, UpdateInput{
		DisplayName: "Weather v2",
		Runtime: RuntimeSpec{
			Type:   RuntimeRemote,
			Remote: &RemoteSpec{Endpoint: "http://new:8080/mcp"},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(repo.updated) != 1 {
		t.Fatalf("Update called %d times, want 1", len(repo.updated))
	}
	in := repo.updated[0]
	// 零值超时/限流回落到与 Register 相同的冻结默认值。
	wantTimeouts := TimeoutSpec{Connect: 5 * time.Second, List: 10 * time.Second, Call: 60 * time.Second}
	if in.Timeouts != wantTimeouts {
		t.Errorf("forwarded Timeouts = %+v, want %+v", in.Timeouts, wantTimeouts)
	}
	if in.Limits.MaxInFlight != 16 {
		t.Errorf("forwarded MaxInFlight = %d, want 16", in.Limits.MaxInFlight)
	}

	// 不可变字段来自当前快照，Update 不得改写。
	if got.Namespace != "default" || got.Name != "weather" {
		t.Errorf("Namespace/Name = %q/%q, want default/weather", got.Namespace, got.Name)
	}
	if got.Spec.Transport != TransportStreamableHTTP {
		t.Errorf("Transport = %q, want unchanged %q", got.Spec.Transport, TransportStreamableHTTP)
	}
	if got.Spec.DesiredState != DesiredStopped {
		t.Errorf("DesiredState = %q, want unchanged %q", got.Spec.DesiredState, DesiredStopped)
	}
	if got.Revision != current.Revision+1 {
		t.Errorf("Revision = %d, want %d", got.Revision, current.Revision+1)
	}
}

func TestServiceUpdateRejectsInvalidRuntimeWithoutWriting(t *testing.T) {
	repo := &stubRepo{current: stubCurrent()}
	svc, _ := newTestService(repo)

	// 既没有 remote 也没有 process/docker 载荷，Validate 必然失败。
	_, err := svc.Update(context.Background(), "id-1", 3, UpdateInput{
		Runtime: RuntimeSpec{Type: RuntimeRemote},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update err = %v, want ErrInvalid", err)
	}
	if len(repo.updated) != 0 {
		t.Errorf("invalid update was persisted: %+v", repo.updated)
	}
}

func TestServiceUpdateRunsHostResourcePolicy(t *testing.T) {
	repo := &stubRepo{current: stubCurrent()}
	policy := &stubPolicy{err: errors.New("mount denied")}
	svc, _ := newTestService(repo)
	svc.WithPolicy(policy)

	_, err := svc.Update(context.Background(), "id-1", 3, UpdateInput{
		Runtime: RuntimeSpec{
			Type:   RuntimeDocker,
			Docker: &DockerSpec{Image: "weather:2", Port: 8080},
		},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update err = %v, want ErrInvalid", err)
	}
	if policy.called != 1 {
		t.Errorf("policy called %d times, want 1", policy.called)
	}
	if len(repo.updated) != 0 {
		t.Errorf("policy-rejected update was persisted: %+v", repo.updated)
	}
}

func TestServiceUpdatePropagatesConflict(t *testing.T) {
	repo := &stubRepo{current: stubCurrent(), updateErr: ErrConflict}
	svc, _ := newTestService(repo)

	got, err := svc.Update(context.Background(), "id-1", 3, UpdateInput{
		Runtime: RuntimeSpec{Type: RuntimeRemote, Remote: &RemoteSpec{Endpoint: "http://new:8080/mcp"}},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update err = %v, want ErrConflict", err)
	}
	if !reflect.DeepEqual(got, Server{}) {
		t.Errorf("Update returned %+v on conflict, want zero Server", got)
	}
}

func TestServiceSetEnabledAndDesiredStateAreIdempotent(t *testing.T) {
	t.Run("enabled already matches", func(t *testing.T) {
		current := stubCurrent() // Enabled=true
		repo := &stubRepo{current: current}
		svc, _ := newTestService(repo)

		got, err := svc.SetEnabled(context.Background(), "id-1", true)
		if err != nil {
			t.Fatalf("set enabled: %v", err)
		}
		if len(repo.enabledWrites) != 0 {
			t.Errorf("write called %d times for unchanged value, want 0", len(repo.enabledWrites))
		}
		if !reflect.DeepEqual(got, current) {
			t.Errorf("returned %+v, want current snapshot %+v", got, current)
		}
	})

	t.Run("desired state already matches", func(t *testing.T) {
		current := stubCurrent()
		current.Spec.DesiredState = DesiredRunning
		repo := &stubRepo{current: current}
		svc, _ := newTestService(repo)

		got, err := svc.SetDesiredState(context.Background(), "id-1", DesiredRunning)
		if err != nil {
			t.Fatalf("set desired state: %v", err)
		}
		if len(repo.desiredWrites) != 0 {
			t.Errorf("write called %d times for unchanged value, want 0", len(repo.desiredWrites))
		}
		if !reflect.DeepEqual(got, current) {
			t.Errorf("returned %+v, want current snapshot %+v", got, current)
		}
	})

	t.Run("changed value writes once", func(t *testing.T) {
		repo := &stubRepo{current: stubCurrent()}
		svc, _ := newTestService(repo)

		if _, err := svc.SetEnabled(context.Background(), "id-1", false); err != nil {
			t.Fatalf("set enabled: %v", err)
		}
		if len(repo.enabledWrites) != 1 || repo.enabledWrites[0] {
			t.Errorf("enabledWrites = %v, want [false]", repo.enabledWrites)
		}

		if _, err := svc.SetDesiredState(context.Background(), "id-1", DesiredRunning); err != nil {
			t.Fatalf("set desired state: %v", err)
		}
		if len(repo.desiredWrites) != 1 || repo.desiredWrites[0] != DesiredRunning {
			t.Errorf("desiredWrites = %v, want [running]", repo.desiredWrites)
		}
	})
}
