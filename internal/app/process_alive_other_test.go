//go:build !unix

package app_test

import "testing"

// waitForProcessExit 在非 Unix 平台上无法可靠查询进程存活,这里只记录跳过。
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	t.Logf("process liveness assertion for pid %d is not supported on this platform", pid)
}
