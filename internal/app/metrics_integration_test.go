package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/app"
	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	mcpserver "github.com/yhwyxy/AgentNexus/internal/mcpadapter/server"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/metrics"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/remote"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// metricsStack 是与 app.Run 同形的完整装配，额外挂上 metrics 与 RequestMetrics。
// 抓取断言因此验证的是「生产装配会得到什么」，而不是孤立的 registry。
type metricsStack struct {
	url       string
	lifecycle *app.Lifecycle
	close     func()
}

func startMetricsStack(t *testing.T) *metricsStack {
	t.Helper()

	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	servers := sqlite.NewServerRepository(db)
	toolRepo := sqlite.NewToolRepository(db)
	recorder := audit.NewRecorder(sqlite.NewAuditRepository(db))
	auditObserver := observability.NewAuditObserver(recorder, testLogger())
	meter := metrics.New(servers, testLogger())
	metricsObserver := metrics.NewObserver(meter)

	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	runtimes, err := runtime.NewManager(servers, remote.NewProvider())
	if err != nil {
		t.Fatalf("create runtime manager: %v", err)
	}
	runtimes.WithObserver(observability.MultiRuntimeObserver{auditObserver, metricsObserver})

	catalog := tool.NewCatalog(toolRepo)
	lifecycle, err := app.NewLifecycle(app.LifecycleOptions{
		Registry:          server.NewService(servers),
		Syncer:            tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog),
		Lister:            servers,
		ReconcileInterval: 10 * time.Millisecond,
		Auditor:           recorder,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
	caller, err := gateway.NewToolCaller(catalog, servers, runtimes, clients)
	if err != nil {
		t.Fatalf("create tool caller: %v", err)
	}
	caller.WithObserver(observability.MultiCallObserver{auditObserver, metricsObserver})

	virtual, err := gateway.NewVirtualServer(catalog, caller)
	if err != nil {
		t.Fatalf("create virtual server: %v", err)
	}
	mcpHandler, err := mcpserver.NewHandler(virtual)
	if err != nil {
		t.Fatalf("create MCP handler: %v", err)
	}
	httpServer := httptest.NewServer(httpapi.NewHandler(httpapi.Options{
		Registry:       lifecycle,
		Refresher:      lifecycle,
		MCP:            mcpHandler,
		Metrics:        meter,
		RequestMetrics: meter,
		Authenticator:  testAuthorizer(t),
		Auditor:        recorder,
		Logger:         testLogger(),
	}))

	return &metricsStack{
		url:       httpServer.URL,
		lifecycle: lifecycle,
		close: func() {
			httpServer.Close()
			closeLifecycle(t, lifecycle)
			_ = clients.Close(context.Background())
			_ = db.Close()
		},
	}
}

// scrapeMetrics 以 admin 抓取 /metrics（非 200 即失败）。
func scrapeMetrics(t *testing.T, baseURL string) string {
	t.Helper()

	resp := doAuthorized(t, http.MethodGet, baseURL+"/metrics", "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	return string(raw)
}

func assertMetricContains(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("/metrics missing %q", want)
	}
}

func assertMetricAbsent(t *testing.T, body, forbidden string) {
	t.Helper()
	if strings.Contains(body, forbidden) {
		t.Errorf("/metrics unexpectedly contains %q", forbidden)
	}
}

// TestMetricsReflectRegistrationToolCallsAndHealth 端到端覆盖四条数据来源：
// HTTP 请求、运行态协调、工具调用、抓取时的健康读取。
func TestMetricsReflectRegistrationToolCallsAndHealth(t *testing.T) {
	ctx := context.Background()
	backend := fakemcp.New()
	defer backend.Close()

	stack := startMetricsStack(t)
	defer stack.close()

	id := registerServer(t, stack.url, "backend", backend.HTTP.URL, nil)
	waitForReady(t, stack.url, id)

	client := mcp.NewClient(&mcp.Implementation{Name: "metrics-test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, streamableTransport(stack.url+"/mcp", testAdminSecret), nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer session.Close()

	// 后端工具返回 isError=true：合法结果，不得计入 failures。
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.fail", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("tools/call backend.demo.fail: %v", err)
	}
	// 参数不满足 inputSchema：网关侧失败，必须计入 failures。
	// （不存在的公开名由 MCP 层的 tools/list 注册表拦下，不会到达路由器；
	//  未解析路由的 label 兜底由 internal/metrics 的单元测试覆盖。）
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.echo", Arguments: map[string]any{}}); err == nil {
		t.Fatal("tools/call backend.demo.echo with empty arguments succeeded, want protocol error")
	}

	body := scrapeMetrics(t, stack.url)

	assertMetricContains(t, body, `mcp_requests_total{method="POST",route="/api/v1/mcp-servers",status="202"} 1`)
	assertMetricContains(t, body, `mcp_server_health_status{phase="ready",server="backend"} 1`)
	assertMetricContains(t, body, `mcp_tool_calls_total{server="backend",tool="backend.demo.fail"} 1`)
	assertMetricContains(t, body, `mcp_tool_calls_total{server="backend",tool="backend.demo.echo"} 1`)
	assertMetricContains(t, body,
		`mcp_tool_call_failures_total{error_code="invalid_argument",server="backend",tool="backend.demo.echo"} 1`)
	// isError=true 不算失败：该工具名下不得出现 failures series。
	assertMetricAbsent(t, body, `mcp_tool_call_failures_total{error_code="unknown",server="backend",tool="backend.demo.fail"}`)
	// 首次启动不是重启，因此该族此时不应有 series。
	assertMetricAbsent(t, body, "mcp_runtime_restarts_total{")
	assertMetricContains(t, body, "mcp_request_duration_seconds_count")

	// route label 取自注册 pattern（不是路径），且 /metrics 自身也被计入：
	// 第一次抓取时该 series 尚不存在（观测发生在响应写完之后），第二次抓取可见。
	body = scrapeMetrics(t, stack.url)
	assertMetricContains(t, body, `mcp_requests_total{method="GET",route="/metrics",status="200"} 1`)
}

// TestMetricsEndpointRejectsAgentRole 断言线上策略对 /metrics 的限制在整装 handler 上生效。
func TestMetricsEndpointRejectsAgentRole(t *testing.T) {
	stack := startMetricsStack(t)
	defer stack.close()

	for secret, want := range map[string]int{
		testAgentSecret: http.StatusForbidden,
		"":              http.StatusUnauthorized,
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, stack.url+"/metrics", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET /metrics with secret %q status = %d, want %d", secret, resp.StatusCode, want)
		}
	}
}
