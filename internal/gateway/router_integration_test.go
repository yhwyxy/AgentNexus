package gateway_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	mcpserver "github.com/yhwyxy/AgentNexus/internal/mcpadapter/server"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/remote"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func TestStreamableMCPGatewayListsValidatesAndCallsBackend(t *testing.T) {
	ctx := context.Background()
	backend := fakemcp.New()
	backend.AddSlowTool()
	defer backend.Close()

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	servers := sqlite.NewServerRepository(db)
	registered, err := server.NewService(servers).WithIDGenerator(func() string { return "gateway-test" }).Register(ctx, server.RegisterInput{
		Name: "backend", Transport: server.TransportStreamableHTTP,
		Runtime:  server.RuntimeSpec{Type: server.RuntimeRemote, Remote: &server.RemoteSpec{Endpoint: backend.HTTP.URL}},
		Timeouts: server.TimeoutSpec{Call: 25 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo)
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	defer clients.Close(ctx)
	runtimes, err := runtime.NewManager(servers, remote.NewProvider())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog).Sync(ctx, registered.ID); err != nil {
		t.Fatal(err)
	}
	caller, err := gateway.NewToolCaller(catalog, servers, runtimes, clients)
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := gateway.NewVirtualServer(catalog, caller)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mcpserver.NewHandler(virtual)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "gateway-test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: testHTTPServer(t, handler)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 3 || listed.Tools[0].Name != "backend.demo.echo" {
		t.Fatalf("tools/list = %#v, want public names including backend.demo.echo", listed.Tools)
	}

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.echo", Arguments: map[string]any{"message": 42}})
	if err == nil {
		t.Fatal("invalid argument was accepted")
	}
	if got := backend.CallCount(); got != 0 {
		t.Fatalf("invalid arguments reached backend %d times, want 0", got)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.echo", Arguments: map[string]any{"message": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "hello" {
		t.Fatalf("tools/call result = %#v", result)
	}
	if got := backend.CallCount(); got != 1 {
		t.Fatalf("backend calls = %d, want 1", got)
	}
	started := time.Now()
	slow, slowErr := session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.slow", Arguments: map[string]any{}})
	if slowErr == nil && !slow.IsError {
		t.Fatalf("slow tools/call = %#v, %v; want deadline error", slow, slowErr)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("slow call took %s, want configured 25ms deadline", elapsed)
	}
	if got := backend.CallCount(); got != 2 {
		t.Fatalf("backend calls after timeout = %d, want echo plus slow", got)
	}

	failed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "backend.demo.fail", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !failed.IsError || len(failed.Content) != 1 || failed.Content[0].(*mcp.TextContent).Text != "tool execution failed" {
		t.Fatalf("backend failure result = %#v, want opaque tool error", failed)
	}
}

func testHTTPServer(t *testing.T, handler *mcpserver.Handler) string {
	t.Helper()
	mux := httpapi.NewHandlerWithMCP(nil, handler, nil)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return httpServer.URL + "/mcp"
}
