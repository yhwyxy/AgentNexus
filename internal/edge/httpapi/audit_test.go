package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// memoryAuditRepo 收集 audit.Recorder 写入的事件。
// 这里刻意用真实的 Recorder（而不是假实现），让校验与脱敏逻辑一起被覆盖。
type memoryAuditRepo struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *memoryAuditRepo) Append(_ context.Context, event audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)

	return nil
}

func (r *memoryAuditRepo) recorded() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]audit.Event(nil), r.events...)
}

// auditFixture 组装"真实 Service + 真实审计 Recorder + 可捕获日志"的 handler。
type auditFixture struct {
	handler   http.Handler
	repo      *memoryAuditRepo
	refresher *fakeRefresher
	logs      *bytes.Buffer
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()

	repo := &memoryAuditRepo{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	registry := newTestRegistry(t)
	refresher := &fakeRefresher{srv: refreshableServer()}

	return &auditFixture{
		handler: httpapi.NewHandler(httpapi.Options{
			Registry:      registry,
			Refresher:     refresher,
			Authenticator: testAuthorizer(t),
			Auditor:       audit.NewRecorder(repo),
			Logger:        logger,
		}),
		repo:      repo,
		refresher: refresher,
		logs:      logs,
	}
}

// logLines 解析被捕获的 JSON 日志行。
func (f *auditFixture) logLines(t *testing.T) []map[string]any {
	t.Helper()

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		lines = append(lines, entry)
	}

	return lines
}

func TestRegisterEmitsAuditEvent(t *testing.T) {
	fixture := newAuditFixture(t)

	body := `{"name":"weather","transport":"stdio","runtime":{"type":"process","process":{"command":"/bin/echo","args":["secret-arg"],"env":{"TOKEN":"secret-env"}}}}`
	rec := doRequest(t, fixture.handler, http.MethodPost, "/api/v1/mcp-servers", body, map[string]string{
		"Authorization": "Bearer " + testAdminSecret,
		"X-Request-Id":  "req-42",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	events := fixture.repo.recorded()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1: %#v", len(events), events)
	}
	event := events[0]
	if event.Type != audit.EventServerRegistered || event.Outcome != audit.OutcomeSuccess {
		t.Fatalf("event = %q/%q, want server.registered/success", event.Type, event.Outcome)
	}
	// 调用者与请求 ID 来自 ctx：这是审计把"谁在哪个请求里做的"接到事实上的唯一路径。
	if event.ActorName != "test-admin" || event.ActorRole != "admin" {
		t.Errorf("actor = %q/%q, want test-admin/admin", event.ActorName, event.ActorRole)
	}
	if event.RequestID != "req-42" {
		t.Errorf("request id = %q, want req-42", event.RequestID)
	}
	if event.AssetID == "" || event.AssetName != "weather" {
		t.Errorf("asset = %q/%q, want the created server", event.AssetID, event.AssetName)
	}

	// 审计 Detail 只放形态字段：命令行、args、env、endpoint 一律不得出现。
	detail := string(event.Detail)
	for _, forbidden := range []string{"secret-arg", "secret-env", "/bin/echo", "TOKEN"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("audit detail leaked %q: %s", forbidden, detail)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(event.Detail, &fields); err != nil {
		t.Fatalf("detail is not a JSON object: %v", err)
	}
	if fields["runtime_type"] != "process" || fields["transport"] != "stdio" {
		t.Errorf("detail = %v, want runtime_type/transport of the registration", fields)
	}
}

func TestRefreshToolsEmitsAuditEvent(t *testing.T) {
	fixture := newAuditFixture(t)

	rec := do(t, fixture.handler, http.MethodPost, "/api/v1/mcp-servers/srv-1:refresh-tools", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	events := fixture.repo.recorded()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1: %#v", len(events), events)
	}
	if events[0].Type != audit.EventServerRefreshRequested {
		t.Fatalf("event type = %q, want server.refresh_requested", events[0].Type)
	}
	if events[0].AssetID != "srv-1" || events[0].AssetName != "weather" {
		t.Errorf("asset = %q/%q, want srv-1/weather", events[0].AssetID, events[0].AssetName)
	}
}

// 拒绝的请求不产生事件，否则审计里会出现"看起来发生过"的状态变更。
func TestFailedRequestsEmitNoAuditEvents(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		header map[string]string
	}{
		{"invalid body", http.MethodPost, "/api/v1/mcp-servers", "{", nil},
		{"unknown action", http.MethodPost, "/api/v1/mcp-servers/srv-1:nope", "", nil},
		{"unauthenticated", http.MethodPost, "/api/v1/mcp-servers", "{}", map[string]string{"Authorization": "Bearer wrong"}},
		{"agent refreshes", http.MethodPost, "/api/v1/mcp-servers/srv-1:refresh-tools", "", map[string]string{"Authorization": "Bearer " + testAgentSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAuditFixture(t)
			headers := map[string]string{"Authorization": "Bearer " + testAdminSecret}
			for name, value := range tt.header {
				headers[name] = value
			}
			rec := doRequest(t, fixture.handler, tt.method, tt.path, tt.body, headers)
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a failure; body: %s", rec.Code, rec.Body)
			}
			if events := fixture.repo.recorded(); len(events) != 0 {
				t.Fatalf("audit events = %#v, want none for a rejected request", events)
			}
		})
	}
}

