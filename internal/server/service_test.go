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

func newTestService(repo *fakeRepo) (*Service, time.Time) {
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
