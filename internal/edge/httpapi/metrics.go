// 指标采集装配：/metrics 端点、请求指标接口与 route label 推导。
//
// route label 必须来自 mux 的注册 pattern，而不是 URL 路径：管理 API 的路径里
// 含资产 ID，若按路径打点，每注册一个 Server 就会多出一批永久序列。
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// routeUnmatched 是所有未命中注册 pattern 的请求共用的 route label。
// mux.Handler 在「未匹配」与「路径规范化重定向」两种情况下都返回空 pattern，
// 因此空 pattern 绝不能回退成原始路径。
const routeUnmatched = "unmatched"

// RequestMetrics 接收每个已结束的 HTTP 请求的事实（消费者定义接口）。
//
// 参数只用 stdlib 类型：实现方是 internal/metrics，但本包不需要认识它，
// 避免 edge 依赖 adapter 的类型。实现方必须快速返回，不得阻塞请求链；
// panic 由 observeRequest 拦截，不会影响请求结果。
type RequestMetrics interface {
	ObserveRequest(method, route string, status int, duration time.Duration, errorCode string)
}

// observeRequest 调用采集方并隔离其 panic：可观测性故障绝不能变成请求失败。
// 开销是每次请求一个 defer，与一次 JSON 序列化无关，可接受。
func observeRequest(observer RequestMetrics, logger *slog.Logger, method, route string, status int, duration time.Duration, errorCode string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("request metrics observer panicked",
				"panic", recovered, "method", method, "route", route)
		}
	}()

	observer.ObserveRequest(method, route, status, duration, errorCode)
}

// routePatterns 是构造时登记的 pattern → route label 映射。
// 它同时充当白名单：只有登记过的 pattern 才能成为 label 取值。
type routePatterns map[string]string

func newRoutePatterns() routePatterns {
	return make(routePatterns)
}

// add 登记一个注册 pattern。带方法前缀的 pattern（"GET /health/live"）
// 去掉方法后作为 label，因为 method 已经是独立 label。
func (p routePatterns) add(pattern string) {
	p[pattern] = stripMethod(pattern)
}

// label 返回本次请求应计入的 route label。mux 为 nil（测试直接调 middleware）时
// 一律记为 unmatched。
func (p routePatterns) label(mux *http.ServeMux, r *http.Request) string {
	if mux == nil {
		return routeUnmatched
	}

	_, pattern := mux.Handler(r)
	if label, ok := p[pattern]; ok {
		return label
	}

	return routeUnmatched
}

func stripMethod(pattern string) string {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		return pattern
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return path
	default:
		return pattern
	}
}
