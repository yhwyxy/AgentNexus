package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
)

type authErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readAuthError(t *testing.T, resp *http.Response) authErrorBody {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	var body authErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("error body is not JSON: %v; raw: %s", err, raw)
	}

	return body
}

// 认证在 HTTP 层短路：被拒绝的注册请求不会到达应用层，仓储里也就不会留下行。
func TestUnauthenticatedRequestNeverReachesApplicationLayer(t *testing.T) {
	stack := startStack(t, filepath.Join(t.TempDir(), "auth.db"), 0)
	defer stack.close()

	body := `{"name":"unauthorized","transport":"streamable_http",
		"runtime":{"type":"remote","remote":{"endpoint":"http://backend/mcp"}}}`
	resp, err := http.Post(stack.url+"/api/v1/mcp-servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("register without credentials: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 401; body: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="agentnexus"` {
		t.Errorf("WWW-Authenticate = %q, want the Bearer challenge", got)
	}
	if got := readAuthError(t, resp).Error.Code; got != "unauthenticated" {
		t.Errorf("error.code = %q, want unauthenticated", got)
	}

	if _, err := stack.servers.GetByName(context.Background(), "default", "unauthorized"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("GetByName error = %v, want ErrNotFound (rejected request must not persist)", err)
	}
}

// 真实装配下的角色分离：agent 进不了管理 API，但数据面（/mcp）可用；
// 无凭证连 MCP 协议层都进不去（拒绝发生在 JSON-RPC 之前）。
func TestRoleSeparationOnRealAssembly(t *testing.T) {
	backend := fakemcp.New()
	defer backend.Close()

	stack := startStack(t, filepath.Join(t.TempDir(), "auth-roles.db"), 0)
	defer stack.close()

	registerBody := fmt.Sprintf(`{"name":"backend","transport":"streamable_http",
		"runtime":{"type":"remote","remote":{"endpoint":%q}}}`, backend.HTTP.URL)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		stack.url+"/api/v1/mcp-servers", strings.NewReader(registerBody))
	if err != nil {
		t.Fatalf("build register request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAgentSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register with agent key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("agent register status = %d, want 403; body: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none on 403", got)
	}
	if got := readAuthError(t, resp).Error.Code; got != "permission_denied" {
		t.Errorf("error.code = %q, want permission_denied", got)
	}

	// 无凭证打 /mcp：传输层 401，不进入 JSON-RPC。
	resp, err = http.Post(stack.url+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("unauthenticated /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("unauthenticated /mcp status = %d, want 401; body: %s", resp.StatusCode, raw)
	}
	if got := readAuthError(t, resp).Error.Code; got != "unauthenticated" {
		t.Errorf("error.code = %q, want unauthenticated", got)
	}

	// admin 注册并等到 ready，随后 agent 通过数据面看到同一份聚合目录。
	id := registerServer(t, stack.url, "backend", backend.HTTP.URL, nil)
	waitForReady(t, stack.url, id)

	names := listToolNamesAs(t, stack.url, testAgentSecret)
	if len(names) == 0 {
		t.Fatalf("agent tools/list = %v, want the aggregated catalog", names)
	}
}
