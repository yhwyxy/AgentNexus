// AgentNexus HTTP Server：路由装配与进程级 http.Server。
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/yhwyxy/AgentNexus/internal/audit"
)

// Options 是 HTTP 层路由装配的依赖。Registry 之外的依赖均可选：
// Refresher 为 nil 时不注册动作路由，MCP 为 nil 时不挂载 /mcp，
// Metrics 为 nil 时不注册 /metrics。
// Authenticator 为 nil 时失败关闭（所有请求 403），生产装配永远传入真实 Authorizer。
// Auditor 为 nil 时不写管理动作的审计事件（测试用），生产装配永远传入真实 Recorder。
// RequestMetrics 为 nil 时不采集请求指标。
type Options struct {
	Registry       ServerRegistry
	Refresher      ServerRefresher
	MCP            http.Handler
	Metrics        http.Handler
	Authenticator  Authenticator
	Auditor        AuditRecorder
	RequestMetrics RequestMetrics
	Logger         *slog.Logger
}

// AuditRecorder 是 HTTP 层对审计写入的最小依赖（消费者定义接口）。
type AuditRecorder interface {
	Record(ctx context.Context, in audit.Input) error
}

// NewHandler 装配管理路由与可选端点。Logger 为 nil 时使用 slog.Default()。
func NewHandler(opts Options) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	servers := &serverHandler{
		registry:  opts.Registry,
		refresher: opts.Refresher,
		auditor:   opts.Auditor,
		logger:    logger,
	}

	mux := http.NewServeMux()
	// patterns 与注册表保持同步：route label 只能取自这里登记过的 pattern，
	// 因此新增路由时必须经过 register（漏登记只会退化成 unmatched，不会漏数据）。
	patterns := newRoutePatterns()
	register := func(pattern string, handler http.Handler) {
		mux.Handle(pattern, handler)
		patterns.add(pattern)
	}
	register("GET /health/live", http.HandlerFunc(handleLive))
	register("GET /health/ready", http.HandlerFunc(handleReady))
	register("POST "+serverRoute, http.HandlerFunc(servers.register))
	register("GET "+serverRoute+"/{id}", http.HandlerFunc(servers.get))
	if opts.Refresher != nil {
		// net/http 的通配段必须独占一段（"{id}:refresh-tools" 非法），
		// 因此动作路由用 {rest...} 承接，在 handler 内解析 "<id>:<action>"。
		register("POST "+serverRoute+"/{rest...}", http.HandlerFunc(servers.action))
	}
	if opts.MCP != nil {
		register("/mcp", opts.MCP)
	}
	if opts.Metrics != nil {
		register("GET /metrics", opts.Metrics)
	}

	// 顺序固定：请求日志最外层（认证失败的请求也要被记录），认证次之（业务 handler 不感知认证）。
	inner := withAuth(mux, opts.Authenticator, logger)

	return withRequestLog(inner, requestLogOptions{
		mux:      mux,
		patterns: patterns,
		metrics:  opts.RequestMetrics,
		logger:   logger,
	})
}

func NewServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    address,
		Handler: handler,
	}
}

func handleLive(w http.ResponseWriter, _ *http.Request) {
	writeHealthOK(w)
}

func handleReady(w http.ResponseWriter, _ *http.Request) {
	writeHealthOK(w)
}

func writeHealthOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
