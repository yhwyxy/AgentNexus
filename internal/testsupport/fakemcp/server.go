// 测试用的确定性 MCP 服务，不用于生产启动路径。
package fakemcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	HTTP    *httptest.Server
	mu      sync.Mutex
	calls   int
	lists   int
	server  *mcp.Server
	handler http.Handler
}

func New() *Server {
	fake := &Server{}
	fake.server = newMCPServer(func(method string) {
		switch method {
		case "tools/list":
			fake.recordList()
		case "tools/call":
			fake.recordCall()
		}
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fake.server }, nil)
	fake.handler = handler
	fake.HTTP = httptest.NewServer(handler)
	return fake
}

// ServeStdio 在 stdin/stdout 上运行与 HTTP fake 相同的工具集,日志走 stderr。
// 供"子进程方式的 stdio 后端"使用:调用方通常直接 os.Exit(0),避免 testing
// 框架往 stdout 打印内容破坏 MCP 协议。
//
// 设置 AGENTNEXUS_FAKE_STDIO_PIDFILE 时会把自身 pid 写进去,便于集成测试验证
// 子进程确实被回收。
func ServeStdio(ctx context.Context) error {
	if path := os.Getenv("AGENTNEXUS_FAKE_STDIO_PIDFILE"); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return fmt.Errorf("write fake stdio pid file: %w", err)
		}
	}
	fmt.Fprintln(os.Stderr, "fakemcp stdio ready")
	return newMCPServer(func(string) {}).Run(ctx, &mcp.StdioTransport{})
}

// newMCPServer 注册确定性工具集,并通过 record 回调上报 tools/list、tools/call 次数。
func newMCPServer(record func(method string)) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agentnexus-fake", Version: "1.0.0"}, nil)
	// 统计 tools/list 次数:调用方需要验证"确实重新拉取了目录",
	// 而不是复用了已有快照。
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				record("tools/list")
			}
			return next(ctx, method, req)
		}
	})
	mcp.AddTool(server, &mcp.Tool{Name: "demo.echo", Description: "returns the supplied message"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	}) (*mcp.CallToolResult, any, error) {
		record("tools/call")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Message}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "demo.fail", Description: "returns a tool error"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		record("tools/call")
		return nil, nil, fmt.Errorf("fake tool failure")
	})
	return server
}

func (s *Server) AddSlowTool() {
	mcp.AddTool(s.server, &mcp.Tool{Name: "demo.slow", Description: "waits until its deadline"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		s.recordCall()
		select {
		case <-time.After(300 * time.Millisecond):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "completed"}}}, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
}

func (s *Server) recordCall() {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
}

func (s *Server) recordList() {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
}

// CallCount 返回 tools/call 次数。
func (s *Server) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ListToolsCount 返回 tools/list 次数。
func (s *Server) ListToolsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// Handler 暴露 MCP HTTP handler，供需要自行包装（例如先注入故障）的测试使用。
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Close() { s.HTTP.Close() }
