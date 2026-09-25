//go:build unix

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStopTerminatesWholeProcessGroup 验证 Stop 按进程组发信号:
// 子进程自己派生的孙进程也必须一起退出,否则 AgentNexus 退出后会留下孤儿。
func TestStopTerminatesWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	provider := NewProvider(context.Background(), WithStopGrace(time.Second))
	t.Cleanup(func() { _ = provider.Close(context.Background()) })

	srv := stdioServer("group", "/bin/sh", "-c", `sleep 30 & printf '%s' "$!" > "$1"; wait $!`, "sh", pidFile)
	instance := mustEnsure(t, provider, srv)

	pid, err := strconv.Atoi(instance.ExternalID)
	if err != nil {
		t.Fatalf("external id %q is not a pid: %v", instance.ExternalID, err)
	}
	var grandchild int
	if !waitForCondition(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		grandchild, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	}) {
		t.Fatal("grandchild pid was not recorded")
	}

	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for name, target := range map[string]int{"child": pid, "process group": -pid, "grandchild": grandchild} {
		if err := syscall.Kill(target, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("%s %d survived Stop: %v", name, target, err)
		}
	}
}
