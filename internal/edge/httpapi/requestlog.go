// 请求级结构化日志：为每个请求生成/校验 request_id，记录 status、耗时、
// 调用者与错误码。它是唯一读取 auth.Principal 与错误码的地方（写入方分别是
// withAuth 与 writeError），也是 /mcp 与审计事件共享的请求上下文来源。
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/auth"
	"github.com/yhwyxy/AgentNexus/internal/observability"
)

const requestIDHeader = "X-Request-Id"

// requestIDPattern 限制入站 X-Request-Id 的字符集与长度：
// 复用它跨服务串联日志是必要的，但任意字符串（换行、控制字符）会污染日志行。
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// requestRecord 是请求日志与错误码的单次请求暂存区：middleware 创建后放进 ctx，
// withAuth 写入调用者、writeError 写入错误码，日志发射时再读回来。
// 没有任何跨包耦合，也不需要二次解析响应体。
type requestRecord struct {
	principalName string
	principalRole string
	errorCode     string
}

type requestRecordKey struct{}

func requestRecordFrom(ctx context.Context) *requestRecord {
	record, _ := ctx.Value(requestRecordKey{}).(*requestRecord)
	return record
}

// setRecordPrincipal / setRecordErrorCode 由 withAuth 与 writeError 调用；
// 没有日志中间件时（例如直接测试 handler）记录为空，写入被静默忽略。
func setRecordPrincipal(ctx context.Context, principal auth.Principal) {
	if record := requestRecordFrom(ctx); record != nil {
		record.principalName = principal.Name
		record.principalRole = string(principal.Role)
	}
}

func setRecordErrorCode(ctx context.Context, code string) {
	if record := requestRecordFrom(ctx); record != nil {
		record.errorCode = code
	}
}

// requestLogOptions 是日志与指标采集的装配参数。
type requestLogOptions struct {
	// mux 用于取回本次请求命中的注册 pattern；nil 时 route 记为 unmatched。
	mux      *http.ServeMux
	patterns routePatterns
	// metrics 为 nil 时不采集请求指标。
	metrics RequestMetrics
	logger  *slog.Logger
}

// withRequestLog 是最外层中间件：认证失败（401/403）的请求也要有 request_id、
// status 与 error_code，因此它必须包在 withAuth 之外。
func withRequestLog(next http.Handler, opts requestLogOptions) http.Handler {
	logger := opts.logger
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(requestIDHeader)
		if !validRequestID(requestID) {
			requestID = observability.NewRequestID()
		}
		// 无论复用还是新生成都回写：调用方可据此在自己的日志里定位本次请求。
		w.Header().Set(requestIDHeader, requestID)

		record := &requestRecord{}
		ctx := observability.WithRequestID(r.Context(), requestID)
		ctx = context.WithValue(ctx, requestRecordKey{}, record)

		// route 在进入 handler 前解析：它就是 mux 的注册 pattern（有界取值）。
		route := opts.patterns.label(opts.mux, r)

		status := &statusWriter{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(status, r.WithContext(ctx))
		duration := time.Since(started)

		if opts.metrics != nil {
			observeRequest(opts.metrics, logger, r.Method, route, status.code(), duration, record.errorCode)
		}

		logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"route", route,
			"status", status.code(),
			"duration_ms", duration.Milliseconds(),
			"bytes", status.written,
			"request_id", requestID,
			"principal", record.principalName,
			"principal_role", record.principalRole,
			"error_code", record.errorCode,
			"remote_addr", r.RemoteAddr,
		)
	})
}

// statusWriter 记录状态码与响应字节数，并把底层 writer 的附加能力透传出去。
//
// Unwrap 不是可选项：MCP 流式 HTTP 用 http.NewResponseController 推送 SSE，
// ResponseController 通过 Unwrap 找到底层的 Flusher。少了它，/mcp 的流式响应
// 会静默退化成一次性写入。
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.written += n

	return n, err
}

// Unwrap 暴露底层 ResponseWriter，供 http.ResponseController 使用。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}

	return w.status
}
