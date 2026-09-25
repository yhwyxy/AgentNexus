package hostaccess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

func TestNewPolicyRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry
		want    string
	}{
		{
			name:    "relative host",
			entries: []Entry{{Host: "sandbox", Container: "/workspace"}},
			want:    "host must be an absolute path",
		},
		{
			name:    "host root",
			entries: []Entry{{Host: "/", Container: "/workspace"}},
			want:    "host must not be the host root",
		},
		{
			name:    "relative container",
			entries: []Entry{{Host: "/srv/sandbox", Container: "workspace"}},
			want:    "container must be an absolute path",
		},
		{
			name:    "container root",
			entries: []Entry{{Host: "/srv/sandbox", Container: "/"}},
			want:    "container must not be the container root",
		},
		{
			name: "duplicate host",
			entries: []Entry{
				{Host: "/srv/sandbox", Container: "/workspace"},
				{Host: "/srv/sandbox/", Container: "/other"},
			},
			want: "is declared twice",
		},
		{
			name:    "empty entry",
			entries: []Entry{{}},
			want:    "host must be an absolute path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, err := NewPolicy(test.entries)
			if err == nil {
				t.Fatalf("NewPolicy(%+v) succeeded, want error containing %q", test.entries, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewPolicy error = %q, want containing %q", err, test.want)
			}
			if policy != nil {
				t.Fatal("NewPolicy returned a policy alongside an error")
			}
		})
	}
}

// newTempPolicy 返回以临时目录为宿主机根的允许清单策略,以及该根目录。
// 临时目录在 macOS 上通常是符号链接(/var -> /private/var),正好覆盖真实路径匹配。
func newTempPolicy(t *testing.T, allowWrite bool) (*Policy, string) {
	t.Helper()
	root := t.TempDir()
	policy, err := NewPolicy(
		[]Entry{{Host: root, Container: "/workspace", AllowWrite: allowWrite}},
		"unix:///var/run/docker.sock",
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return policy, root
}

func TestPolicyCheckDockerMounts(t *testing.T) {
	policy, root := newTempPolicy(t, true)
	readOnlyPolicy, readOnlyRoot := newTempPolicy(t, false)

	tests := []struct {
		name   string
		policy *Policy
		spec   server.DockerSpec
		want   string
	}{
		{
			name:   "allowed read write",
			policy: policy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: filepath.Join(root, "shared"), Target: "/workspace/shared",
			}}},
		},
		{
			name:   "allowed read only",
			policy: readOnlyPolicy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: filepath.Join(readOnlyRoot, "shared"), Target: "/workspace/nested/dir", ReadOnly: true,
			}}},
		},
		{
			name:   "write on read only entry",
			policy: readOnlyPolicy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: filepath.Join(readOnlyRoot, "shared"), Target: "/workspace/shared",
			}}},
			want: "allowed read-only only",
		},
		{
			name:   "outside allowed roots",
			policy: policy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: "/srv/elsewhere", Target: "/workspace/elsewhere",
			}}},
			want: "outside the allowed host roots",
		},
		{
			name:   "sibling prefix is not inside the root",
			policy: policy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: root + "-other", Target: "/workspace/other",
			}}},
			want: "outside the allowed host roots",
		},
		{
			name:   "target outside container root",
			policy: policy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: filepath.Join(root, "shared"), Target: "/data/shared",
			}}},
			want: "outside the allowed container root",
		},
		{
			name:   "target is the container root itself is allowed by target check only",
			policy: policy,
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: filepath.Join(root, "shared"), Target: "/workspace",
			}}},
		},
		{
			name:   "denied system root",
			policy: mustPolicy(t, []Entry{{Host: "/etc", Container: "/workspace", AllowWrite: true}}),
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: "/etc/passwd", Target: "/workspace/passwd",
			}}},
			want: "denied by host access policy",
		},
		{
			name:   "denied docker socket",
			policy: mustPolicy(t, []Entry{{Host: "/var/run", Container: "/workspace", AllowWrite: true}}, "unix:///var/run/docker.sock"),
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: "/var/run/docker.sock", Target: "/workspace/docker.sock", ReadOnly: true,
			}}},
			want: "denied by host access policy",
		},
		{
			name:   "denied socket by suffix",
			policy: mustPolicy(t, []Entry{{Host: "/Users/someone/tmp", Container: "/workspace", AllowWrite: true}}),
			spec: server.DockerSpec{Mounts: []server.Mount{{
				Source: "/Users/someone/tmp/docker.sock", Target: "/workspace/docker.sock", ReadOnly: true,
			}}},
			want: "denied by host access policy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.policy.CheckDocker(test.spec)
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("CheckDocker() = %v, want nil", err)
			case test.want == "":
			case err == nil:
				t.Fatalf("CheckDocker() = nil, want error containing %q", test.want)
			case !strings.Contains(err.Error(), test.want):
				t.Fatalf("CheckDocker() = %q, want containing %q", err, test.want)
			}
		})
	}
}

