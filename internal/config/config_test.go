package config_test

import (
	"os"
	"path/filepath"
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
