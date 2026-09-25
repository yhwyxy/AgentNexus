// 领域错误 → 审计/日志错误码的唯一分类点。
//
// 码取自详细设计 §11.1 的内部错误码；这里只引用既有 sentinel，不新增错误类型。
// 无法识别的错误一律归为 CodeInternal：既不泄漏细节，也让"分类漏了"在审计里可见。
package audit

import (
	"context"
	"errors"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// 详细设计 §11.1 的内部错误码（v0.1 只定义本服务实际会产出的子集）。
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeServerUnavailable  = "server_unavailable"
	CodeRuntimeFailed      = "runtime_failed"
	CodeBackendUnavailable = "backend_unavailable"
	CodeDeadlineExceeded   = "deadline_exceeded"
	CodeInternal           = "internal"
)

// ErrorCode 把错误链映射为审计用的错误码。nil 返回空串（成功没有错误码）。
//
// 注意：§11.1 没有 "cancelled" 码——调用被取消由事件 Outcome=cancelled 表达，
// 因此 context.Canceled 返回空串。
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return CodeDeadlineExceeded
	case errors.Is(err, server.ErrInvalid), errors.Is(err, tool.ErrInvalidTool):
		return CodeInvalidArgument
	case errors.Is(err, server.ErrNotFound),
		errors.Is(err, tool.ErrToolNotFound),
		errors.Is(err, tool.ErrSnapshotNotFound):
		return CodeNotFound
	case errors.Is(err, server.ErrAlreadyExists), errors.Is(err, server.ErrConflict),
		errors.Is(err, tool.ErrCatalogConflict),
		errors.Is(err, server.ErrNotRunnable):
		return CodeConflict
	case errors.Is(err, runtime.ErrServerDisabled),
		errors.Is(err, runtime.ErrDesiredStateStopped),
		errors.Is(err, runtime.ErrManagerClosed):
		return CodeServerUnavailable
	case errors.Is(err, runtime.ErrInvalidInstance):
		return CodeRuntimeFailed
	case errors.Is(err, runtime.ErrProviderNotFound):
		return CodeBackendUnavailable
	default:
		return CodeInternal
	}
}

// OutcomeOf 从错误推导事件结果：nil → success；调用被取消 → cancelled；其余 → error。
func OutcomeOf(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeSuccess
	case errors.Is(err, context.Canceled):
		return OutcomeCancelled
	default:
		return OutcomeError
	}
}