func mustPolicy(t *testing.T, entries []Entry, socketPaths ...string) *Policy {
	t.Helper()
	policy, err := NewPolicy(entries, socketPaths...)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return policy
}

// 允许清单内的符号链接指向被拒绝目录时必须失败:词法路径合规但真实路径越界。
func TestPolicyRejectsSymlinkEscape(t *testing.T) {
	policy, root := newTempPolicy(t, true)
	escape := filepath.Join(root, "escape")
	if err := os.Symlink("/etc", escape); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := policy.CheckDocker(server.DockerSpec{Mounts: []server.Mount{{
		Source: escape, Target: "/workspace/escape",
	}}})
	if err == nil {
		t.Fatal("CheckDocker() = nil, want error for symlink escaping the allowed roots")
	}
	// macOS 上 /etc 本身是符号链接,解析后可能落到拒绝清单里,也可能只是
	// 不再命中允许清单 —— 两者都必须失败,且错误里要有原始来源便于排查。
	if !strings.Contains(err.Error(), escape) {
		t.Fatalf("CheckDocker() = %q, want it to name the mount source", err)
	}
}

func TestPolicyCheckDockerEnv(t *testing.T) {
	policy, _ := newTempPolicy(t, false)

	allowed := []string{"PATH", "DEBUG", "LOG_LEVEL", "AWS_REGION", "TOKENIZER_PATH", "MY_SECRETS"}
	for _, key := range allowed {
		if err := policy.CheckDocker(server.DockerSpec{Env: map[string]string{key: "value"}}); err != nil {
			t.Fatalf("CheckDocker(env %q) = %v, want nil", key, err)
		}
	}

	denied := []string{"TOKEN", "GITHUB_TOKEN", "API_KEY", "apiKey", "MYSQL_PASSWORD", "AWS_SECRET_ACCESS_KEY", "CLIENT_CREDENTIAL"}
	for _, key := range denied {
		err := policy.CheckDocker(server.DockerSpec{Env: map[string]string{key: "value"}})
		if err == nil {
			t.Fatalf("CheckDocker(env %q) = nil, want error", key)
		}
		if !strings.Contains(err.Error(), "looks like a secret") {
			t.Fatalf("CheckDocker(env %q) = %q, want a secret hint", key, err)
		}
		if strings.Contains(err.Error(), "value") {
			t.Fatalf("CheckDocker(env %q) leaked the value: %q", key, err)
		}
	}
}

func TestPolicyWithoutEntriesAllowsNothing(t *testing.T) {
	policy := mustPolicy(t, nil)
	err := policy.CheckDocker(server.DockerSpec{Mounts: []server.Mount{{Source: "/srv/data", Target: "/workspace/data"}}})
	if err == nil {
		t.Fatal("CheckDocker() without entries = nil, want error")
	}
	if err := policy.CheckDocker(server.DockerSpec{}); err != nil {
		t.Fatalf("CheckDocker() with no mounts = %v, want nil", err)
	}
}
