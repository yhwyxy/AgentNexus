// AgentNexus HTTP Server：路由装配与进程级 http.Server。
package httpapi

import (
	"log/slog"
	"net/http"
)

// Options 是 HTTP 层路由装配的依赖。Registry 之外的依赖均可选：
// Refresher 为 nil 时不注册动作路由，MCP 为 nil 时不挂载 /mcp。
// Authenticator 为 nil 时失败关闭（所有请求 403），生产装配永远传入真实 Authorizer。
type Options struct {
	Registry      ServerRegistry
	Refresher     ServerRefresher
	MCP           http.Handler
	Authenticator Authenticator
	Logger        *slog.Logger
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
		logger:    logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady)
	mux.HandleFunc("POST "+serverRoute, servers.register)
	mux.HandleFunc("GET "+serverRoute+"/{id}", servers.get)
	if opts.Refresher != nil {
		// net/http 的通配段必须独占一段（"{id}:refresh-tools" 非法），
		// 因此动作路由用 {rest...} 承接，在 handler 内解析 "<id>:<action>"。
		mux.HandleFunc("POST "+serverRoute+"/{rest...}", servers.action)
	}
	if opts.MCP != nil {
		mux.Handle("/mcp", opts.MCP)
	}

	return withAuth(mux, opts.Authenticator, logger)
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
