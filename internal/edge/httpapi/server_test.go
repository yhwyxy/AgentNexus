package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/auth"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/runtime/hostaccess"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// 测试密钥：与 configs 里的示例同形，但值只存在于测试进程内。
const (
	testAdminSecret = "test-admin-secret"
	testAgentSecret = "test-agent-secret"
)

// testAuthorizer 与生产装配共用同一套默认策略，只替换密钥表，因此这里断言的行为
// 就是线上策略的行为。
func testAuthorizer(t *testing.T) *auth.Authorizer {
	t.Helper()
	authorizer, err := auth.NewAuthorizer([]auth.KeyConfig{
		{Name: "test-admin", Role: auth.RoleAdmin, Secret: testAdminSecret},
		{Name: "test-agent", Role: auth.RoleAgent, Secret: testAgentSecret},
	}, auth.DefaultPolicy())
	if err != nil {
		t.Fatalf("build test authorizer: %v", err)
	}
	return authorizer
}

// newTestHandler 用真实 Service + 真实 SQLite 组装 HTTP handler，
// 时钟与 ID 固定，使响应 JSON 可与字面量逐字段比对。
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWithService(t, nil)
}

// newTestHandlerWithService 允许用例在注册服上追加装配（例如注入宿主机访问策略）。
func newTestHandlerWithService(t *testing.T, configure func(*server.Service) *server.Service) http.Handler {
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

	t0 := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	n := 0
	registry := server.NewService(sqlite.NewServerRepository(db)).
		WithClock(func() time.Time { return t0 }).
		WithIDGenerator(func() string { n++; return fmt.Sprintf("srv-%d", n) })
	if configure != nil {
		registry = configure(registry)
	}

	return httpapi.NewHandler(httpapi.Options{
		Registry:      registry,
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// do 默认以 admin 身份发起请求：注册/查询/动作用例都应当通过认证。
// 认证相关的断言改用 doRequest 自行决定凭证。
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, method, path, body, map[string]string{
		"Authorization": "Bearer " + testAdminSecret,
	})
}

func doRequest(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("expected literal is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string
			Message string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, rec.Body)
	}
	if body.Error.Code != want {
		t.Errorf("error.code = %q, want %q; body: %s", body.Error.Code, want, rec.Body)
	}
	if body.Error.Message == "" {
		t.Error("error.message is empty")
	}
}

// 详细设计 3.2 的 Remote MCP 注册示例。
const registerWeather = `{
  "namespace": "default",
  "name": "weather",
  "displayName": "Weather MCP",
  "description": "Weather tools",
  "labels": {"category": "utility"},
  "enabled": true,
  "transport": "streamable_http",
  "runtime": {
    "type": "remote",
    "remote": {"endpoint": "http://weather-mcp:8080/mcp"}
  },
  "timeouts": {"connectSeconds": 5, "listSeconds": 10, "callSeconds": 60},
  "limits": {"maxInFlight": 16}
}`

const weatherResponse = `{
  "id": "srv-1",
  "namespace": "default",
  "name": "weather",
  "displayName": "Weather MCP",
  "description": "Weather tools",
  "labels": {"category": "utility"},
  "enabled": true,
  "revision": 1,
  "spec": {
    "transport": "streamable_http",
    "runtime": {
      "type": "remote",
      "remote": {"endpoint": "http://weather-mcp:8080/mcp"}
    },
    "credentialId": null,
    "timeouts": {"connectSeconds": 5, "listSeconds": 10, "callSeconds": 60},
    "limits": {"maxInFlight": 16},
    "desiredState": "running"
  },
  "status": {
    "phase": "pending",
    "message": "",
    "observedRevision": 0,
    "lastHealthAt": null,
    "lastSuccessAt": null,
    "consecutiveFailures": 0
  },
  "createdAt": "2026-09-22T08:00:00Z",
  "updatedAt": "2026-09-22T08:00:00Z",
  "links": {"self": "/api/v1/mcp-servers/srv-1"}
}`

const minimalWeather = `{
  "name": "weather",
  "transport": "streamable_http",
  "runtime": {"type": "remote", "remote": {"endpoint": "http://weather-mcp:8080/mcp"}}
}`

