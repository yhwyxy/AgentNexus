// 运行时接口只表达连接目标和观测状态，不暴露具体进程、Docker 或 MCP SDK 类型。
package runtime

import (
	"context"
	"io"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

// Streams 是 stdio 连接专用的标准流:由进程类 Provider 创建,只在内存中存在,
// 不落库、不序列化。Close 只表示"本次连接结束",子进程 fd 始终由 Provider 持有。
type Streams struct {
	Stdin  io.WriteCloser // 写入子进程 stdin
	Stdout io.ReadCloser  // 读取子进程 stdout
}

type ConnectTarget struct {
	Transport string
	URL       string
	Command   string
	Args      []string
	Env       []string
	Headers   map[string]string
	Streams   *Streams // 仅 stdio/process 使用,remote 为 nil
}

type Instance struct {
	ID               string
	ServerID         server.ID
	Provider         string
	ExternalID       string
	Phase            string
	Target           ConnectTarget
	ObservedRevision int64
}

type LogOptions struct {
	Follow bool
}

type Provider interface {
	Type() server.RuntimeType
	Ensure(context.Context, server.Server) (Instance, error)
	Stop(context.Context, Instance) error
	Inspect(context.Context, Instance) (Instance, error)
	Logs(context.Context, Instance, LogOptions) (io.ReadCloser, error)
}

type Manager interface {
	EnsureReady(context.Context, server.Server) (Instance, error)
	Stop(context.Context, server.ID) error
	Reconcile(context.Context, server.ID) error
}

// Releaser 由持有进程内资源的 Provider 实现(如本机子进程、fd);
// ProviderManager.Close 在停止活动实例后调用它做最终回收。
type Releaser interface {
	Close(context.Context) error
}
