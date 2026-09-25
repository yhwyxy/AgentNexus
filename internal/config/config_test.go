package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/config"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentnexus.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadReconcileInterval(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  string
		want time.Duration
	}{
		{name: "default", yaml: "", want: 30 * time.Second},
		{name: "yaml override", yaml: "runtime:\n  reconcileInterval: 1m\n", want: time.Minute},
		{name: "disabled", yaml: "runtime:\n  reconcileInterval: 0s\n", want: 0},
		{name: "env wins over yaml", yaml: "runtime:\n  reconcileInterval: 1m\n", env: "7s", want: 7 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL", tt.env)

			cfg, err := config.Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.Runtime.ReconcileInterval != tt.want {
				t.Fatalf("reconcile interval = %s, want %s", cfg.Runtime.ReconcileInterval, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidReconcileInterval(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  string
	}{
		{name: "negative yaml", yaml: "runtime:\n  reconcileInterval: -1s\n"},
		{name: "negative env", env: "-1s"},
		{name: "unparsable yaml", yaml: "runtime:\n  reconcileInterval: soon\n"},
		{name: "unparsable env", env: "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL", tt.env)

			if _, err := config.Load(writeConfig(t, tt.yaml)); err == nil {
				t.Fatal("Load accepted an invalid reconcile interval")
			}
		})
	}
}

func TestLoadSecurityAPIKeys(t *testing.T) {
	const yamlOnlyLiteral = "security:\n  apiKeys:\n    - name: admin\n      role: admin\n      key: literal-secret\n"
	const yamlWithEnv = "security:\n  apiKeys:\n    - name: ci\n      role: agent\n      keyEnv: AGENTNEXUS_TEST_AGENT_KEY\n"

	tests := []struct {
		name string
		yaml string
		env  string
		want []config.APIKeyConfig
	}{
		{name: "absent section", yaml: "", want: nil},
		{name: "empty list", yaml: "security:\n  apiKeys: []\n", want: []config.APIKeyConfig{}},
		{
			name: "literal key",
			yaml: yamlOnlyLiteral,
			want: []config.APIKeyConfig{{Name: "admin", Role: "admin", Secret: "literal-secret"}},
		},
		{
			name: "key from environment",
			yaml: yamlWithEnv,
			env:  "env-secret",
			want: []config.APIKeyConfig{{Name: "ci", Role: "agent", Secret: "env-secret"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTNEXUS_TEST_AGENT_KEY", tt.env)

			cfg, err := config.Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if len(cfg.Security.APIKeys) != len(tt.want) {
				t.Fatalf("api keys = %#v, want %#v", cfg.Security.APIKeys, tt.want)
			}
			for i, want := range tt.want {
				if cfg.Security.APIKeys[i] != want {
					t.Fatalf("api key %d = %#v, want %#v", i, cfg.Security.APIKeys[i], want)
				}
			}
		})
	}
}

func TestLoadRejectsInvalidSecurityAPIKeys(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  string
	}{
		{
			name: "key and keyEnv together",
			yaml: "security:\n  apiKeys:\n    - name: admin\n      role: admin\n      key: leaky-secret\n      keyEnv: AGENTNEXUS_TEST_AGENT_KEY\n",
			env:  "env-secret",
		},
		{
			name: "neither key nor keyEnv",
			yaml: "security:\n  apiKeys:\n    - name: admin\n      role: admin\n",
		},
		{
			name: "keyEnv unset",
			yaml: "security:\n  apiKeys:\n    - name: admin\n      role: admin\n      keyEnv: AGENTNEXUS_TEST_AGENT_KEY\n",
		},
		{
			name: "keyEnv empty",
			yaml: "security:\n  apiKeys:\n    - name: admin\n      role: admin\n      keyEnv: AGENTNEXUS_TEST_AGENT_KEY\n",
			env:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTNEXUS_TEST_AGENT_KEY", tt.env)

			_, err := config.Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatal("Load accepted an invalid security.apiKeys entry")
			}
			// 错误信息只能带字段名与下标，绝不回显密钥值。
			if strings.Contains(err.Error(), "leaky-secret") {
				t.Fatalf("error %q leaks the configured secret", err)
			}
		})
	}
}