func TestRegisterThenGet(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", registerWeather)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/mcp-servers/srv-1" {
		t.Errorf("Location = %q, want /api/v1/mcp-servers/srv-1", loc)
	}
	assertJSONEqual(t, rec.Body.Bytes(), weatherResponse)

	rec = do(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	assertJSONEqual(t, rec.Body.Bytes(), weatherResponse)
}

func TestRegisterMinimalBodyAppliesDefaults(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	var got struct {
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
		Enabled   bool              `json:"enabled"`
		Spec      struct {
			Timeouts struct {
				ConnectSeconds int `json:"connectSeconds"`
				ListSeconds    int `json:"listSeconds"`
				CallSeconds    int `json:"callSeconds"`
			} `json:"timeouts"`
			Limits struct {
				MaxInFlight int `json:"maxInFlight"`
			} `json:"limits"`
			DesiredState string `json:"desiredState"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Namespace != "default" {
		t.Errorf("namespace = %q, want default", got.Namespace)
	}
	if got.Labels == nil || len(got.Labels) != 0 {
		t.Errorf("labels = %v, want empty object", got.Labels)
	}
	if !got.Enabled {
		t.Error("enabled = false, want true")
	}
	if got.Spec.Timeouts.ConnectSeconds != 5 || got.Spec.Timeouts.ListSeconds != 10 || got.Spec.Timeouts.CallSeconds != 60 {
		t.Errorf("timeouts = %+v, want 5/10/60", got.Spec.Timeouts)
	}
	if got.Spec.Limits.MaxInFlight != 16 {
		t.Errorf("maxInFlight = %d, want 16", got.Spec.Limits.MaxInFlight)
	}
	if got.Spec.DesiredState != "running" {
		t.Errorf("desiredState = %q, want running", got.Spec.DesiredState)
	}
}

func TestRegisterRuntimeVariantsRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantRuntime string
	}{
		{
			name: "remote with headers",
			body: `{"name":"weather","transport":"streamable_http","runtime":{"type":"remote",
				"remote":{"endpoint":"http://weather-mcp:8080/mcp","headers":{"X-Env":"test"}}}}`,
			wantRuntime: `{"type":"remote","remote":{"endpoint":"http://weather-mcp:8080/mcp","headers":{"X-Env":"test"}}}`,
		},
		{
			name: "process",
			body: `{"name":"filesystem","transport":"stdio","runtime":{"type":"process",
				"process":{"command":"/usr/local/bin/filesystem-mcp","args":["/workspace"],
				"workingDir":"/var/lib/agentnexus/runtimes/filesystem"}}}`,
			wantRuntime: `{"type":"process","process":{"command":"/usr/local/bin/filesystem-mcp",
				"args":["/workspace"],"workingDir":"/var/lib/agentnexus/runtimes/filesystem"}}`,
		},
		{
			name: "docker",
			body: `{"name":"fs","transport":"stdio","runtime":{"type":"docker",
				"docker":{"image":"ghcr.io/example/fs:1","command":["fs","--root","/w"],"env":{"LOG":"info"},
				"mounts":[{"source":"/srv/data","target":"/w","readOnly":true}],
				"networkMode":"none","memoryBytes":268435456,"cpus":0.5}}}`,
			wantRuntime: `{"type":"docker","docker":{"image":"ghcr.io/example/fs:1","command":["fs","--root","/w"],
				"env":{"LOG":"info"},"mounts":[{"source":"/srv/data","target":"/w","readOnly":true}],
				"networkMode":"none","memoryBytes":268435456,"cpus":0.5}}`,
		},
		{
			name: "docker streamable http",
			body: `{"name":"fs-http","transport":"streamable_http","runtime":{"type":"docker",
				"docker":{"image":"ghcr.io/example/fs:1","port":8080}}}`,
			// endpointPath 未提供时按默认 /mcp 落库。
			wantRuntime: `{"type":"docker","docker":{"image":"ghcr.io/example/fs:1",
				"memoryBytes":268435456,"cpus":1,"port":8080,"endpointPath":"/mcp"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(t)

			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", tt.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("POST status = %d, want 202; body: %s", rec.Code, rec.Body)
			}

			rec = do(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want 200; body: %s", rec.Code, rec.Body)
			}
			var got struct {
				Spec struct {
					Runtime json.RawMessage `json:"runtime"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertJSONEqual(t, got.Spec.Runtime, tt.wantRuntime)
		})
	}
}

// 宿主机挂载策略在注册期生效：允许清单外的路径必须得到 400 invalid_argument，
// 清单内但越权写、越界落地同样被拒绝。
func TestRegisterAppliesHostAccessPolicy(t *testing.T) {
	root := t.TempDir()
	policy, err := hostaccess.NewPolicy([]hostaccess.Entry{
		{Host: root, Container: "/workspace", AllowWrite: false},
	})
	if err != nil {
		t.Fatalf("build policy: %v", err)
	}
	h := newTestHandlerWithService(t, func(s *server.Service) *server.Service {
		return s.WithPolicy(policy)
	})

	body := func(name string, mounts []map[string]any) string {
		encoded, err := json.Marshal(map[string]any{
			"name": name, "transport": "stdio",
			"runtime": map[string]any{
				"type":   "docker",
				"docker": map[string]any{"image": "ghcr.io/example/fs:1", "mounts": mounts},
			},
		})
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		return string(encoded)
	}

	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", body("allowed", []map[string]any{
		{"source": filepath.Join(root, "data"), "target": "/workspace/data", "readOnly": true},
	}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("allowed mount status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	rejected := map[string][]map[string]any{
		"outside allowed roots": {{"source": "/srv/elsewhere", "target": "/workspace/data", "readOnly": true}},
		"write on read only":    {{"source": filepath.Join(root, "data"), "target": "/workspace/data"}},
		"target outside root":   {{"source": filepath.Join(root, "data"), "target": "/data", "readOnly": true}},
	}
	for name, mounts := range rejected {
		t.Run(name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", body("denied", mounts))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "invalid_argument")
		})
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{"name":`},
		{"unknown field", `{"name":"weather","transport":"streamable_http",
			"runtime":{"type":"remote","remote":{"endpoint":"http://x/mcp"}},"bogus":1}`},
		{"invalid name", `{"name":"Weather_Server","transport":"streamable_http",
			"runtime":{"type":"remote","remote":{"endpoint":"http://x/mcp"}}}`},
		{"unsupported transport", `{"name":"weather","transport":"grpc",
			"runtime":{"type":"remote","remote":{"endpoint":"http://x/mcp"}}}`},
		{"relative process command", `{"name":"fs","transport":"stdio",
			"runtime":{"type":"process","process":{"command":"bin/mcp"}}}`},
		{"call timeout above limit", `{"name":"weather","transport":"streamable_http",
			"runtime":{"type":"remote","remote":{"endpoint":"http://x/mcp"}},"timeouts":{"callSeconds":301}}`},
	}

	h := newTestHandler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "invalid_argument")
		})
	}
}

