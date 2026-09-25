//go:build unix

package process

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让子进程自成进程组:Stop 时按组发信号,连带回收它的子进程,
// 避免 AgentNexus 退出后留下孤儿。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcess 向进程组发 SIGTERM。
func terminateProcess(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}

// killProcess 向进程组发 SIGKILL。
func killProcess(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
