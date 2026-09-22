package httpapi_test

import (
	"bytes"
	"context"
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

	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// newTestHandler 用真实 Service + 真实 SQLite 组装 HTTP handler，
// 时钟与 ID 固定，使响应 JSON 可与字面量逐字段比对。
func newTestHandler(t *testing.T) http.Handler {
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

	return httpapi.NewHandler(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
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
	h := httpapi.NewHandler(
		failingRegistry{err: errors.New("boom: /var/lib/secret")},
		slog.New(slog.NewTextHandler(&logs, nil)),
	)

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
