package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
)

func TestConnectorListAndCallFakeServer(t *testing.T) {
	fake := fakemcp.New()
	defer fake.Close()

	connector := NewConnector()
	session, err := connector.Connect(context.Background(), runtime.ConnectTarget{
		Transport: "streamable_http",
		URL:       fake.HTTP.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, next, err := session.ListTools(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(tools) != 2 {
		t.Fatalf("tools = %#v, next = %q", tools, next)
	}

	result, err := session.CallTool(ctx, mcpclient.CallRequest{
		Name:      "demo.echo",
		Arguments: json.RawMessage(`{"message":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

// TestConnectorStdioTransport 用内存管道模拟"进程 Provider 交出的标准流":
// Connector 必须把它们当成 MCP 传输,并且 session Close 之后两端都被关闭。
func TestConnectorStdioTransport(t *testing.T) {
	clientToServerReader, clientToServerWriter := io.Pipe()
	serverToClientReader, serverToClientWriter := io.Pipe()

	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-fake", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "demo.echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Message}}}, nil, nil
	})
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(serverCtx, &mcp.IOTransport{Reader: clientToServerReader, Writer: serverToClientWriter})
	}()

	streams := &runtime.Streams{Stdin: clientToServerWriter, Stdout: serverToClientReader}
	session, err := NewConnector().Connect(context.Background(), runtime.ConnectTarget{
		Transport: "stdio",
		Command:   "/bin/echo",
		Streams:   streams,
	})
	if err != nil {
		t.Fatalf("connect over stdio streams: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, _, err := session.ListTools(ctx, "")
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "demo.echo" {
		t.Fatalf("tools = %#v", tools)
	}
	result, err := session.CallTool(ctx, mcpclient.CallRequest{Name: "demo.echo", Arguments: json.RawMessage(`{"message":"over-stdio"}`)})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "over-stdio" {
		t.Fatalf("unexpected result: %#v", result)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	// 关闭 session 必须关掉两端的包装:进程 Provider 依赖这个信号判断实例失效。
	if _, err := serverToClientReader.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stdout reader read after close = %v, want ErrClosedPipe", err)
	}
	if _, err := clientToServerWriter.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stdin writer write after close = %v, want ErrClosedPipe", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio server did not stop after the client closed its streams")
	}
}

func TestConnectorRejectsInvalidTargets(t *testing.T) {
	connector := NewConnector()
	targets := map[string]runtime.ConnectTarget{
		"unknown transport":      {Transport: "carrier-pigeon"},
		"streamable without url": {Transport: "streamable_http"},
		"stdio without streams":  {Transport: "stdio"},
		"stdio with partial streams": {
			Transport: "stdio",
			Streams:   &runtime.Streams{Stdin: nopWriteCloser{}, Stdout: nil},
		},
	}
	for name, target := range targets {
		if _, err := connector.Connect(context.Background(), target); err == nil {
			t.Fatalf("%s: connect must fail", name)
		}
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
