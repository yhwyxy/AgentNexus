// 指标注册表：整体架构设计 §9.2 的 7 个指标族与 /metrics 端点。
//
// 本包是 adapter：只有它认识 Prometheus 的类型；领域包（gateway/runtime/server）
// 只发射事实，由这里翻译成指标。指标名与 label 取值集合刻意保持有限，
// 见 docs/superpowers/specs/2026-09-25-metrics-endpoint-design.md §4。
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

// healthTimeout 限制抓取时的健康读取：抓取是周期行为，不能被一次慢查询拖死。
const healthTimeout = 2 * time.Second

// ServerLister 是本包对 Server 持久化的最小依赖（消费者定义接口），
// 与 Lifecycle 的 Lister 同形；生产装配传入 sqlite.ServerRepository。
type ServerLister interface {
	ListEnabled(ctx context.Context) ([]server.Server, error)
}

// Metrics 持有进程内唯一的 registry 与 7 个指标族，并实现 http.Handler。
//
// 它不使用 Prometheus 的默认 registry：全局状态会让测试互相污染，
// 也会把任何第三方库顺手注册的 collector 混进 /metrics 输出。
type Metrics struct {
	registry *prometheus.Registry
	handler  http.Handler
	lister   ServerLister
	logger   *slog.Logger

	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	requestErrors *prometheus.CounterVec
	toolCalls     *prometheus.CounterVec
	toolFailures  *prometheus.CounterVec
	health        *prometheus.GaugeVec
	restarts      *prometheus.CounterVec

	// scrapeMu 串行化抓取。健康 gauge 是「Reset 后重填」的，并发抓取必须互斥，
	// 否则一方会看到另一方清空后的半成品。
	scrapeMu sync.Mutex
}

// New 构建注册表并注册全部 7 个指标族。lister 必须非 nil（健康 gauge 依赖它）。
func New(lister ServerLister, logger *slog.Logger) *Metrics {
	if logger == nil {
		logger = slog.Default()
	}

	m := &Metrics{
		registry: prometheus.NewRegistry(),
		lister:   lister,
		logger:   logger,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_requests_total",
			Help: "HTTP requests handled by the control plane, including authentication failures.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_request_duration_seconds",
			Help:    "End-to-end HTTP request duration (includes authentication and streamed responses).",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		requestErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_request_errors_total",
			Help: "HTTP requests that ended with a classified error code.",
		}, []string{"method", "route", "error_code"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_tool_calls_total",
			Help: "Tool calls routed through the MCP gateway.",
		}, []string{"server", "tool"}),
		toolFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_tool_call_failures_total",
			Help: "Tool calls the gateway itself failed; backend isError=true results are not failures.",
		}, []string{"server", "tool", "error_code"}),
		health: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mcp_server_health_status",
			Help: "1 when the MCP server's observed phase is ready, 0 otherwise (read at scrape time).",
		}, []string{"server", "phase"}),
		restarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_runtime_restarts_total",
			Help: "Runtime instances replaced while the server stayed desired-running.",
		}, []string{"server"}),
	}

	m.registry.MustRegister(
		m.requests, m.duration, m.requestErrors,
		m.toolCalls, m.toolFailures, m.health, m.restarts,
	)
	// promhttp 负责内容协商（text/plain 0.0.4 与 OpenMetrics）。
	m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})

	return m
}

// ObserveRequest 记录一个已结束的 HTTP 请求。httpapi 的 RequestMetrics 接口由本方法满足。
//
// errorCode 为空表示请求没有分类错误（成功、重定向或 handler 直接写的状态码）：
// 此时不产生 mcp_request_errors_total 样本。
func (m *Metrics) ObserveRequest(method, route string, status int, duration time.Duration, errorCode string) {
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(duration.Seconds())
	if errorCode != "" {
		m.requestErrors.WithLabelValues(method, route, errorCode).Inc()
	}
}

// ServeHTTP 暴露 /metrics：先按请求刷新健康 gauge，再输出注册表。
//
// 健康读取失败返回 500：让抓取方在 up{} 上立刻看到失败，而不是拿到一份
// 静默缺族的成功响应。响应体不含内部细节，原因只进日志。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.scrapeMu.Lock()
	defer m.scrapeMu.Unlock()

	if err := m.refreshHealth(r.Context()); err != nil {
		m.logger.Error("collect server health for metrics", "error", err)
		http.Error(w, "metrics collection failed", http.StatusInternalServerError)

		return
	}

	m.handler.ServeHTTP(w, r)
}

// refreshHealth 用 ListEnabled 的当前结果整体替换健康 gauge。
// Reset 是必要的：否则已删除或已禁用的 Server 会以最后一次观测值永久留在输出里。
func (m *Metrics) refreshHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	servers, err := m.lister.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("list enabled servers: %w", err)
	}

	m.health.Reset()
	for _, srv := range servers {
		value := 0.0
		if srv.Status.Phase == server.PhaseReady {
			value = 1
		}
		m.health.WithLabelValues(labelValue(srv.Name), string(srv.Status.Phase)).Set(value)
	}

	return nil
}

// labelValue 兜底空 label 值：空串在 Prometheus 里是合法取值，但无法与
// 「标签不存在」区分，排查时歧义太大，因此统一落到 "unknown"。
func labelValue(value string) string {
	if value == "" {
		return "unknown"
	}

	return value
}

// errorCodeLabel 兜底缺失的错误码（调用点没能分类失败原因时）。
func errorCodeLabel(code string) string {
	if code == "" {
		return "unknown"
	}

	return code
}
