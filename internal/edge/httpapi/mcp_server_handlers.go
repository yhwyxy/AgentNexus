// MCP Server 管理端点：注册与查询。领域错误到 HTTP 状态码的映射只在这里。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

// ServerRegistry 是 HTTP 层对应用服务的最小依赖（消费者定义接口）。
type ServerRegistry interface {
	Register(ctx context.Context, in server.RegisterInput) (server.Server, error)
	Get(ctx context.Context, id server.ID) (server.Server, error)
}

// 对外错误码，对应详细设计 11.1 的子集。
const (
	codeInvalidArgument = "invalid_argument"
	codeNotFound        = "not_found"
	codeConflict        = "conflict"
	codeInternal        = "internal"
)

// 管理请求体上限；防止异常客户端耗尽内存。
const maxBodyBytes = 1 << 20

type serverHandler struct {
	registry ServerRegistry
	logger   *slog.Logger
}

func (h *serverHandler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidArgument, err.Error())
		return
	}

	created, err := h.registry.Register(r.Context(), req.toInput())
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// 202：Runtime 启动与后端初始化在请求之后异步完成（详细设计 10.1）。
	w.Header().Set("Location", selfLink(created.ID))
	writeJSON(w, http.StatusAccepted, toServerResponse(created))
}

func (h *serverHandler) get(w http.ResponseWriter, r *http.Request) {
	srv, err := h.registry.Get(r.Context(), server.ID(r.PathValue("id")))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerResponse(srv))
}

// writeDomainError 把领域哨兵错误映射为状态码；未知错误只记日志，
// 对客户端只返回固定文案，不泄漏内部细节。
func (h *serverHandler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, server.ErrInvalid):
		writeError(w, http.StatusBadRequest, codeInvalidArgument, err.Error())
	case errors.Is(err, server.ErrAlreadyExists):
		writeError(w, http.StatusConflict, codeConflict, "server already exists")
	case errors.Is(err, server.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "server not found")
	default:
		h.logger.Error("request failed",
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
