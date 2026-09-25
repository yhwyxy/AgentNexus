package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/app"
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

// 注册 → 异步协调（RuntimeManager.EnsureReady → 分页 tools/list → ReplaceSnapshot）
// → Catalog 失效 → /mcp 聚合可见。使用真实 SQLite、真实 MCP handler 与本地 fake 后端。
func TestRegisterTriggersAsyncToolSyncAndCatalogPublish(t *testing.T) {
	ctx := context.Background()
	backend := fakemcp.New()
	defer backend.Close()

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	servers := sqlite.NewServerRepository(db)
	registry := server.NewService(servers).WithIDGenerator(func() string { return "srv-1" })
	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo)
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	defer clients.Close(ctx)
	runtimes, err := runtime.NewManager(servers, remote.NewProvider())
	if err != nil {
		t.Fatalf("create runtime manager: %v", err)
	}
	lifecycle, err := app.NewLifecycle(app.LifecycleOptions{
		Registry:          registry,
		Syncer:            tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog),
		Lister:            servers,
		ReconcileInterval: 10 * time.Millisecond,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
	defer closeLifecycle(t, lifecycle)

	caller, err := gateway.NewToolCaller(catalog, servers, runtimes, clients)
	if err != nil {
		t.Fatalf("create tool caller: %v", err)
	}
	virtual, err := gateway.NewVirtualServer(catalog, caller)
	if err != nil {
		t.Fatalf("create virtual server: %v", err)
	}
	mcpHandler, err := mcpserver.NewHandler(virtual)
	if err != nil {
		t.Fatalf("create MCP handler: %v", err)
	}
	httpServer := httptest.NewServer(httpapi.NewHandler(httpapi.Options{
		Registry:      lifecycle,
		Refresher:     lifecycle,
		MCP:           mcpHandler,
		Authenticator: testAuthorizer(t),
		Logger:        testLogger(),
	}))
	defer httpServer.Close()

	body := fmt.Sprintf(`{"name":"backend","transport":"streamable_http",
		"runtime":{"type":"remote","remote":{"endpoint":%q}}}`, backend.HTTP.URL)
	resp := doAuthorized(t, http.MethodPost, httpServer.URL+"/api/v1/mcp-servers", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
	var created struct {
		ID     string `json:"id"`
		Status struct {
			Phase            string `json:"phase"`
			ObservedRevision int64  `json:"observedRevision"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if created.ID != "srv-1" || created.Status.Phase != string(server.PhasePending) || created.Status.ObservedRevision != 0 {
		t.Fatalf("registration response = %#v, want pending srv-1", created)
	}

	// 运行态由后台协调收敛：ready 且 observedRevision 跟上当前 revision。
	deadline := time.Now().Add(10 * time.Second)
	for {
		phase, observed := getServerStatus(t, httpServer.URL, "srv-1")
		if phase == string(server.PhaseReady) {
			if observed != 1 {
				t.Fatalf("observedRevision = %d, want 1", observed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server status = %q after %s, want ready", phase, testTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "app-test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, streamableTransport(httpServer.URL+"/mcp", testAdminSecret), nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer session.Close()

	// ready 必须意味着快照已经发布：ready 后立即列一次 tools/list 即可见，
	// 不允许出现"ready 但目录为空"的中间态。
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, listedTool := range listed.Tools {
		names = append(names, listedTool.Name)
	}
	want := []string{"backend.demo.echo", "backend.demo.fail"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("tools/list immediately after ready = %v, want %v", names, want)
	}
}

func getServerStatus(t *testing.T, baseURL, id string) (string, int64) {
	t.Helper()
	resp := doAuthorized(t, http.MethodGet, baseURL+"/api/v1/mcp-servers/"+id, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET status = %d; body: %s", resp.StatusCode, raw)
	}
	var got struct {
		Status struct {
			Phase            string `json:"phase"`
			ObservedRevision int64  `json:"observedRevision"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode server response: %v", err)
	}
	return got.Status.Phase, got.Status.ObservedRevision
}
