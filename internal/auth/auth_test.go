package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/auth"
)

func testKeys() []auth.KeyConfig {
	return []auth.KeyConfig{
		{Name: "boss", Role: auth.RoleAdmin, Secret: "admin-secret"},
		{Name: "ci", Role: auth.RoleAgent, Secret: "agent-secret"},
	}
}

func newAuthorizer(t *testing.T, keys []auth.KeyConfig, policy auth.Policy) *auth.Authorizer {
	t.Helper()
	authorizer, err := auth.NewAuthorizer(keys, policy)
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	return authorizer
}

func TestNewAuthorizerRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		keys   []auth.KeyConfig
		policy auth.Policy
	}{
		{
			name:   "empty name",
			keys:   []auth.KeyConfig{{Role: auth.RoleAdmin, Secret: "s"}},
			policy: auth.DefaultPolicy(),
		},
		{
			name:   "unsupported role",
			keys:   []auth.KeyConfig{{Name: "a", Role: "root", Secret: "s"}},
			policy: auth.DefaultPolicy(),
		},
		{
			name:   "empty secret",
			keys:   []auth.KeyConfig{{Name: "a", Role: auth.RoleAdmin}},
			policy: auth.DefaultPolicy(),
		},
		{
			name:   "duplicated name",
			keys:   []auth.KeyConfig{{Name: "a", Role: auth.RoleAdmin, Secret: "s"}, {Name: "a", Role: auth.RoleAgent, Secret: "t"}},
			policy: auth.DefaultPolicy(),
		},
		{
			name:   "duplicated secret",
			keys:   []auth.KeyConfig{{Name: "a", Role: auth.RoleAdmin, Secret: "s"}, {Name: "b", Role: auth.RoleAgent, Secret: "s"}},
			policy: auth.DefaultPolicy(),
		},
		{
			name:   "relative public path",
			keys:   testKeys(),
			policy: auth.Policy{Public: []string{"health"}},
		},
		{
			name:   "rule without roles",
			keys:   testKeys(),
			policy: auth.Policy{Rules: []auth.Rule{{Prefix: "/api/v1/"}}},
		},
		{
			name:   "rule with unsupported role",
			keys:   testKeys(),
			policy: auth.Policy{Rules: []auth.Rule{{Prefix: "/api/v1/", Roles: []auth.Role{"root"}}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := auth.NewAuthorizer(test.keys, test.policy); err == nil {
				t.Fatal("NewAuthorizer succeeded, want error")
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name          string
		keys          []auth.KeyConfig
		path          string
		presented     string
		wantErr       error
		wantPrincipal *auth.Principal
	}{
		{
			name: "public liveness without credentials",
			keys: testKeys(), path: "/health/live", presented: "",
		},
		{
			name: "public readiness without credentials",
			keys: testKeys(), path: "/health/ready", presented: "",
		},
		{
			name: "public path wins over bad credentials",
			keys: testKeys(), path: "/health/live", presented: "garbage",
		},
		{
			name: "public path is exact match only",
			keys: testKeys(), path: "/health/lively", presented: "", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "surface under /health is not public",
			keys: testKeys(), path: "/health/", presented: "", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "missing credentials",
			keys: testKeys(), path: "/api/v1/mcp-servers", presented: "", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "unknown credentials",
			keys: testKeys(), path: "/api/v1/mcp-servers", presented: "nope", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "admin on management API",
			keys: testKeys(), path: "/api/v1/mcp-servers", presented: "admin-secret",
			wantPrincipal: &auth.Principal{Name: "boss", Role: auth.RoleAdmin},
		},
		{
			name: "agent on management API is denied",
			keys: testKeys(), path: "/api/v1/mcp-servers", presented: "agent-secret", wantErr: auth.ErrPermissionDenied,
		},
		{
			name: "agent on data plane",
			keys: testKeys(), path: "/mcp", presented: "agent-secret",
			wantPrincipal: &auth.Principal{Name: "ci", Role: auth.RoleAgent},
		},
		{
			name: "admin on data plane",
			keys: testKeys(), path: "/mcp", presented: "admin-secret",
			wantPrincipal: &auth.Principal{Name: "boss", Role: auth.RoleAdmin},
		},
		{
			name: "unregistered path is denied even for admin",
			keys: testKeys(), path: "/internal/debug", presented: "admin-secret", wantErr: auth.ErrPermissionDenied,
		},
		{
			name: "unregistered path reports unauthenticated without credentials",
			keys: testKeys(), path: "/internal/debug", presented: "", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "empty key table rejects data plane",
			keys: nil, path: "/mcp", presented: "anything", wantErr: auth.ErrUnauthenticated,
		},
		{
			name: "empty key table still serves public paths",
			keys: nil, path: "/health/ready", presented: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorizer := newAuthorizer(t, test.keys, auth.DefaultPolicy())
			principal, err := authorizer.Authorize(test.path, test.presented)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Authorize(%q, %q) error = %v, want %v", test.path, test.presented, err, test.wantErr)
				}
				if principal != nil {
					t.Fatalf("Authorize returned principal %#v alongside error", principal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize(%q, %q) error = %v, want nil", test.path, test.presented, err)
			}
			if test.wantPrincipal == nil {
				if principal != nil {
					t.Fatalf("Authorize(%q, %q) principal = %#v, want nil (public path)", test.path, test.presented, principal)
				}
				return
			}
			if principal == nil || *principal != *test.wantPrincipal {
				t.Fatalf("Authorize(%q, %q) principal = %#v, want %#v", test.path, test.presented, principal, test.wantPrincipal)
			}
		})
	}
}

// 多条规则匹配同一路径时最长前缀胜出：更严格（或不同角色集）的子前缀可以覆盖父前缀。
func TestAuthorizeLongestPrefixWins(t *testing.T) {
	policy := auth.Policy{
		Rules: []auth.Rule{
			{Prefix: "/api/v1/", Roles: []auth.Role{auth.RoleAdmin}},
			{Prefix: "/api/v1/audit/", Roles: []auth.Role{auth.RoleAgent}},
		},
	}
	authorizer := newAuthorizer(t, testKeys(), policy)

	principal, err := authorizer.Authorize("/api/v1/audit/events", "agent-secret")
	if err != nil || principal == nil || principal.Role != auth.RoleAgent {
		t.Fatalf("agent on /api/v1/audit/events = (%#v, %v), want agent principal", principal, err)
	}
	if _, err := authorizer.Authorize("/api/v1/audit/events", "admin-secret"); !errors.Is(err, auth.ErrPermissionDenied) {
		t.Fatalf("admin on /api/v1/audit/events error = %v, want ErrPermissionDenied (narrower prefix wins)", err)
	}
	if _, err := authorizer.Authorize("/api/v1/mcp-servers", "agent-secret"); !errors.Is(err, auth.ErrPermissionDenied) {
		t.Fatalf("agent on /api/v1/mcp-servers error = %v, want ErrPermissionDenied", err)
	}
}

func TestPrincipalContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := auth.PrincipalFrom(ctx); ok {
		t.Fatal("PrincipalFrom on empty context reported a principal")
	}

	want := auth.Principal{Name: "boss", Role: auth.RoleAdmin}
	got, ok := auth.PrincipalFrom(auth.WithPrincipal(ctx, want))
	if !ok || got != want {
		t.Fatalf("PrincipalFrom = (%#v, %t), want (%#v, true)", got, ok, want)
	}
}
