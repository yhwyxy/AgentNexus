package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// newRegistry 用真实 SQLite 组装 Service：注册路由的基数断言需要真实 id 生成。
func newRegistry(t *testing.T) *server.Service {
	t.Helper()

	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	n := 0

	return server.NewService(sqlite.NewServerRepository(db)).
		WithIDGenerator(func() string { n++; return fmt.Sprintf("srv-%d", n) })
}

// recorderMetrics 实现 RequestMetrics，记录 httpapi 实际交给采集方的 route label。
type recorderMetrics struct {
	mu     sync.Mutex
	routes []string
	codes  []string
}

func (m *recorderMetrics) ObserveRequest(_, route string, _ int, _ time.Duration, errorCode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes = append(m.routes, route)
	m.codes = append(m.codes, errorCode)
}

func (m *recorderMetrics) snapshot() ([]string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.routes...), append([]string(nil), m.codes...)
}

func newMetricsHandler(t *testing.T) (http.Handler, *recorderMetrics) {
	t.Helper()

	metrics := &recorderMetrics{}
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:       newRegistry(t),
		Metrics:        http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "# test\n") }),
		RequestMetrics: metrics,
		Authenticator:  testAuthorizer(t),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	return handler, metrics
}

// TestMetricsEndpointAuthorization 固定 /metrics 的访问控制：未带凭证 401、agent 403、admin 200。
func TestMetricsEndpointAuthorization(t *testing.T) {
	handler, _ := newMetricsHandler(t)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no credentials", nil, http.StatusUnauthorized},
		{"agent key", map[string]string{"Authorization": "Bearer " + testAgentSecret}, http.StatusForbidden},
		{"admin key", map[string]string{"Authorization": "Bearer " + testAdminSecret}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, handler, http.MethodGet, "/metrics", "", tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("GET /metrics status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK {
				if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
					t.Errorf("Content-Type = %q, want text/plain prefix", got)
				}
			}
		})
	}
}

// TestRouteLabelIsBoundedByPatterns 固定基数防线：route label 取注册 pattern，
// 不随资产 ID 变化，响应文本里也不出现 id。
func TestRouteLabelIsBoundedByPatterns(t *testing.T) {
	handler, metrics := newMetricsHandler(t)

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"name":"server-%d","transport":"streamable_http","runtime":{"type":"remote","remote":{"endpoint":"http://backend-%d:8080/mcp"}}}`, i, i)
		rec := do(t, handler, http.MethodPost, "/api/v1/mcp-servers", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST status = %d, want 202; body: %s", rec.Code, rec.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		ids = append(ids, created.ID)

		rec = do(t, handler, http.MethodGet, "/api/v1/mcp-servers/"+created.ID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", rec.Code)
		}
	}
	// 未知路径：必须收敛到 unmatched，而不是把任意路径原样变成 label。
	// 默认策略对未登记前缀失败关闭，因此这里得到 403（认证先于 mux 匹配）。
	if rec := do(t, handler, http.MethodGet, "/definitely-not-a-route", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown path status = %d, want 403", rec.Code)
	}

	routes, _ := metrics.snapshot()
	want := map[string]bool{
		"/api/v1/mcp-servers":      true,
		"/api/v1/mcp-servers/{id}": true,
		"unmatched":                true,
	}
	for _, route := range routes {
		if !want[route] {
			t.Errorf("unexpected route label %q; labels = %v", route, routes)
		}
		if route != "unmatched" && strings.Contains(route, "srv-") {
			t.Errorf("route label leaked an asset id: %q", route)
		}
	}
	// 三个不同 id 的 GET 必须共用同一个 label。
	count := 0
	for _, route := range routes {
		if route == "/api/v1/mcp-servers/{id}" {
			count++
		}
	}
	if count != len(ids) {
		t.Errorf("GET /api/v1/mcp-servers/{id} observed %d times, want %d", count, len(ids))
	}
}

// TestRequestMetricsReceivesErrorCode 断言错误码来自 requestRecord（writeError 分类过的失败），
// 而成功请求带空错误码。
func TestRequestMetricsReceivesErrorCode(t *testing.T) {
	handler, metrics := newMetricsHandler(t)

	do(t, handler, http.MethodGet, "/api/v1/mcp-servers/unknown-id", "")
	do(t, handler, http.MethodPost, "/api/v1/mcp-servers", `{"name":"BAD NAME","transport":"streamable_http"}`)

	_, codes := metrics.snapshot()
	if len(codes) != 2 {
		t.Fatalf("observed %d requests, want 2: %v", len(codes), codes)
	}
	if codes[0] != "not_found" {
		t.Errorf("error_code for unknown id = %q, want not_found", codes[0])
	}
	if codes[1] != "invalid_argument" {
		t.Errorf("error_code for invalid body = %q, want invalid_argument", codes[1])
	}
}

// TestRequestMetricsNilDisablesCollection 断言未装配 RequestMetrics 时不再有观测调用
// （handler 仍然正常工作）。
func TestRequestMetricsNilDisablesCollection(t *testing.T) {
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      newRegistry(t),
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if rec := do(t, handler, http.MethodGet, "/health/live", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /health/live status = %d, want 200", rec.Code)
	}
}

// TestMetricsRouteAbsentWhenNotConfigured 断言 Options.Metrics 为 nil 时 /metrics 不存在。
func TestMetricsRouteAbsentWhenNotConfigured(t *testing.T) {
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:      newRegistry(t),
		Authenticator: testAuthorizer(t),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	rec := do(t, handler, http.MethodGet, "/metrics", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics without Metrics option = %d, want 404", rec.Code)
	}
}

// TestPanickingObserverDoesNotBreakRequests 断言采集方 panic 不会摧毁请求：这是
// 「可观测性不能成为业务故障源」的最小保证（中间件里必须拦住）。
func TestPanickingObserverDoesNotBreakRequests(t *testing.T) {
	handler := httpapi.NewHandler(httpapi.Options{
		Registry:       newRegistry(t),
		RequestMetrics: panickingMetrics{},
		Authenticator:  testAuthorizer(t),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	rec := do(t, handler, http.MethodGet, "/health/live", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health/live status = %d, want 200", rec.Code)
	}
}

type panickingMetrics struct{}

func (panickingMetrics) ObserveRequest(string, string, int, time.Duration, string) {
	panic(errors.New("observer exploded"))
}
