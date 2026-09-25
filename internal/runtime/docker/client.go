// Package docker 把容器化的 MCP 后端转换成 SDK 无关的运行实例。
//
// 依赖边界:Moby Engine 客户端只允许出现在这个包(与 internal/mcpadapter 同理),
// 领域层与 service 层看不到任何 Docker 类型。Provider 通过 api 接口访问 Engine,
// 测试注入内存假实现,真实路径才构造 *client.Client。
package docker

import (
	"context"

	"github.com/moby/moby/client"
)

// api 是本包用到的 Engine 端点集合。签名与 *client.Client 一致,
// 只是把 ImageInspect 的可变参数收窄,便于用内存假实现替换。
type api interface {
	ImageInspect(ctx context.Context, ref string) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)

	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerAttach(ctx context.Context, id string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerLogs(ctx context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)

	Close() error
}

// engineClient 内嵌真实客户端,只覆盖签名需要收窄的方法;
// 其余方法由方法集提升直接满足 api,编译期保证两边一致。
type engineClient struct {
	*client.Client
}

var _ api = (*engineClient)(nil)

// newClient 构造真实客户端:host 为空时走环境变量(DOCKER_HOST,再退回平台默认 socket),
// 并开启 API 版本协商,避免把 daemon 版本钉死在代码里。
// 构造过程不连接 daemon(daemon 不可用不阻断控制面启动)。
func newClient(host string) (api, error) {
	if host == "" {
		created, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		return &engineClient{Client: created}, nil
	}
	created, err := client.New(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &engineClient{Client: created}, nil
}

// SocketPath 返回 host 指向的本机 socket 路径(tcp:// 等非 unix 地址返回空串),
// 供 host access 拒绝清单识别"不要把 daemon socket 挂进业务容器"。
func SocketPath(host string) string {
	if host == "" {
		return ""
	}
	parsed, err := client.ParseHostURL(host)
	if err != nil || parsed.Scheme != "unix" {
		return ""
	}
	return parsed.Path
}

func (c *engineClient) ImageInspect(ctx context.Context, ref string) (client.ImageInspectResult, error) {
	return c.Client.ImageInspect(ctx, ref)
}
