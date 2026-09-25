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
