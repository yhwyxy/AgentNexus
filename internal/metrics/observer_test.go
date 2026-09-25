package metrics_test

import (
	"context"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/metrics"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// TestToolCallFailureCounting 固定两条判废规则：
// 后端工具自身 isError=true 是合法结果（详细设计 §11.2），只有网关侧失败才计数；
// error_code 缺失时落到 "unknown"，不允许出现空 label。
func TestToolCallFailureCounting(t *testing.T) {
	meter, _ := newMeter(t)
	observer := metrics.NewObserver(meter)
	ctx := context.Background()

	observer.ToolCalled(ctx, gateway.ToolCallObservation{
		PublicName: "backend.demo.echo",
		Route:      tool.Route{ServerName: "backend"},
		Outcome:    audit.OutcomeSuccess,
	})
	// 后端工具返回 isError=true：Outcome 仍是 success，不得计入 failures。
	observer.ToolCalled(ctx, gateway.ToolCallObservation{
		PublicName: "backend.demo.fail",
		Route:      tool.Route{ServerName: "backend"},
		Outcome:    audit.OutcomeSuccess,
		IsError:    true,
	})
	// 未解析出路由（工具名不存在）：label 兜底为 unknown，且仍是失败。
	observer.ToolCalled(ctx, gateway.ToolCallObservation{
		PublicName: "ghost",
		Outcome:    audit.OutcomeError,
		ErrorCode:  "tool_not_found",
	})
	// 网关侧失败但没有分类错误码。
	observer.ToolCalled(ctx, gateway.ToolCallObservation{
		PublicName: "backend.demo.echo",
		Route:      tool.Route{ServerName: "backend"},
		Outcome:    audit.OutcomeError,
	})

	body := scrape(t, meter)
	assertContains(t, body, `mcp_tool_calls_total{server="backend",tool="backend.demo.echo"} 2`)
	assertContains(t, body, `mcp_tool_calls_total{server="backend",tool="backend.demo.fail"} 1`)
	assertContains(t, body, `mcp_tool_calls_total{server="unknown",tool="ghost"} 1`)
	// 判废规则：只有 Outcome=success 不计失败（详细设计 §11.2 的 isError 不变量）。
	// 前三次调用都不算失败：failures 族只有一条 series，且 error_code 兜底为 unknown。
	assertContains(t, body, `mcp_tool_call_failures_total{error_code="unknown",server="backend",tool="backend.demo.echo"} 1`)
	assertContains(t, body, `mcp_tool_call_failures_total{error_code="tool_not_found",server="unknown",tool="ghost"} 1`)
	assertAbsent(t, body, `mcp_tool_call_failures_total{error_code="unknown",server="backend",tool="backend.demo.fail"}`)
}

// TestRuntimeRestartOnlyCountsReplacement 断言首次启动、停止、协调失败都不产生重启计数。
func TestRuntimeRestartOnlyCountsReplacement(t *testing.T) {
	meter, _ := newMeter(t)
	observer := metrics.NewObserver(meter)
	ctx := context.Background()
	srv := readyServer("backend")
	instance := runtime.Instance{ID: "inst-1", ServerID: srv.ID}

	observer.RuntimeStarted(ctx, srv, instance)
	observer.RuntimeStopped(ctx, srv, instance)
	observer.RuntimeFailed(ctx, srv, context.DeadlineExceeded)

	body := scrape(t, meter)
	assertAbsent(t, body, "mcp_runtime_restarts_total")

	observer.RuntimeRestarted(ctx, srv, instance, runtime.Instance{ID: "inst-2", ServerID: srv.ID})
	body = scrape(t, meter)
	assertContains(t, body, `mcp_runtime_restarts_total{server="backend"} 1`)
}
