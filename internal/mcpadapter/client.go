// 官方 MCP SDK 的唯一客户端适配层。
package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
)

type Connector struct {
	Implementation *mcp.Implementation
	HTTPClient     *http.Client
}

func NewConnector() *Connector {
	return &Connector{Implementation: &mcp.Implementation{Name: "agentnexus", Version: "0.1.0"}}
}

func (c *Connector) Connect(ctx context.Context, target runtime.ConnectTarget) (mcpclient.Session, error) {
	if target.Transport != "streamable_http" || target.URL == "" {
		return nil, fmt.Errorf("unsupported MCP target")
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	if len(target.Headers) != 0 {
		client = &http.Client{Transport: headerTransport{base: client.Transport, headers: target.Headers}, Timeout: client.Timeout, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
	}
	transport := &mcp.StreamableClientTransport{Endpoint: target.URL, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1}
	session, err := c.ImplementationClient().Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize MCP session: %w", err)
	}
	return &sessionAdapter{session: session}, nil
}

func (c *Connector) ImplementationClient() *mcp.Client {
	return mcp.NewClient(c.Implementation, nil)
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	copyReq := req.Clone(req.Context())
	for key, value := range t.headers {
		copyReq.Header.Set(key, value)
	}
	return base.RoundTrip(copyReq)
}

type sessionAdapter struct{ session *mcp.ClientSession }

func (s *sessionAdapter) ListTools(ctx context.Context, cursor string) ([]mcpclient.Tool, string, error) {
	result, err := s.session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
	if err != nil {
		return nil, "", err
	}
	tools := make([]mcpclient.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		input, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, "", fmt.Errorf("marshal input schema: %w", err)
		}
		var output json.RawMessage
		if tool.OutputSchema != nil {
			output, err = json.Marshal(tool.OutputSchema)
			if err != nil {
				return nil, "", fmt.Errorf("marshal output schema: %w", err)
			}
		}
		annotations, err := json.Marshal(tool.Annotations)
		if err != nil {
			return nil, "", fmt.Errorf("marshal annotations: %w", err)
		}
		tools = append(tools, mcpclient.Tool{Name: tool.Name, Title: tool.Title, Description: tool.Description, InputSchema: input, OutputSchema: output, Annotations: annotations})
	}
	return tools, result.NextCursor, nil
}

func (s *sessionAdapter) CallTool(ctx context.Context, req mcpclient.CallRequest) (mcpclient.CallResult, error) {
	var args map[string]any
	if len(req.Arguments) != 0 {
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return mcpclient.CallResult{}, fmt.Errorf("decode tool arguments: %w", err)
		}
	}
	result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: req.Name, Arguments: args})
	if err != nil {
		return mcpclient.CallResult{}, err
	}
	contents := make([]mcpclient.Content, 0, len(result.Content))
	for _, content := range result.Content {
		var text string
		if textContent, ok := content.(*mcp.TextContent); ok {
			text = textContent.Text
		}
		contents = append(contents, mcpclient.Content{Type: "text", Text: text})
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("marshal structured content: %w", err)
	}
	return mcpclient.CallResult{Content: contents, StructuredContent: structured, IsError: result.IsError}, nil
}

func (s *sessionAdapter) Ping(ctx context.Context) error {
	return s.session.Ping(ctx, nil)
}

func (s *sessionAdapter) Close() error { return s.session.Close() }
