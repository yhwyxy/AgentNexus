package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/metrics"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// fakeLister 是 ServerLister 的测试替身：返回固定结果，并记录调用次数。
type fakeLister struct {
	servers []server.Server
	err     error
	calls   int
}

func (f *fakeLister) ListEnabled(context.Context) ([]server.Server, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}

	return f.servers, nil
}

func testLogger(buffer *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buffer, nil))
}

// scrape 走真实 HTTP 面：/metrics 的输出文本才是指标的对外契约。
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	return rec.Body.String()
}

func assertContains(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("/metrics output missing %q\n---\n%s", want, body)
	}
}

func assertAbsent(t *testing.T, body, forbidden string) {
	t.Helper()
	if strings.Contains(body, forbidden) {
		t.Errorf("/metrics output unexpectedly contains %q\n---\n%s", forbidden, body)
	}
}

func readyServer(name string) server.Server {
	return server.Server{
		ID:     server.ID("id-" + name),
		Name:   name,
		Status: server.Status{Phase: server.PhaseReady},
	}
}

// TestExposesArchitectureMetricFamilies 断言 7 个指标族（架构文档 §9.2）都能出现在输出里，
// 且类型与 histogram 后缀正确。
func TestExposesArchitectureMetricFamilies(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{readyServer("backend")}}
	meter, _ := newMeterWithLister(t, lister)
	meter.ObserveRequest(http.MethodGet, "/health/live", http.StatusOK, time.Millisecond, "")
	meter.ObserveRequest(http.MethodGet, "/metrics", http.StatusUnauthorized, time.Millisecond, "unauthenticated")
	observer := metrics.NewObserver(meter)
	observer.ToolCalled(context.Background(), gateway.ToolCallObservation{
		PublicName: "backend.demo.echo",
		Route:      tool.Route{ServerName: "backend"},
		Outcome:    audit.OutcomeSuccess,
	})
	observer.ToolCalled(context.Background(), gateway.ToolCallObservation{
		PublicName: "backend.demo.fail",
		Route:      tool.Route{ServerName: "backend"},
		Outcome:    audit.OutcomeError,
		ErrorCode:  "server_unavailable",
	})
	observer.RuntimeRestarted(context.Background(), readyServer("backend"), runtime.Instance{}, runtime.Instance{})

	body := scrape(t, meter)

	for _, want := range []string{
		"# TYPE mcp_requests_total counter",
		"# TYPE mcp_request_duration_seconds histogram",
		"# TYPE mcp_request_errors_total counter",
		"# TYPE mcp_tool_calls_total counter",
		"# TYPE mcp_tool_call_failures_total counter",
		"# TYPE mcp_server_health_status gauge",
		"# TYPE mcp_runtime_restarts_total counter",
		"mcp_request_duration_seconds_bucket",
		"mcp_request_duration_seconds_sum",
		"mcp_request_duration_seconds_count",
	} {
		assertContains(t, body, want)
	}
}

// TestObserveRequestLabelsAndErrorFamily 覆盖请求族的三条 label 语义与
// 「无错误码不产生 error 族」的约定。
func TestObserveRequestLabelsAndErrorFamily(t *testing.T) {
	meter, _ := newMeter(t)

	meter.ObserveRequest(http.MethodPost, "/api/v1/mcp-servers", http.StatusAccepted, time.Millisecond, "")
	meter.ObserveRequest(http.MethodPost, "/api/v1/mcp-servers", http.StatusAccepted, time.Millisecond, "")
	meter.ObserveRequest(http.MethodGet, "unmatched", http.StatusNotFound, time.Millisecond, "")

	body := scrape(t, meter)
	assertContains(t, body, `mcp_requests_total{method="POST",route="/api/v1/mcp-servers",status="202"} 2`)
	assertContains(t, body, `mcp_requests_total{method="GET",route="unmatched",status="404"} 1`)
	assertContains(t, body, `mcp_request_duration_seconds_count{method="POST",route="/api/v1/mcp-servers"} 2`)
	// status=202 的两次请求都没有分类错误码，error 族不应出现。
	assertAbsent(t, body, "mcp_request_errors_total{")

	meter.ObserveRequest(http.MethodPost, "/api/v1/mcp-servers", http.StatusBadRequest, time.Millisecond, "invalid_argument")
	body = scrape(t, meter)
	assertContains(t, body,
		`mcp_request_errors_total{error_code="invalid_argument",method="POST",route="/api/v1/mcp-servers"} 1`)
}

