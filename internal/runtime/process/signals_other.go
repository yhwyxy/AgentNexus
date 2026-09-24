//go:build !unix

package process

import (
	"os"
	"os/exec"
)

// setProcessGroup 在非 Unix 平台上没有进程组语义,退化为直接管理子进程。
func setProcessGroup(_ *exec.Cmd) {}

// terminateProcess 在非 Unix 平台上无法发送 SIGTERM,直接终止进程。
func terminateProcess(pid int) { killProcess(pid) }

func killProcess(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
}
