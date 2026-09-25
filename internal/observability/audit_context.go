// 审计事件的请求上下文补全：把「谁、在哪个请求里」接到领域事实上。
//
// 这两个字段都只存在于请求 ctx（withAuth 写入身份、withRequestLog 写入 request_id），
// 因此读取规则只有这一份：HTTP 管理动作、工具调用与 Runtime 事件都经它补齐，
// 后台任务（周期巡检）没有请求上下文，actor 与 request_id 留空。
package observability

import (
	"context"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/auth"
)

// EnrichAuditInput 用 ctx 里的调用者与请求 ID 补全审计输入。
// 它只补空字段之外的既有值，不校验事件本身（校验是 audit.Recorder 的职责）。
func EnrichAuditInput(ctx context.Context, in audit.Input) audit.Input {
	if principal, ok := auth.PrincipalFrom(ctx); ok {
		in.ActorName = principal.Name
		in.ActorRole = string(principal.Role)
	}
	if requestID, ok := RequestIDFrom(ctx); ok {
		in.RequestID = requestID
	}

	return in
}
