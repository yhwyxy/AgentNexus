// 观察者适配器：把 gateway/runtime 发射的领域事实翻译成指标。
//
// 与审计适配器（internal/observability.AuditObserver）并列，同属 adapter 层：
// 领域包只声明观察者接口，谁来实现、写到哪里都不由领域决定。
package metrics

import (
	"context"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// Observer 实现 gateway.CallObserver 与 runtime.Observer。
//
// 它只做计数：不写存储、不返回错误、不加锁之外的等待，
// 因此可以在调用链与运行态协调的关键路径上直接同步调用。
type Observer struct {
	metrics *Metrics
}

var (
	_ gateway.CallObserver = (*Observer)(nil)
	_ runtime.Observer     = (*Observer)(nil)
)

func NewObserver(m *Metrics) *Observer {
	return &Observer{metrics: m}
}

// ToolCalled 记录一次工具调用。失败只以网关自身的原因计数：
// 后端工具返回 isError=true 是合法结果（详细设计 §11.2），不计入 failures。
func (o *Observer) ToolCalled(_ context.Context, obs gateway.ToolCallObservation) {
	serverName := labelValue(obs.Route.ServerName)
	tool := labelValue(obs.PublicName)

	o.metrics.toolCalls.WithLabelValues(serverName, tool).Inc()

	if obs.Outcome == audit.OutcomeSuccess {
		return
	}
	o.metrics.toolFailures.WithLabelValues(serverName, tool, errorCodeLabel(obs.ErrorCode)).Inc()
}

// RuntimeRestarted 只统计「期望运行且已被替换」这一种情况：
// 首次启动、主动停止与协调失败都不是重启，没有对应指标族。
func (o *Observer) RuntimeRestarted(_ context.Context, srv server.Server, _, _ runtime.Instance) {
	o.metrics.restarts.WithLabelValues(labelValue(srv.Name)).Inc()
}

// 以下三个方法有意为空：7 条指标里没有对应族。
// 留空实现（而不是不实现接口）是为了让编译期断言继续保护接口一致性。
func (o *Observer) RuntimeStarted(context.Context, server.Server, runtime.Instance) {}

func (o *Observer) RuntimeStopped(context.Context, server.Server, runtime.Instance) {}

func (o *Observer) RuntimeFailed(context.Context, server.Server, error) {}
