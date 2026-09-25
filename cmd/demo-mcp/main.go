// demo-mcp 是 AgentNexus 的示例 MCP 后端:确定性工具集 + 可选的宿主挂载读取验证。
// 用途是 Compose / docker provider 的端到端冒烟,不是生产服务。
//
// 用法:
//
//	demo-mcp -transport stdio
//	demo-mcp -transport http -addr 0.0.0.0:8080 -path /mcp
//
// 日志一律走 stderr:stdio 模式下 stdout 属于 MCP 协议。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultReadRoot = "/workspace"
	readRootEnv     = "DEMO_MCP_READ_ROOT"
)

func main() {
	transport := flag.String("transport", "stdio", "transport: stdio | http")
	addr := flag.String("addr", "0.0.0.0:8080", "http listen address (transport=http)")
	path := flag.String("path", "/mcp", "http endpoint path (transport=http)")
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("demo-mcp ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *transport, *addr, *path); err != nil {
		log.Fatalf("exit: %v", err)
	}
}

func run(ctx context.Context, transport, addr, path string) error {
	server := newServer()
	switch transport {
	case "stdio":
		log.Print("stdio transport ready")
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		httpServer := &http.Server{Addr: addr, Handler: mux}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
		log.Printf("http transport listening on %s%s", addr, path)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve http: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", transport)
	}
}

// newServer 注册确定性工具集。demo.readfile 读取允许根目录下的文件,
// 让"宿主挂载确实生效"成为可由外部断言的事实。
func newServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agentnexus-demo-mcp", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "demo.echo", Description: "returns the supplied message"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	}) (*mcp.CallToolResult, any, error) {
		return textResult(args.Message), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "demo.fail", Description: "always returns a tool error"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("demo tool failure")
	})

	mcp.AddTool(server, &mcp.Tool{Name: "demo.readfile", Description: "reads a UTF-8 file below the allowed root"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Path string `json:"path"`
	}) (*mcp.CallToolResult, any, error) {
		content, err := readBelowRoot(readRoot(), args.Path)
		if err != nil {
			return nil, nil, err
		}
		return textResult(content), nil, nil
	})

	return server
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func readRoot() string {
	if root := os.Getenv(readRootEnv); root != "" {
		return root
	}
	return defaultReadRoot
}

// readBelowRoot 只读允许根目录内的文件:相对路径按根解析,绝对路径必须落在根内,
// 并且在解析符号链接之后再判一次(与 HostAccessPolicy 同口径,避免绕过)。
func readBelowRoot(root, requested string) (string, error) {
	if requested == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(requested, 0) {
		return "", errors.New("path must not contain NUL")
	}
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve read root: %w", err)
	}
	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(resolvedRoot, target)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the allowed root", requested)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	return string(content), nil
}
