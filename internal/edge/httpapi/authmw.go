// HTTP 层的认证/授权中间件：这里是唯一的凭证解析点，业务 handler 不感知认证。
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yhwyxy/AgentNexus/internal/auth"
)

const (
	codeUnauthenticated  = "unauthenticated"
	codePermissionDenied = "permission_denied"
)

// Authenticator 是 HTTP 层对认证/授权的最小依赖（消费者定义接口，与 ServerRegistry 同风格）。
type Authenticator interface {
	// Authorize 返回 (nil, nil) 表示公开路径放行且无身份。
	Authorize(path, presented string) (*auth.Principal, error)
}

// withAuth 把整个 mux 包在认证之后：公开路径放行（不注入身份），其余请求必须先通过
// Authorize，认证发生在任何业务 handler（含 MCP 协议层）之前。
// Authenticator 为 nil（生产装配遗漏）时失败关闭：全部请求 403 并记一条 error 日志。
func withAuth(next http.Handler, authenticator Authenticator, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			logger.Error("no authenticator configured; rejecting request", "path", r.URL.Path)
			writeError(w, r, http.StatusForbidden, codePermissionDenied, "permission denied")

			return
		}

		principal, err := authenticator.Authorize(r.URL.Path, presentedSecret(r))

		switch {
		case err == nil && principal == nil:
			next.ServeHTTP(w, r)
		case err == nil:
			// 唯一的调用者传递 seam：审计、metrics 与请求日志都从 ctx 读取身份。
			setRecordPrincipal(r.Context(), *principal)
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), *principal)))
		case errors.Is(err, auth.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentnexus"`)
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		case errors.Is(err, auth.ErrPermissionDenied):
			writeError(w, r, http.StatusForbidden, codePermissionDenied, "permission denied")
		default:
			logger.Error("authenticate request", "path", r.URL.Path, "error", err)
			writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
		}
	})
}

// presentedSecret 解析调用方凭证：Authorization: Bearer 优先；非 Bearer scheme 不回退到
// X-API-Key（避免把 Basic 的 base64 当成密钥）；否则取 X-API-Key；都没有返回空串，
// 由 Authorizer 判为未认证。
func presentedSecret(r *http.Request) string {
	if value := r.Header.Get("Authorization"); value != "" {
		scheme, token, found := strings.Cut(value, " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}

		return ""
	}

	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}