// TestHistogramCountsAndBuckets 断言 duration histogram 的累计桶与 sum。
func TestHistogramCountsAndBuckets(t *testing.T) {
	meter, _ := newMeter(t)

	meter.ObserveRequest(http.MethodGet, "/health/live", http.StatusOK, 10*time.Millisecond, "")
	meter.ObserveRequest(http.MethodGet, "/health/live", http.StatusOK, 12*time.Second, "")

	body := scrape(t, meter)
	assertContains(t, body, `mcp_request_duration_seconds_bucket{method="GET",route="/health/live",le="0.005"} 0`)
	assertContains(t, body, `mcp_request_duration_seconds_bucket{method="GET",route="/health/live",le="10"} 1`)
	assertContains(t, body, `mcp_request_duration_seconds_bucket{method="GET",route="/health/live",le="+Inf"} 2`)
	assertContains(t, body, `mcp_request_duration_seconds_count{method="GET",route="/health/live"} 2`)
}

// TestHealthGaugeReflectsListEnabled 断言健康 gauge 是抓取时读取的结果，
// 且 Reset 使已消失的 Server 不再出现在输出里。
func TestHealthGaugeReflectsListEnabled(t *testing.T) {
	lister := &fakeLister{servers: []server.Server{
		readyServer("weather"),
		{ID: "id-clock", Name: "clock", Status: server.Status{Phase: server.PhaseDegraded}},
	}}
	meter, _ := newMeterWithLister(t, lister)

	body := scrape(t, meter)
	assertContains(t, body, `mcp_server_health_status{phase="ready",server="weather"} 1`)
	assertContains(t, body, `mcp_server_health_status{phase="degraded",server="clock"} 0`)

	// 第二次抓取前状态变化：weather 被删除，clock 转为 ready。
	lister.servers = []server.Server{readyServer("clock")}
	body = scrape(t, meter)
	assertAbsent(t, body, "weather")
	assertContains(t, body, `mcp_server_health_status{phase="ready",server="clock"} 1`)

	if lister.calls != 2 {
		t.Errorf("ListEnabled calls = %d, want 2 (每次抓取读一次)", lister.calls)
	}
}

// TestHealthReadFailureReturns500 断言健康读取失败时抓取方看到失败，
// 而不是一份静默缺族的成功响应。
func TestHealthReadFailureReturns500(t *testing.T) {
	lister := &fakeLister{err: errors.New("boom")}
	var logs bytes.Buffer
	meter := metrics.New(lister, testLogger(&logs))

	rec := httptest.NewRecorder()
	meter.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /metrics status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response leaks the underlying error: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "collect server health for metrics") {
		t.Errorf("failure was not logged; logs: %s", logs.String())
	}
}

// TestConcurrentObservationAndScrape 在 -race 下验证并发观测与并发抓取：
// 抓取被串行化（健康 gauge 是 Reset + 重填），且计数不丢。
func TestConcurrentObservationAndScrape(t *testing.T) {
	meter, lister := newMeter(t)

	const observers, scrapes, perObserver = 8, 8, 25
	var wg sync.WaitGroup
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perObserver; j++ {
				meter.ObserveRequest(http.MethodPost, "/mcp", http.StatusOK, time.Millisecond, "")
			}
		}()
	}
	for i := 0; i < scrapes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			meter.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent GET /metrics status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	body := scrape(t, meter)
	assertContains(t, body, `mcp_requests_total{method="POST",route="/mcp",status="200"} 200`)
	// 观测不触发抓取：只有 scrapes 次并发抓取 + 最后一次断言抓取读到 ListEnabled。
	if want := scrapes + 1; lister.calls != want {
		t.Errorf("ListEnabled calls = %d, want %d", lister.calls, want)
	}
}

func newMeter(t *testing.T) (*metrics.Metrics, *fakeLister) {
	t.Helper()

	lister := &fakeLister{}
	meter, _ := newMeterWithLister(t, lister)

	return meter, lister
}

func newMeterWithLister(t *testing.T, lister *fakeLister) (*metrics.Metrics, *fakeLister) {
	t.Helper()

	var logs bytes.Buffer

	return metrics.New(lister, testLogger(&logs)), lister
}