func TestRegisterDuplicateConflict(t *testing.T) {
	h := newTestHandler(t)

	if rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather); rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409; body: %s", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "conflict")
}

func TestGetMissingNotFound(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/api/v1/mcp-servers/ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "not_found")
}

type failingRegistry struct{ err error }

func (f failingRegistry) Register(context.Context, server.RegisterInput) (server.Server, error) {
	return server.Server{}, f.err
}

func (f failingRegistry) Get(context.Context, server.ID) (server.Server, error) {
	return server.Server{}, f.err
}

func (f failingRegistry) List(context.Context) ([]server.Server, error) {
	return nil, f.err
}

func (f failingRegistry) Update(context.Context, server.ID, int64, server.UpdateInput) (server.Server, error) {
	return server.Server{}, f.err
}

func TestUnexpectedErrorIsOpaqueAndLogged(t *testing.T) {
	var logs bytes.Buffer
	h := httpapi.NewHandler(httpapi.Options{
		Registry:      failingRegistry{err: errors.New("boom: /var/lib/secret")},
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(&logs, nil)),
	})

	tests := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/mcp-servers", minimalWeather},
		{http.MethodGet, "/api/v1/mcp-servers/srv-1", ""},
		{http.MethodGet, "/api/v1/mcp-servers", ""},
		{http.MethodPut, "/api/v1/mcp-servers/srv-1", updateBody(1, "Weather", nil)},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			logs.Reset()

			rec := do(t, h, tt.method, tt.path, tt.body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "internal")
			if strings.Contains(rec.Body.String(), "boom") {
				t.Errorf("internal error detail leaked to client: %s", rec.Body)
			}
			if !strings.Contains(logs.String(), "boom") {
				t.Errorf("internal error was not logged; log output: %q", logs.String())
			}
		})
	}
}

func TestHealthEndpoints(t *testing.T) {
	h := newTestHandler(t)

	for _, path := range []string{"/health/live", "/health/ready"} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
		assertJSONEqual(t, rec.Body.Bytes(), `{"status":"ok"}`)
	}
}

// fakeRefresher 记录被请求刷新的 ID，并可注入领域错误。
type fakeRefresher struct {
	srv  server.Server
	err  error
	gets []server.ID
}

func (f *fakeRefresher) RefreshTools(_ context.Context, id server.ID) (server.Server, error) {
	f.gets = append(f.gets, id)
	if f.err != nil {
		return server.Server{}, f.err
	}
	return f.srv, nil
}

func refreshableServer() server.Server {
	return server.Server{
		ID:        "srv-1",
		Namespace: "default",
		Name:      "weather",
		Enabled:   true,
		Revision:  1,
		Spec: server.Spec{
			Transport: server.TransportStreamableHTTP,
			Runtime: server.RuntimeSpec{
				Type:   server.RuntimeRemote,
				Remote: &server.RemoteSpec{Endpoint: "http://weather-mcp:8080/mcp"},
			},
			Timeouts:     server.TimeoutSpec{Connect: 5 * time.Second, List: 10 * time.Second, Call: 60 * time.Second},
			Limits:       server.LimitSpec{MaxInFlight: 16},
			DesiredState: server.DesiredRunning,
		},
		Status: server.Status{Phase: server.PhaseReady, ObservedRevision: 1},
	}
}

