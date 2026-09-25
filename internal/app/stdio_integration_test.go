package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/app"
	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	mcpserver "github.com/yhwyxy/AgentNexus/internal/mcpadapter/server"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/process"
	"github.com/yhwyxy/AgentNexus/internal/runtime/remote"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// fakeStdioEnv 让测试二进制自身扮演 stdio MCP 子进程。
const fakeStdioEnv = "AGENTNEXUS_FAKE_STDIO"

// TestFakeStdioBackendProcess 是子进程入口:只有带环境变量时才会真正跑 MCP stdio 服务。
// 调用方通过 -test.run 选中本测试,并依赖 os.Exit 阻断 testing 框架往 stdout 写内容。
func TestFakeStdioBackendProcess(t *testing.T) {
	if os.Getenv(fakeStdioEnv) != "1" {
		t.Skip("helper process for stdio integration tests")
	}
	if err := fakemcp.ServeStdio(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// stdioStack 是"注册一个本机 stdio 子进程后端"的完整装配:真实 SQLite、真实生命周期、
// 真实进程 Provider、真实 /mcp。
type stdioStack struct {
	URL       string
	PidFile   string
	lifecycle *app.Lifecycle
	runtimes  *runtime.ProviderManager
	httpSrv   *httptest.Server
	closeOnce sync.Once
}

// startStdioStack 启动整栈并注册 fake stdio 后端;返回前不保证 ready。
func startStdioStack(t *testing.T) *stdioStack {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	servers := sqlite.NewServerRepository(db)
	registry := server.NewService(servers).WithIDGenerator(func() string { return "stdio-1" })
	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo)
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	provider := process.NewProvider(ctx)
	runtimes, err := runtime.NewManager(servers, remote.NewProvider(), provider)
	if err != nil {
		t.Fatalf("create runtime manager: %v", err)
	}
	lifecycle, err := app.NewLifecycle(app.LifecycleOptions{
		Registry:          registry,
		Runtime:           runtimes,
		Catalog:           catalog,
		Syncer:            tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog),
		Lister:            servers,
		ReconcileInterval: 10 * time.Millisecond,
		Auditor:           audit.NewRecorder(sqlite.NewAuditRepository(db)),
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
	httpSrv := httptest.NewServer(httpapi.NewHandler(httpapi.Options{
		Registry:      lifecycle,
		Refresher:     lifecycle,
		MCP:           mcpHandler,
		Authenticator: testAuthorizer(t),
		Logger:        testLogger(),
	}))

	stack := &stdioStack{
		URL:       httpSrv.URL,
		PidFile:   filepath.Join(dir, "stdio-child.pid"),
		lifecycle: lifecycle,
		runtimes:  runtimes,
		httpSrv:   httpSrv,
	}
	t.Cleanup(func() {
		stack.close(t)
		clients.Close(context.Background())
	})

	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"name":      "stdio-backend",
		"transport": "stdio",
		"runtime": map[string]any{
			"type": "process",
			"process": map[string]any{
				"command": executable,
				"args":    []string{"-test.run=TestFakeStdioBackendProcess"},
				"env": map[string]string{
					fakeStdioEnv:                    "1",
					"AGENTNEXUS_FAKE_STDIO_PIDFILE": stack.PidFile,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("encode registration body: %v", err)
	}
	resp := doAuthorized(t, http.MethodPost, stack.URL+"/api/v1/mcp-servers", string(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
	return stack
}

// close 复刻生产过程关闭顺序:停生命周期队列 → 终止运行实例(子进程)→ 停 HTTP。
func (s *stdioStack) close(t *testing.T) {
	t.Helper()
	s.closeOnce.Do(func() {
		closeLifecycle(t, s.lifecycle)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := s.runtimes.Close(ctx); err != nil {
			t.Fatalf("close runtime manager: %v", err)
		}
		s.httpSrv.Close()
	})
}

// waitUntilReady 等待后台协调把 stdio 后端推进到 ready/observedRevision=revision。
func (s *stdioStack) waitUntilReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		phase, observed := getServerStatus(t, s.URL, "stdio-1")
		if phase == string(server.PhaseReady) && observed == 1 {
			return
		}
		if phase == string(server.PhaseFailed) {
			t.Fatalf("stdio backend failed to start")
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdio backend status = %q/%d, want ready/1", phase, observed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *stdioStack) childPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if raw, err := os.ReadFile(s.PidFile); err == nil {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fake stdio child never wrote its pid file")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *stdioStack) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "stdio-app-test", Version: "1.0.0"}, nil).
		Connect(context.Background(), streamableTransport(s.URL+"/mcp", testAdminSecret), nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// TestRegisterStdioProcessBackendPublishesAndCallsTools 覆盖端到端链路:
// HTTP 注册 → 后台 EnsureReady(进程 Provider 起子进程 + stdio MCP 握手)→ tools/list
// → 快照发布 → /mcp 聚合可见 → tools/call 经由 stdio 子进程返回结果。
func TestRegisterStdioProcessBackendPublishesAndCallsTools(t *testing.T) {
	stack := startStdioStack(t)
	stack.waitUntilReady(t)
	stack.childPID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := stack.connect(t)

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, listedTool := range listed.Tools {
		names = append(names, listedTool.Name)
	}
	want := []string{"stdio-backend.demo.echo", "stdio-backend.demo.fail"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("tools/list = %v, want %v", names, want)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stdio-backend.demo.echo",
		Arguments: json.RawMessage(`{"message":"over-process-stdio"}`),
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "over-process-stdio" {
		t.Fatalf("tool result content = %#v", result.Content[0])
	}
}

// TestStdioChildIsReapedWhenRuntimesClose 验证应用关闭路径会终止本机子进程:
// 运行时管理器关闭后 pid 必须消失,不能留下孤儿。
func TestStdioChildIsReapedWhenRuntimesClose(t *testing.T) {
	stack := startStdioStack(t)
	stack.waitUntilReady(t)
	pid := stack.childPID(t)

	stack.close(t)
	waitForProcessExit(t, pid)
}

// killProcess 让子进程异常退出(SIGKILL / TerminateProcess),模拟后端崩溃。
func killProcess(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process %d: %v", pid, err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("kill process %d: %v", pid, err)
	}
}

// refreshTools 排队一次快照刷新,并等待新的子进程被拉起且目录重新可见。
func (s *stdioStack) refreshTools(t *testing.T, previousPID int) int {
	t.Helper()
	resp := doAuthorized(t, http.MethodPost, s.URL+"/api/v1/mcp-servers/stdio-1:refresh-tools", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh status = %d, want 202; body: %s", resp.StatusCode, raw)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		pid := s.readPID(t)
		if pid != 0 && pid != previousPID {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no replacement stdio child after %s (previous pid %d)", 20*time.Second, previousPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *stdioStack) readPID(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(s.PidFile)
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// TestStdioBackendRecoversAfterChildCrash 覆盖自愈路径:子进程被强杀后实例失效,
// 一次 refresh-tools 重新拉起新进程并重新发布目录,且 /mcp 仍能调用。
func TestStdioBackendRecoversAfterChildCrash(t *testing.T) {
	stack := startStdioStack(t)
	stack.waitUntilReady(t)
	killedPID := stack.childPID(t)

	killProcess(t, killedPID)
	waitForProcessExit(t, killedPID)

	newPID := stack.refreshTools(t, killedPID)
	if newPID == killedPID {
		t.Fatalf("refresh reused the killed pid %d", killedPID)
	}
	stack.waitUntilReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := stack.connect(t)
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatalf("tools/list after recovery: %v", err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stdio-backend.demo.echo",
		Arguments: json.RawMessage(`{"message":"after-restart"}`),
	})
	if err != nil {
		t.Fatalf("tools/call after recovery: %v", err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("unexpected result after recovery: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "after-restart" {
		t.Fatalf("tool result content = %#v", result.Content[0])
	}
}
