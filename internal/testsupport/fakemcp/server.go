// 测试用的确定性 MCP 服务，不用于生产启动路径。
package fakemcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	HTTP   *httptest.Server
	Calls  int
	server *mcp.Server
}

func New() *Server {
	fake := &Server{}
	fake.server = mcp.NewServer(&mcp.Implementation{Name: "agentnexus-fake", Version: "1.0.0"}, nil)
	mcp.AddTool(fake.server, &mcp.Tool{Name: "demo.echo", Description: "returns the supplied message"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Message}}}, nil, nil
	})
	mcp.AddTool(fake.server, &mcp.Tool{Name: "demo.fail", Description: "returns a tool error"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return nil, nil, fmt.Errorf("fake tool failure")
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fake.server }, nil)
	fake.HTTP = httptest.NewServer(handler)
	return fake
}

func (s *Server) Close() { s.HTTP.Close() }
