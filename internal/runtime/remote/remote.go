// RemoteProvider 将已存在的远程 MCP HTTP 服务转换为 SDK 无关的连接目标。
package remote

import (
	"context"
	"fmt"
	"io"
	"net/url"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

type Provider struct{}

func NewProvider() *Provider { return &Provider{} }

func (p *Provider) Type() server.RuntimeType { return server.RuntimeRemote }

func (p *Provider) Ensure(_ context.Context, srv server.Server) (runtime.Instance, error) {
	if srv.Spec.Runtime.Type != server.RuntimeRemote || srv.Spec.Runtime.Remote == nil {
		return runtime.Instance{}, fmt.Errorf("remote provider requires remote runtime")
	}
	raw := srv.Spec.Runtime.Remote.Endpoint
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil {
		return runtime.Instance{}, fmt.Errorf("invalid remote endpoint")
	}
	headers := cloneHeaders(srv.Spec.Runtime.Remote.Headers)
	return runtime.Instance{
		ID:               string(srv.ID) + "-remote",
		ServerID:         srv.ID,
		Provider:         string(server.RuntimeRemote),
		ExternalID:       raw,
		Phase:            "running",
		ObservedRevision: srv.Revision,
		Target: runtime.ConnectTarget{
			Transport: string(server.TransportStreamableHTTP), URL: raw, Headers: headers,
		},
	}, nil
}

func (p *Provider) Stop(_ context.Context, instance runtime.Instance) error {
	if instance.Provider != string(server.RuntimeRemote) {
		return fmt.Errorf("remote provider cannot stop %q instance", instance.Provider)
	}
	return nil
}

func (p *Provider) Inspect(_ context.Context, instance runtime.Instance) (runtime.Instance, error) {
	if instance.Provider != string(server.RuntimeRemote) || instance.Target.URL == "" {
		return runtime.Instance{}, fmt.Errorf("invalid remote instance")
	}
	return instance, nil
}

func (p *Provider) Logs(_ context.Context, _ runtime.Instance, _ runtime.LogOptions) (io.ReadCloser, error) {
	return nil, fmt.Errorf("remote runtime has no logs")
}

func cloneHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
