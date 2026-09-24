// 运行时接口只表达连接目标和观测状态，不暴露具体进程、Docker 或 MCP SDK 类型。
package runtime

import (
	"context"
	"io"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

type ConnectTarget struct {
	Transport string
	URL       string
	Command   string
	Args      []string
	Env       []string
	Headers   map[string]string
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
