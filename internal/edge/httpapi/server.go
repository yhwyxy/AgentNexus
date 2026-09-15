// AgentNexus HTTP Server
package httpapi

import "net/http"

func NewServer(address string) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady)

	return &http.Server{
		Addr:    address,
		Handler: mux,
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
