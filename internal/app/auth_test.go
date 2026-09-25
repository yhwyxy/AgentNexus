package app_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/auth"
)

// 测试密钥：与生产装配共用 auth.DefaultPolicy()，只替换密钥表，
// 因此这里断言的就是线上授权行为。
const (
	testAdminSecret = "app-admin-secret"
	testAgentSecret = "app-agent-secret"
)

func testAuthorizer(t *testing.T) *auth.Authorizer {
	t.Helper()
	authorizer, err := auth.NewAuthorizer([]auth.KeyConfig{
		{Name: "admin", Role: auth.RoleAdmin, Secret: testAdminSecret},
		{Name: "agent", Role: auth.RoleAgent, Secret: testAgentSecret},
	}, auth.DefaultPolicy())
	if err != nil {
		t.Fatalf("build test authorizer: %v", err)
	}

	return authorizer
}

// bearerTransport 给每个出站请求补上凭证：既有用例不必逐处改 header。
type bearerTransport struct {
	base   http.RoundTripper
	secret string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.secret)

	return t.base.RoundTrip(clone)
}

// authorizedClient 是带凭证的 HTTP 客户端，管理 API 与 /mcp 共用。
func authorizedClient(secret string) *http.Client {
	return &http.Client{Transport: bearerTransport{base: http.DefaultTransport, secret: secret}}
}

// doAuthorized 以 admin 身份发起请求，替代裸 http.Post/http.Get。
func doAuthorized(t *testing.T, method, url, body string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testAdminSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	return resp
}

// streamableTransport 返回带凭证的 streamable HTTP 客户端传输。
func streamableTransport(endpoint, secret string) *mcp.StreamableClientTransport {
	return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: authorizedClient(secret)}
}