func newRefreshHandler(t *testing.T, refresher httpapi.ServerRefresher) http.Handler {
	t.Helper()

	return httpapi.NewHandler(httpapi.Options{
		Registry:      failingRegistry{err: errors.New("unused")},
		Refresher:     refresher,
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// 动作路由以 202 接受：真正的拉取由后台完成，调用方通过 GET 观察状态。
func TestRefreshToolsAccepted(t *testing.T) {
	refresher := &fakeRefresher{srv: refreshableServer()}
	h := newRefreshHandler(t, refresher)

	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers/srv-1:refresh-tools", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/mcp-servers/srv-1" {
		t.Errorf("Location = %q, want the server self link", got)
	}
	assertJSONEqual(t, rec.Body.Bytes(), `{
		"id": "srv-1",
		"namespace": "default",
		"name": "weather",
		"displayName": "",
		"description": "",
		"labels": {},
		"enabled": true,
		"revision": 1,
		"spec": {
			"transport": "streamable_http",
			"runtime": {"type": "remote", "remote": {"endpoint": "http://weather-mcp:8080/mcp"}},
			"credentialId": null,
			"timeouts": {"connectSeconds": 5, "listSeconds": 10, "callSeconds": 60},
			"limits": {"maxInFlight": 16},
			"desiredState": "running"
		},
		"status": {
			"phase": "ready",
			"message": "",
			"observedRevision": 1,
			"lastHealthAt": null,
			"lastSuccessAt": null,
			"consecutiveFailures": 0
		},
		"createdAt": "0001-01-01T00:00:00Z",
		"updatedAt": "0001-01-01T00:00:00Z",
		"links": {"self": "/api/v1/mcp-servers/srv-1"}
	}`)
	if len(refresher.gets) != 1 || refresher.gets[0] != "srv-1" {
		t.Errorf("refresher calls = %v, want [srv-1]", refresher.gets)
	}
}

func TestRefreshToolsDomainErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"missing server", server.ErrNotFound, http.StatusNotFound, "not_found"},
		{"not runnable", fmt.Errorf("%w: disabled", server.ErrNotRunnable), http.StatusConflict, "conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refresher := &fakeRefresher{err: tt.err}
			h := newRefreshHandler(t, refresher)

			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers/srv-1:refresh-tools", "")
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body)
			}
			assertErrorCode(t, rec, tt.wantBody)
		})
	}
}

func TestActionRouteRejectsUnknownShapes(t *testing.T) {
	tests := []struct{ name, path string }{
		{"unknown action", "/api/v1/mcp-servers/srv-1:restart"},
		{"missing action", "/api/v1/mcp-servers/srv-1:"},
		{"no separator", "/api/v1/mcp-servers/srv-1"},
		{"empty id", "/api/v1/mcp-servers/:refresh-tools"},
		{"deeper path", "/api/v1/mcp-servers/a/b:refresh-tools"},
		{"collection", "/api/v1/mcp-servers/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refresher := &fakeRefresher{srv: refreshableServer()}
			h := newRefreshHandler(t, refresher)

			rec := do(t, h, http.MethodPost, tt.path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "not_found")
			if len(refresher.gets) != 0 {
				t.Errorf("refresher calls = %v, want none", refresher.gets)
			}
		})
	}
}

func TestActionRouteAbsentWithoutRefresher(t *testing.T) {
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      failingRegistry{err: errors.New("unused")},
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// 未配置 Refresher 时不注册动作模式：该路径只匹配 GET /{id}，因此是 405 而非命中动作 handler。
	rec := do(t, handler, http.MethodPost, "/api/v1/mcp-servers/srv-1:refresh-tools", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 when no refresher is configured; body: %s", rec.Code, rec.Body)
	}
}

