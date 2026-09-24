//go:build unix

package app_test

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// waitForProcessExit 断言 pid 已经彻底消失(退出后由父进程 Wait 回收,不留僵尸)。
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
