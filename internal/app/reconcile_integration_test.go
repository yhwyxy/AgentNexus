package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// appStack 是一套完整装配（SQLite + 真实 MCP handler + 后台生命周期），
// 供重启、后端故障恢复等跨进程行为验证使用。
type appStack struct {
	url       string
	lifecycle *app.Lifecycle
	close     func()
}

func startStack(t *testing.T, dbPath string, reconcileInterval time.Duration) *appStack {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	servers := sqlite.NewServerRepository(db)
	registry := server.NewService(servers)
	catalog := tool.NewCatalog(sqlite.NewToolRepository(db))
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	runtimes, err := runtime.NewManager(servers, remote.NewProvider())
	if err != nil {
		t.Fatalf("create runtime manager: %v", err)
	}
	lifecycle, err := app.NewLifecycle(app.LifecycleOptions{
		Registry:          registry,
		Syncer:            tool.NewSyncService(servers, runtimes, clients, sqlite.NewToolRepository(db), catalog),
		Lister:            servers,
		ReconcileInterval: reconcileInterval,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
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
		Registry:  lifecycle,
		Refresher: lifecycle,
		MCP:       mcpHandler,
		Logger:    testLogger(),
	}))

	return &appStack{
		url:       httpServer.URL,
		lifecycle: lifecycle,
		close: func() {
			httpServer.Close()
			closeLifecycle(t, lifecycle)
			_ = clients.Close(context.Background())
			_ = db.Close()
		},
	}
}

// gate 在后端"未就绪"时对全部 MCP 请求返回 503，用于模拟注册时后端尚不可用。
type gate struct {
	ready atomic.Bool
	next  http.Handler
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.ready.Load() {
		http.Error(w, "backend not ready", http.StatusServiceUnavailable)
		return
	}
	g.next.ServeHTTP(w, r)
}

func registerServer(t *testing.T, baseURL, name, endpoint string, enabled *bool) string {
	t.Helper()
	enabledField := ""
	if enabled != nil {
		enabledField = fmt.Sprintf(`"enabled":%t,`, *enabled)
	}
	body := fmt.Sprintf(`{%s"name":%q,"transport":"streamable_http",
		"runtime":{"type":"remote","remote":{"endpoint":%q}}}`, enabledField, name, endpoint)

	resp, err := http.Post(baseURL+"/api/v1/mcp-servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("register request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("registration response has no id")
	}
	return created.ID
}

func waitForPhase(t *testing.T, baseURL, id string, want server.Phase) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var observed int64
	for {
		phase, got := getServerStatus(t, baseURL, id)
		if phase == string(want) {
			return got
		}
		observed = got
		if time.Now().After(deadline) {
			t.Fatalf("server %s phase = %q (observedRevision %d) after %s, want %q",
				id, phase, observed, testTimeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForReady(t *testing.T, baseURL, id string) {
	t.Helper()
	if observed := waitForPhase(t, baseURL, id, server.PhaseReady); observed != 1 {
		t.Fatalf("observedRevision = %d, want 1", observed)
	}
}

func listToolNames(t *testing.T, httpURL string) []string {
	t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "app-test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpURL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, listedTool := range listed.Tools {
		names = append(names, listedTool.Name)
	}
	return names
}

func waitForListIncrease(t *testing.T, backend *fakemcp.Server, before int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if backend.ListToolsCount() > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend tools/list count stayed at %d after %s, want a reload", before, testTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 首次同步失败（后端 503）后必须由巡检自动重试，恢复后无需人工干预即可 ready。
func TestReconcilerRecoversAfterBackendBecomesAvailable(t *testing.T) {
	backend := fakemcp.New()
	defer backend.Close()
	gated := &gate{next: backend.Handler()}
	backendServer := httptest.NewServer(gated)
	defer backendServer.Close()

	stack := startStack(t, filepath.Join(t.TempDir(), "app.db"), 10*time.Millisecond)
	defer stack.close()

	id := registerServer(t, stack.url, "backend", backendServer.URL, nil)
	if observed := waitForPhase(t, stack.url, id, server.PhaseDegraded); observed != 0 {
		t.Fatalf("observedRevision = %d while degraded, want 0", observed)
	}

	gated.ready.Store(true)
	waitForReady(t, stack.url, id)

	got := listToolNames(t, stack.url)
	want := []string{"backend.demo.echo", "backend.demo.fail"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tools/list = %v, want %v", got, want)
	}
}

// 重启后实例缓存为空：DB 里的 ready 不代表运行态存在，启动重放必须重新建立实例并拉取目录。
func TestStartupReplayReconcilesServerAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	backend := fakemcp.New()
	defer backend.Close()

	first := startStack(t, dbPath, 10*time.Millisecond)
	id := registerServer(t, first.url, "backend", backend.HTTP.URL, nil)
	waitForReady(t, first.url, id)
	first.close()

	before := backend.ListToolsCount()
	second := startStack(t, dbPath, 10*time.Millisecond)
	defer second.close()

	// 启动重放不依赖任何客户端交互：目录被重新拉取即为证据。
	waitForListIncrease(t, backend, before)
	waitForReady(t, second.url, id)
}

// POST /api/v1/mcp-servers/{id}:refresh-tools 排队一次真实刷新。
func TestRefreshToolsEndpointTriggersReload(t *testing.T) {
	backend := fakemcp.New()
	defer backend.Close()

	stack := startStack(t, filepath.Join(t.TempDir(), "app.db"), 10*time.Millisecond)
	defer stack.close()

	id := registerServer(t, stack.url, "backend", backend.HTTP.URL, nil)
	waitForReady(t, stack.url, id)
	before := backend.ListToolsCount()

	resp, err := http.Post(stack.url+"/api/v1/mcp-servers/"+id+":refresh-tools", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Location"); got != "/api/v1/mcp-servers/"+id {
		t.Fatalf("Location = %q, want the server self link", got)
	}
	waitForListIncrease(t, backend, before)
}

func TestRefreshToolsEndpointRejectsUnknownAndNonRunnable(t *testing.T) {
	stack := startStack(t, filepath.Join(t.TempDir(), "app.db"), 0)
	defer stack.close()

	disabled := false
	id := registerServer(t, stack.url, "backend", "http://127.0.0.1:1/mcp", &disabled)

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{"missing server", "/api/v1/mcp-servers/ghost:refresh-tools", http.StatusNotFound, "not_found"},
		{"disabled server", "/api/v1/mcp-servers/" + id + ":refresh-tools", http.StatusConflict, "conflict"},
		{"unknown action", "/api/v1/mcp-servers/" + id + ":restart", http.StatusNotFound, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(stack.url+tt.path, "application/json", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantCode {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tt.wantCode, raw)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != tt.wantBody {
				t.Fatalf("error.code = %q, want %q", body.Error.Code, tt.wantBody)
			}
		})
	}
}