func TestRequestLogRecordsOutcome(t *testing.T) {
	fixture := newAuditFixture(t)

	doRequest(t, fixture.handler, http.MethodGet, "/api/v1/mcp-servers/unknown", "", map[string]string{
		"Authorization": "Bearer " + testAdminSecret,
	})
	doRequest(t, fixture.handler, http.MethodGet, "/api/v1/mcp-servers/unknown", "", map[string]string{
		"Authorization": "Bearer wrong",
	})

	lines := fixture.logLines(t)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2: %v", len(lines), lines)
	}
	notFound, unauthorized := lines[0], lines[1]

	if notFound["status"] != float64(http.StatusNotFound) || notFound["error_code"] != "not_found" {
		t.Errorf("404 log = %v, want status 404 with error_code not_found", notFound)
	}
	if notFound["principal"] != "test-admin" || notFound["principal_role"] != "admin" {
		t.Errorf("404 log principal = %v/%v, want test-admin/admin", notFound["principal"], notFound["principal_role"])
	}
	// 认证失败的请求也必须被记录：这是它必须包在 withAuth 之外的原因。
	if unauthorized["status"] != float64(http.StatusUnauthorized) || unauthorized["error_code"] != "unauthenticated" {
		t.Errorf("401 log = %v, want status 401 with error_code unauthenticated", unauthorized)
	}
	if unauthorized["principal"] != "" {
		t.Errorf("401 log principal = %v, want empty", unauthorized["principal"])
	}
	if _, ok := notFound["duration_ms"]; !ok {
		t.Errorf("log entry has no duration_ms: %v", notFound)
	}
}

func TestRequestIDIsEchoedOrGenerated(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   func(string) bool
	}{
		{"valid header is reused", "req-abc.1_X", func(id string) bool { return id == "req-abc.1_X" }},
		{"missing header is generated", "", func(id string) bool { return len(id) == 36 }},
		{"invalid header is replaced", "bad id\ninjection", func(id string) bool { return len(id) == 36 }},
		{"overlong header is replaced", strings.Repeat("a", 65), func(id string) bool { return len(id) == 36 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAuditFixture(t)
			headers := map[string]string{"Authorization": "Bearer " + testAdminSecret}
			if tt.header != "" {
				headers["X-Request-Id"] = tt.header
			}
			rec := doRequest(t, fixture.handler, http.MethodGet, "/health/live", "", headers)

			echoed := rec.Header().Get("X-Request-Id")
			if !tt.want(echoed) {
				t.Fatalf("X-Request-Id = %q, unexpected", echoed)
			}
			lines := fixture.logLines(t)
			if len(lines) != 1 || lines[0]["request_id"] != echoed {
				t.Fatalf("log request_id = %v, want %q", lines, echoed)
			}
		})
	}
}

// 未装配 Recorder（测试/最小部署）时管理动作照常成功：审计是旁路，不是前置条件。
func TestHandlerWorksWithoutAuditor(t *testing.T) {
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      newTestRegistry(t),
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	rec := do(t, handler, http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"weather","transport":"streamable_http","runtime":{"type":"remote","remote":{"endpoint":"http://backend/mcp"}}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
}

// 审计存储失败不得改变已经成功的业务结果。
func TestAuditFailureDoesNotFailRequest(t *testing.T) {
	logs := &bytes.Buffer{}
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      newTestRegistry(t),
		Authenticator: testAuthorizer(t),
		Auditor:       failingRecorder{},
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
	})

	rec := do(t, handler, http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"weather","transport":"streamable_http","runtime":{"type":"remote","remote":{"endpoint":"http://backend/mcp"}}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 even when audit write fails; body: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "record audit event") {
		t.Errorf("audit failure was not logged: %s", logs.String())
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(context.Context, audit.Input) error {
	return errors.New("audit storage unavailable")
}

// newTestRegistry 用真实 Service + 真实 SQLite 组装注册服务，ID 固定便于断言。
func newTestRegistry(t *testing.T) *server.Service {
	t.Helper()

	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return server.NewService(sqlite.NewServerRepository(db)).
		WithIDGenerator(func() string { return "srv-1" })
}

// statusWriter 必须透传底层 writer：MCP 流式 HTTP 通过 http.ResponseController
// 找到 Flusher，否则 SSE 会静默退化成一次性写入。
func TestRequestLogMiddlewareKeepsStreaming(t *testing.T) {
	flushed := false
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err == nil {
			flushed = true
		}
	})

	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      failingRegistry{err: errors.New("unused")},
		MCP:           mcpHandler,
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminSecret)
	handler.ServeHTTP(recorder, req)

	if !flushed {
		t.Fatal("flush through the request-log middleware failed: statusWriter hid the Flusher")
	}
	if !recorder.Flushed {
		t.Fatal("underlying ResponseWriter was not flushed")
	}
}
