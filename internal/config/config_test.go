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
