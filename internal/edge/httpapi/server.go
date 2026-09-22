// AgentNexus HTTP Server：路由装配与进程级 http.Server。
package httpapi

import (
	"log/slog"
	"net/http"
)

// NewHandler 装配全部 HTTP 路由。logger 为 nil 时使用 slog.Default()。
func NewHandler(registry ServerRegistry, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	servers := &serverHandler{registry: registry, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady)
	mux.HandleFunc("POST "+serverRoute, servers.register)
	mux.HandleFunc("GET "+serverRoute+"/{id}", servers.get)

	return mux
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