// DOCKER_HOST 是 Docker 生态的既有约定：未显式配置时跟随它，YAML 显式配置优先。
func TestLoadDockerConfig(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		dockerHost      string
		wantHost        string
		wantConnectHost string
		wantPublishHost string
		wantStopGrace   time.Duration
	}{
		{
			name:            "defaults",
			wantHost:        "unix:///var/run/docker.sock",
			wantConnectHost: "127.0.0.1",
			wantPublishHost: "127.0.0.1",
			wantStopGrace:   5 * time.Second,
		},
		{
			name:            "docker host environment",
			dockerHost:      "tcp://orbstack.local:2375",
			wantHost:        "tcp://orbstack.local:2375",
			wantConnectHost: "127.0.0.1",
			wantPublishHost: "127.0.0.1",
			wantStopGrace:   5 * time.Second,
		},
		{
			name:            "yaml wins over environment",
			yaml:            "runtime:\n  docker:\n    host: unix:///run/user/501/docker.sock\n",
			dockerHost:      "tcp://orbstack.local:2375",
			wantHost:        "unix:///run/user/501/docker.sock",
			wantConnectHost: "127.0.0.1",
			wantPublishHost: "127.0.0.1",
			wantStopGrace:   5 * time.Second,
		},
		{
			name: "full override",
			yaml: "runtime:\n  docker:\n    host: tcp://dockerd:2375\n    connectHost: host.docker.internal\n" +
				"    publishHost: 0.0.0.0\n    stopGrace: 12s\n",
			wantHost:        "tcp://dockerd:2375",
			wantConnectHost: "host.docker.internal",
			wantPublishHost: "0.0.0.0",
			wantStopGrace:   12 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tt.dockerHost)

			cfg, err := config.Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.Runtime.Docker.Host != tt.wantHost {
				t.Fatalf("docker host = %q, want %q", cfg.Runtime.Docker.Host, tt.wantHost)
			}
			if cfg.Runtime.Docker.ConnectHost != tt.wantConnectHost {
				t.Fatalf("docker connectHost = %q, want %q", cfg.Runtime.Docker.ConnectHost, tt.wantConnectHost)
			}
			if cfg.Runtime.Docker.PublishHost != tt.wantPublishHost {
				t.Fatalf("docker publishHost = %q, want %q", cfg.Runtime.Docker.PublishHost, tt.wantPublishHost)
			}
			if cfg.Runtime.Docker.StopGrace != tt.wantStopGrace {
				t.Fatalf("docker stopGrace = %s, want %s", cfg.Runtime.Docker.StopGrace, tt.wantStopGrace)
			}
		})
	}
}

func TestLoadRejectsInvalidDockerConfig(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "host without scheme", yaml: "runtime:\n  docker:\n    host: /var/run/docker.sock\n"},
		{name: "unsupported scheme", yaml: "runtime:\n  docker:\n    host: npipe:////./pipe/docker_engine\n"},
		{name: "unparsable stop grace", yaml: "runtime:\n  docker:\n    stopGrace: soon\n"},
		{name: "zero stop grace", yaml: "runtime:\n  docker:\n    stopGrace: 0s\n"},
		{name: "negative stop grace", yaml: "runtime:\n  docker:\n    stopGrace: -3s\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", "")

			if _, err := config.Load(writeConfig(t, tt.yaml)); err == nil {
				t.Fatal("Load accepted an invalid runtime.docker configuration")
			}
		})
	}
}

func TestLoadHostAccessMounts(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	cfg, err := config.Load(writeConfig(t, "security:\n  hostAccess:\n    mounts:\n"+
		"      - {host: /Users/dev/sandbox, container: /workspace, access: rw}\n"+
		"      - {host: /Users/dev/reference, container: /reference}\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	want := []config.HostMountConfig{
		{Host: "/Users/dev/sandbox", Container: "/workspace", Access: config.AccessReadWrite},
		{Host: "/Users/dev/reference", Container: "/reference", Access: config.AccessReadOnly},
	}
	if len(cfg.Security.HostAccess.Mounts) != len(want) {
		t.Fatalf("mounts = %+v, want %+v", cfg.Security.HostAccess.Mounts, want)
	}
	for i, entry := range cfg.Security.HostAccess.Mounts {
		if entry != want[i] {
			t.Fatalf("mounts[%d] = %+v, want %+v", i, entry, want[i])
		}
	}
}

func TestLoadRejectsInvalidHostAccessMounts(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	_, err := config.Load(writeConfig(t, "security:\n  hostAccess:\n    mounts:\n"+
		"      - {host: /Users/dev/sandbox, container: /workspace, access: write}\n"))
	if err == nil {
		t.Fatal("Load accepted an invalid hostAccess access mode")
	}
	if !strings.Contains(err.Error(), "security.hostAccess.mounts[0].access") {
		t.Fatalf("error %q does not point at the offending mount", err)
	}
}
