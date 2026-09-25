// 观察者扇出：领域包只接受一个观察者，而审计与 metrics 都要收到同一批事实。
//
// 放在本包（adapter 层）而不是领域包，是为了让 runtime/gateway 保持
// 「一个接口、一个调用点」的简单形状；扇出顺序即声明顺序。
package observability

import (
	"context"

	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// MultiRuntimeObserver 顺序分发运行态事件。nil 元素被跳过，
// 方便装配处按配置省略某个消费者。
type MultiRuntimeObserver []runtime.Observer

var _ runtime.Observer = MultiRuntimeObserver(nil)

func (m MultiRuntimeObserver) RuntimeStarted(ctx context.Context, srv server.Server, instance runtime.Instance) {
	for _, observer := range m {
		if observer != nil {
			observer.RuntimeStarted(ctx, srv, instance)
		}
	}
}

func (m MultiRuntimeObserver) RuntimeRestarted(ctx context.Context, srv server.Server, previous, current runtime.Instance) {
	for _, observer := range m {
		if observer != nil {
			observer.RuntimeRestarted(ctx, srv, previous, current)
		}
	}
}

func (m MultiRuntimeObserver) RuntimeStopped(ctx context.Context, srv server.Server, instance runtime.Instance) {
	for _, observer := range m {
		if observer != nil {
			observer.RuntimeStopped(ctx, srv, instance)
		}
	}
}

func (m MultiRuntimeObserver) RuntimeFailed(ctx context.Context, srv server.Server, cause error) {
	for _, observer := range m {
		if observer != nil {
			observer.RuntimeFailed(ctx, srv, cause)
		}
	}
}

// MultiCallObserver 顺序分发工具调用事实。
type MultiCallObserver []gateway.CallObserver

var _ gateway.CallObserver = MultiCallObserver(nil)

func (m MultiCallObserver) ToolCalled(ctx context.Context, obs gateway.ToolCallObservation) {
	for _, observer := range m {
		if observer != nil {
			observer.ToolCalled(ctx, obs)
		}
	}
}
