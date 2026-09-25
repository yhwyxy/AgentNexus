// 请求 ID 的生成与上下文传递。放在 observability 而非 edge：审计适配器、
// metrics 与请求日志都要从同一处读取，且这里不依赖 net/http。
package observability

import (
	"context"

	"github.com/google/uuid"
)

type requestIDContextKey struct{}

// WithRequestID 把请求 ID 写入上下文。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFrom 读取上下文里的请求 ID；未设置（如后台巡检）返回 false。
func RequestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDContextKey{}).(string)
	return id, ok
}

// NewRequestID 生成一个请求 ID，用于没有携带合法 X-Request-Id 的请求。
func NewRequestID() string {
	return uuid.NewString()
}