func TestManagementAPIRequiresCredentials(t *testing.T) {
	h := newTestHandler(t)

	tests := []struct{ name, method, path, body string }{
		{"register", http.MethodPost, "/api/v1/mcp-servers", registerWeather},
		{"get", http.MethodGet, "/api/v1/mcp-servers/srv-1", ""},
		{"list", http.MethodGet, "/api/v1/mcp-servers", ""},
		{"update", http.MethodPut, "/api/v1/mcp-servers/srv-1", updateBody(1, "Weather", nil)},
		{"action", http.MethodPost, "/api/v1/mcp-servers/srv-1:stop", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, h, tt.method, tt.path, tt.body, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="agentnexus"` {
				t.Errorf("WWW-Authenticate = %q, want the Bearer challenge", got)
			}
			assertErrorCode(t, rec, "unauthenticated")
		})
	}

	// 无凭证的注册请求不产生任何副作用：srv-1 仍然不存在。
	rec := do(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status after rejected register = %d, want 404 (no row written); body: %s", rec.Code, rec.Body)
	}
}

func TestAgentCredentialsAreDeniedOnManagementAPI(t *testing.T) {
	h := newTestHandler(t)

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer " + testAgentSecret}},
		{"x-api-key", map[string]string{"X-API-Key": testAgentSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodPost, "/api/v1/mcp-servers", registerWeather, tt.headers)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Errorf("WWW-Authenticate = %q, want none on 403", got)
			}
			assertErrorCode(t, rec, "permission_denied")
		})
	}
}

func TestUnknownCredentialsAndUnregisteredPaths(t *testing.T) {
	h := newTestHandler(t)

	rec := doRequest(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "", map[string]string{
		"Authorization": "Bearer not-a-key",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key status = %d, want 401; body: %s", rec.Code, rec.Body)
	}

	// 未登记策略的路径默认拒绝：连合法 admin 也进不去，且不会暴露路由是否存在。
	rec = doRequest(t, h, http.MethodGet, "/internal/debug", "", map[string]string{
		"Authorization": "Bearer " + testAdminSecret,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unregistered path with admin key status = %d, want 403; body: %s", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "permission_denied")

	rec = doRequest(t, h, http.MethodGet, "/internal/debug", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unregistered path without credentials status = %d, want 401; body: %s", rec.Code, rec.Body)
	}
}

func TestCredentialExtraction(t *testing.T) {
	h := newTestHandler(t)

	tests := []struct {
		name     string
		headers  map[string]string
		wantCode int
	}{
		{
			name:     "bearer scheme is case insensitive",
			headers:  map[string]string{"Authorization": "bearer " + testAdminSecret},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "x-api-key is accepted",
			headers:  map[string]string{"X-API-Key": testAdminSecret},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "surrounding whitespace is trimmed",
			headers:  map[string]string{"X-API-Key": "  " + testAdminSecret + "  "},
			wantCode: http.StatusNotFound,
		},
		{
			name: "non bearer scheme does not fall back to x-api-key",
			headers: map[string]string{
				"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+testAdminSecret)),
				"X-API-Key":     testAdminSecret,
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "bearer without token",
			headers:  map[string]string{"Authorization": "Bearer"},
			wantCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 认证通过时该请求落到 GET /{id}，因此 404 是"已认证"的证明。
			rec := doRequest(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "", tt.headers)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body)
			}
		})
	}
}

// 装配遗漏（nil Authenticator）失败关闭：连探针也不放行，避免"忘装配 = 裸网关"。
func TestAuthenticatorMissingFailsClosed(t *testing.T) {
	var logs bytes.Buffer
	h := httpapi.NewHandler(httpapi.Options{
		Registry: failingRegistry{err: errors.New("unused")},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})

	rec := doRequest(t, h, http.MethodGet, "/health/live", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no authenticator is wired; body: %s", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "permission_denied")
	if !strings.Contains(logs.String(), "no authenticator configured") {
		t.Errorf("missing authenticator was not logged; log output: %q", logs.String())
	}
}

// serverBody 是断言用的响应形状：只解出用例关心的字段，避免把整份 DTO 再抄一遍。
type serverBody struct {
	ID          string            `json:"id"`
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Labels      map[string]string `json:"labels"`
	Enabled     bool              `json:"enabled"`
	Revision    int64             `json:"revision"`
	Spec        struct {
		DesiredState string `json:"desiredState"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type listBody struct {
	Items []serverBody `json:"items"`
	Count int          `json:"count"`
}

// updateBody 构造 PUT 请求体：revision 是必填的乐观锁载体，runtime 必填，
// 其余可变字段由用例决定（labels 为 nil 时序列化成 null，等价于清空）。
func updateBody(revision int64, displayName string, labels map[string]string) string {
	body := map[string]any{
		"revision":    revision,
		"displayName": displayName,
		"labels":      labels,
		"runtime": map[string]any{
			"type":   "remote",
			"remote": map[string]any{"endpoint": "http://weather-mcp:8080/mcp"},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func assertErrorMessage(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, rec.Body)
	}
	if body.Error.Message != want {
		t.Errorf("error.message = %q, want %q", body.Error.Message, want)
	}
}

func decodeServerBody(t *testing.T, rec *httptest.ResponseRecorder) serverBody {
	t.Helper()

	var got serverBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body)
	}
	return got
}

// registerNamed 注册一个指定 namespace/name 的 Server，返回响应 ID。
func registerNamed(t *testing.T, h http.Handler, namespace, name string) string {
	t.Helper()

	body := fmt.Sprintf(`{"namespace":%q,"name":%q,"transport":"streamable_http",
		"runtime":{"type":"remote","remote":{"endpoint":"http://%s:8080/mcp"}}}`, namespace, name, name)
	rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register %s/%s status = %d, want 202; body: %s", namespace, name, rec.Code, rec.Body)
	}
	return decodeServerBody(t, rec).ID
}

// 空集合必须是 {"items":[],"count":0}：items 为 null 会让调用方多写一条分支。
func TestListServersEmpty(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/api/v1/mcp-servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	assertJSONEqual(t, rec.Body.Bytes(), `{"items":[],"count":0}`)
}

func TestListServersOrdersByNamespaceThenName(t *testing.T) {
	h := newTestHandler(t)

	// 注册顺序刻意与期望排序不同，避免"恰好按写入顺序返回"也能通过。
	registered := []struct{ namespace, name string }{
		{"default", "zulu"},
		{"team-a", "alpha"},
		{"default", "alpha"},
	}
	for _, reg := range registered {
		registerNamed(t, h, reg.namespace, reg.name)
	}

	rec := do(t, h, http.MethodGet, "/api/v1/mcp-servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}

	var got listBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v\n%s", err, rec.Body)
	}
	if got.Count != len(registered) || len(got.Items) != len(registered) {
		t.Fatalf("count = %d, items = %d, want %d", got.Count, len(got.Items), len(registered))
	}

	want := []struct{ namespace, name string }{
		{"default", "alpha"},
		{"default", "zulu"},
		{"team-a", "alpha"},
	}
	for i, w := range want {
		if got.Items[i].Namespace != w.namespace || got.Items[i].Name != w.name {
			t.Errorf("items[%d] = %s/%s, want %s/%s", i,
				got.Items[i].Namespace, got.Items[i].Name, w.namespace, w.name)
		}
	}
}

// PUT 全量替换可变配置并递增配置版本；响应与随后的 GET 必须一致（变更确实落库）。
func TestUpdateServerBumpsRevisionAndMetadata(t *testing.T) {
	h := newTestHandler(t)

	if rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather); rec.Code != http.StatusAccepted {
		t.Fatalf("register status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
	// 两次更新把配置版本推到 3，让"3 -> 4"这一跳成为断言目标。
	for revision := int64(1); revision <= 2; revision++ {
		rec := do(t, h, http.MethodPut, "/api/v1/mcp-servers/srv-1", updateBody(revision, "Weather", nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("PUT revision %d status = %d, want 202; body: %s", revision, rec.Code, rec.Body)
		}
	}

	rec := do(t, h, http.MethodPut, "/api/v1/mcp-servers/srv-1",
		updateBody(3, "Weather v2", map[string]string{"team": "platform"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/mcp-servers/srv-1" {
		t.Errorf("Location = %q, want the server self link", loc)
	}

	got := decodeServerBody(t, rec)
	if got.Revision != 4 {
		t.Errorf("revision = %d, want 4", got.Revision)
	}
	if got.DisplayName != "Weather v2" {
		t.Errorf("displayName = %q, want Weather v2", got.DisplayName)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("labels = %v, want team=platform", got.Labels)
	}
	// enabled/desiredState 有专责端点，PUT 不得触碰。
	if !got.Enabled || got.Spec.DesiredState != "running" {
		t.Errorf("enabled/desiredState = %t/%q, want true/running", got.Enabled, got.Spec.DesiredState)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	persisted := decodeServerBody(t, rec)
	if persisted.Revision != 4 || persisted.DisplayName != "Weather v2" || persisted.Labels["team"] != "platform" {
		t.Errorf("persisted server = %+v, want revision 4 with the updated metadata", persisted)
	}
}

func TestUpdateServerRejectsBadRequests(t *testing.T) {
	const (
		validRuntime = `"runtime":{"type":"remote","remote":{"endpoint":"http://weather-mcp:8080/mcp"}}`
	)

	tests := []struct {
		name        string
		path        string
		body        string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name: "missing revision", path: "/api/v1/mcp-servers/srv-1",
			body:       `{` + validRuntime + `}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument", wantMessage: "revision is required",
		},
		{
			name: "zero revision", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":0,` + validRuntime + `}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument", wantMessage: "revision is required",
		},
		{
			name: "stale revision", path: "/api/v1/mcp-servers/srv-1",
			body:       updateBody(7, "Weather", nil),
			wantStatus: http.StatusConflict, wantCode: "conflict",
		},
		{
			name: "unknown field enabled", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":1,` + validRuntime + `,"enabled":true}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument",
		},
		{
			name: "unknown field desiredState", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":1,` + validRuntime + `,"desiredState":"stopped"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument",
		},
		{
			name: "missing runtime", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument", wantMessage: "runtime is required",
		},
		{
			name: "unknown id", path: "/api/v1/mcp-servers/ghost",
			body:       updateBody(1, "Weather", nil),
			wantStatus: http.StatusNotFound, wantCode: "not_found",
		},
		{
			name: "malformed json", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument",
		},
		{
			name: "invalid runtime shape", path: "/api/v1/mcp-servers/srv-1",
			body:       `{"revision":1,"runtime":{"type":"remote"}}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(t)
			if rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather); rec.Code != http.StatusAccepted {
				t.Fatalf("register status = %d, want 202; body: %s", rec.Code, rec.Body)
			}

			rec := do(t, h, http.MethodPut, tt.path, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			assertErrorCode(t, rec, tt.wantCode)
			if tt.wantMessage != "" {
				assertErrorMessage(t, rec, tt.wantMessage)
			}
		})
	}
}

// fakeLifecycle 记录动作调用并可注入领域错误；srv 是动作成功时返回的快照。
type fakeLifecycle struct {
	srv   server.Server
	err   error
	calls []string
}

func (f *fakeLifecycle) SetEnabled(_ context.Context, id server.ID, enabled bool) (server.Server, error) {
	f.calls = append(f.calls, fmt.Sprintf("set-enabled:%s:%t", id, enabled))
	return f.result()
}

func (f *fakeLifecycle) Start(_ context.Context, id server.ID) (server.Server, error) {
	f.calls = append(f.calls, "start:"+string(id))
	return f.result()
}

func (f *fakeLifecycle) Stop(_ context.Context, id server.ID) (server.Server, error) {
	f.calls = append(f.calls, "stop:"+string(id))
	return f.result()
}

func (f *fakeLifecycle) Restart(_ context.Context, id server.ID) (server.Server, error) {
	f.calls = append(f.calls, "restart:"+string(id))
	return f.result()
}

func (f *fakeLifecycle) result() (server.Server, error) {
	if f.err != nil {
		return server.Server{}, f.err
	}
	return f.srv, nil
}

// serviceLifecycle 是 app.Lifecycle 在 HTTP 层测试里的等价物：enabled/desiredState
// 走真实 Service 与真实仓储，只有"实例收敛"这一步用写 phase 代替
// （HTTP 层不感知实例，phase 就是它对外的观测面）。
type serviceLifecycle struct {
	svc  *server.Service
	repo server.Repository
}

func (l serviceLifecycle) SetEnabled(ctx context.Context, id server.ID, enabled bool) (server.Server, error) {
	if _, err := l.svc.SetEnabled(ctx, id, enabled); err != nil {
		return server.Server{}, err
	}
	phase := server.PhaseStarting
	if !enabled {
		phase = server.PhaseStopped
	}
	return l.setPhase(ctx, id, phase)
}

func (l serviceLifecycle) Start(ctx context.Context, id server.ID) (server.Server, error) {
	return l.setDesiredState(ctx, id, server.DesiredRunning, server.PhaseStarting)
}

func (l serviceLifecycle) Stop(ctx context.Context, id server.ID) (server.Server, error) {
	return l.setDesiredState(ctx, id, server.DesiredStopped, server.PhaseStopped)
}

func (l serviceLifecycle) Restart(ctx context.Context, id server.ID) (server.Server, error) {
	return l.setDesiredState(ctx, id, server.DesiredRunning, server.PhaseStarting)
}

func (l serviceLifecycle) setDesiredState(ctx context.Context, id server.ID, state server.DesiredState, phase server.Phase) (server.Server, error) {
	if _, err := l.svc.SetDesiredState(ctx, id, state); err != nil {
		return server.Server{}, err
	}
	return l.setPhase(ctx, id, phase)
}

func (l serviceLifecycle) setPhase(ctx context.Context, id server.ID, phase server.Phase) (server.Server, error) {
	srv, err := l.svc.Get(ctx, id)
	if err != nil {
		return server.Server{}, err
	}
	in := srv.Status.CreateInput()
	in.Phase = phase
	if err := l.repo.UpdateStatus(ctx, id, in); err != nil {
		return server.Server{}, err
	}
	return l.svc.Get(ctx, id)
}

// newLifecycleHandler 用真实 Service + 真实 SQLite 组装 handler，并把 Lifecycle 接到
// 同一个 Service 上（生产装配里这一层是 app.Lifecycle）。
func newLifecycleHandler(t *testing.T) http.Handler {
	t.Helper()

	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := sqlite.NewServerRepository(db)
	registry := server.NewService(repo).WithIDGenerator(func() string { return "srv-1" })

	return httpapi.NewHandler(httpapi.Options{
		Registry:      registry,
		Lifecycle:     serviceLifecycle{svc: registry, repo: repo},
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// newLifecycleStubHandler 只装配 Lifecycle（无 Refresher）：动作路由仍然注册，
// 但 refresh-tools 必须退化为"未知动作"。
func newLifecycleStubHandler(t *testing.T, lifecycle httpapi.ServerLifecycle) http.Handler {
	t.Helper()

	return httpapi.NewHandler(httpapi.Options{
		Registry:      failingRegistry{err: errors.New("unused")},
		Lifecycle:     lifecycle,
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// 动作端点按"结果能否立即观测"区分状态码：需要后台重建实例的返回 202 + Location，
// 同步回收的返回 200（终态 phase 已在响应里）。变更必须落库，GET 复核一致。
func TestLifecycleActions(t *testing.T) {
	tests := []struct {
		action       string
		wantStatus   int
		wantLocation bool
		wantEnabled  bool
		wantDesired  string
		wantPhase    string
	}{
		{"enable", http.StatusAccepted, true, true, "running", "starting"},
		{"disable", http.StatusOK, false, false, "running", "stopped"},
		{"start", http.StatusAccepted, true, true, "running", "starting"},
		{"stop", http.StatusOK, false, true, "stopped", "stopped"},
		{"restart", http.StatusAccepted, true, true, "running", "starting"},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			h := newLifecycleHandler(t)
			if rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers", minimalWeather); rec.Code != http.StatusAccepted {
				t.Fatalf("register status = %d, want 202; body: %s", rec.Code, rec.Body)
			}

			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers/srv-1:"+tt.action, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if loc := rec.Header().Get("Location"); tt.wantLocation && loc != "/api/v1/mcp-servers/srv-1" {
				t.Errorf("Location = %q, want the server self link", loc)
			} else if !tt.wantLocation && loc != "" {
				t.Errorf("Location = %q, want none on a synchronous action", loc)
			}

			assertActionState(t, decodeServerBody(t, rec), tt.wantEnabled, tt.wantDesired, tt.wantPhase)

			rec = do(t, h, http.MethodGet, "/api/v1/mcp-servers/srv-1", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want 200; body: %s", rec.Code, rec.Body)
			}
			assertActionState(t, decodeServerBody(t, rec), tt.wantEnabled, tt.wantDesired, tt.wantPhase)
		})
	}
}

func assertActionState(t *testing.T, got serverBody, wantEnabled bool, wantDesired, wantPhase string) {
	t.Helper()

	if got.Enabled != wantEnabled {
		t.Errorf("enabled = %t, want %t", got.Enabled, wantEnabled)
	}
	if got.Spec.DesiredState != wantDesired {
		t.Errorf("desiredState = %q, want %q", got.Spec.DesiredState, wantDesired)
	}
	if got.Status.Phase != wantPhase {
		t.Errorf("phase = %q, want %q", got.Status.Phase, wantPhase)
	}
}

// 动作语义全在路径里：带非空请求体一律 400，且不得触碰生命周期依赖。
func TestLifecycleActionRejectsRequestBody(t *testing.T) {
	for _, action := range []string{"enable", "disable", "start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			lifecycle := &fakeLifecycle{srv: refreshableServer()}
			h := newLifecycleStubHandler(t, lifecycle)

			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers/srv-1:"+action, `{}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "invalid_argument")
			assertErrorMessage(t, rec, "action does not accept a request body")
			if len(lifecycle.calls) != 0 {
				t.Errorf("lifecycle calls = %v, want none", lifecycle.calls)
			}
		})
	}
}

func TestLifecycleActionDomainErrors(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		err        error
		wantStatus int
		wantCode   string
	}{
		// :start 在 disabled 的 Server 上是 409：ErrNotRunnable 是唯一判定，
		// 无论它来自 SetEnabled 还是 Start，映射都必须一致。
		{"not runnable", "start", fmt.Errorf("%w: disabled", server.ErrNotRunnable), http.StatusConflict, "conflict"},
		{"not runnable enable", "enable", fmt.Errorf("%w: disabled", server.ErrNotRunnable), http.StatusConflict, "conflict"},
		{"missing server", "stop", server.ErrNotFound, http.StatusNotFound, "not_found"},
		{"revision conflict", "restart", server.ErrConflict, http.StatusConflict, "conflict"},
		{"invalid argument", "disable", fmt.Errorf("%w: bad request", server.ErrInvalid), http.StatusBadRequest, "invalid_argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newLifecycleStubHandler(t, &fakeLifecycle{err: tt.err})

			rec := do(t, h, http.MethodPost, "/api/v1/mcp-servers/srv-1:"+tt.action, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			assertErrorCode(t, rec, tt.wantCode)
		})
	}
}

// 未知动作、以及所需依赖未装配的动作，都收敛到 404 unknown action。
func TestActionRouteUnknownOrUnwired(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		withLifecycle bool
	}{
		{"unknown action", "/api/v1/mcp-servers/srv-1:nope", true},
		{"refresh-tools without refresher", "/api/v1/mcp-servers/srv-1:refresh-tools", true},
		{"enable without lifecycle", "/api/v1/mcp-servers/srv-1:enable", false},
		{"restart without lifecycle", "/api/v1/mcp-servers/srv-1:restart", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h http.Handler
			if tt.withLifecycle {
				h = newLifecycleStubHandler(t, &fakeLifecycle{srv: refreshableServer()})
			} else {
				h = newRefreshHandler(t, &fakeRefresher{srv: refreshableServer()})
			}

			rec := do(t, h, http.MethodPost, tt.path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
			}
			assertErrorCode(t, rec, "not_found")
			assertErrorMessage(t, rec, "unknown action")
		})
	}
}
