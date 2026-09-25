package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
)

// 管理方法在 app.Run 同形的真实装配（SQLite + ProviderManager + CatalogCache）下的端到端行为：
// 写库是否真的发生、同步回收后快照是否带 phase=stopped、revision 是否只在配置变更时递增，
// 以及工具集在 /mcp 上的可见性是否跟着运行意图收敛。
func TestManagementMethodsEndToEnd(t *testing.T) {
	ctx := context.Background()
	backend := fakemcp.New()
	defer backend.Close()

	stack := startStack(t, filepath.Join(t.TempDir(), "app.db"), 10*time.Millisecond)
	defer stack.close()

	id := registerServer(t, stack.url, "backend", backend.HTTP.URL, nil)
	sid := server.ID(id)
	waitForReady(t, stack.url, id)
	want := []string{"backend.demo.echo", "backend.demo.fail"}
	waitForToolNames(t, stack.url, want)

	listed, err := stack.lifecycle.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != sid {
		t.Fatalf("list = %#v, want the registered server", listed)
	}

	// 停用：同步回收，返回的快照必须已经是 stopped，工具立刻从聚合视图消失。
	disabled, err := stack.lifecycle.SetEnabled(ctx, sid, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Enabled || disabled.Status.Phase != server.PhaseStopped {
		t.Fatalf("disable returned enabled=%t phase=%q, want false/stopped", disabled.Enabled, disabled.Status.Phase)
	}
	persisted := loadServer(t, ctx, stack.servers, sid)
	if persisted.Enabled || persisted.Status.Phase != server.PhaseStopped {
		t.Fatalf("persisted enabled=%t phase=%q, want false/stopped", persisted.Enabled, persisted.Status.Phase)
	}
	if persisted.Revision != 1 {
		t.Fatalf("revision = %d after disable, want 1 (runtime intent never bumps revision)", persisted.Revision)
	}
	waitForToolNames(t, stack.url, nil)

	// 幂等：重复停用不报错，终态不变。
	again, err := stack.lifecycle.SetEnabled(ctx, sid, false)
	if err != nil {
		t.Fatalf("disable again: %v", err)
	}
	if again.Enabled || again.Status.Phase != server.PhaseStopped {
		t.Fatalf("second disable returned enabled=%t phase=%q, want false/stopped", again.Enabled, again.Status.Phase)
	}

	// disabled 的 Server 不能被 start 悄悄启用；先把期望状态置为 stopped，让断言有区分度。
	idle, err := stack.lifecycle.Stop(ctx, sid)
	if err != nil {
		t.Fatalf("stop while disabled: %v", err)
	}
	if idle.Spec.DesiredState != server.DesiredStopped || idle.Status.Phase != server.PhaseStopped {
		t.Fatalf("stop while disabled returned desiredState=%q phase=%q, want stopped/stopped", idle.Spec.DesiredState, idle.Status.Phase)
	}
	if _, err := stack.lifecycle.Start(ctx, sid); !errors.Is(err, server.ErrNotRunnable) {
		t.Fatalf("start on disabled error = %v, want ErrNotRunnable", err)
	}
	rejected := loadServer(t, ctx, stack.servers, sid)
	if rejected.Enabled || rejected.Spec.DesiredState != server.DesiredStopped || rejected.Revision != 1 {
		t.Fatalf("rejected start mutated the server: enabled=%t desiredState=%q revision=%d",
			rejected.Enabled, rejected.Spec.DesiredState, rejected.Revision)
	}

	// 启用只翻转标志位：期望状态仍是 stopped，聚合查询要求 desired_state=running，因此工具仍不可路由；
	// 真正的重建由 :start 负责（start 是运行意图的唯一写路径）。
	enabled, err := stack.lifecycle.SetEnabled(ctx, sid, true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !enabled.Enabled || enabled.Spec.DesiredState != server.DesiredStopped ||
		enabled.Status.Phase != server.PhaseStopped || enabled.Revision != 1 {
		t.Fatalf("enable returned enabled=%t desiredState=%q phase=%q revision=%d, want true/stopped/stopped/1",
			enabled.Enabled, enabled.Spec.DesiredState, enabled.Status.Phase, enabled.Revision)
	}
	waitForToolNames(t, stack.url, nil)

	// 启动：排队重建实例并收敛到 ready。
	running, err := stack.lifecycle.Start(ctx, sid)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if running.Spec.DesiredState != server.DesiredRunning || running.Revision != 1 {
		t.Fatalf("start returned desiredState=%q revision=%d, want running/1", running.Spec.DesiredState, running.Revision)
	}
	waitForObservedRevision(t, stack.url, id, 1)
	waitForToolNames(t, stack.url, want)

	// 已经是运行态时 start 幂等：仍会重新排队收敛，但不变更 revision。
	alreadyRunning, err := stack.lifecycle.Start(ctx, sid)
	if err != nil {
		t.Fatalf("start when running: %v", err)
	}
	if alreadyRunning.Spec.DesiredState != server.DesiredRunning || alreadyRunning.Revision != 1 {
		t.Fatalf("second start returned desiredState=%q revision=%d, want running/1", alreadyRunning.Spec.DesiredState, alreadyRunning.Revision)
	}
	waitForObservedRevision(t, stack.url, id, 1)

	// 停止：同步回收，phase=stopped，revision 不变，工具消失。
	stopped, err := stack.lifecycle.Stop(ctx, sid)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Spec.DesiredState != server.DesiredStopped || stopped.Status.Phase != server.PhaseStopped {
		t.Fatalf("stop returned desiredState=%q phase=%q, want stopped/stopped", stopped.Spec.DesiredState, stopped.Status.Phase)
	}
	if stopped.Revision != 1 {
		t.Fatalf("revision = %d after stop, want 1", stopped.Revision)
	}
	waitForToolNames(t, stack.url, nil)

	// 重启：先同步停旧实例，再排队重建；revision 必须不变。
	if _, err := stack.lifecycle.Start(ctx, sid); err != nil {
		t.Fatalf("start after stop: %v", err)
	}
	waitForObservedRevision(t, stack.url, id, 1)
	before := backend.ListToolsCount()
	restarted, err := stack.lifecycle.Restart(ctx, sid)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.Revision != 1 {
		t.Fatalf("revision = %d after restart, want 1", restarted.Revision)
	}
	waitForListIncrease(t, backend, before)
	waitForObservedRevision(t, stack.url, id, 1)

	// 更新配置：revision 递增，陈旧 revision 被乐观锁拒绝。
	current := loadServer(t, ctx, stack.servers, sid)
	in := server.UpdateInput{
		DisplayName: "Renamed",
		Runtime:     current.Spec.Runtime,
		Timeouts:    current.Spec.Timeouts,
		Limits:      current.Spec.Limits,
	}
	updated, err := stack.lifecycle.Update(ctx, sid, current.Revision, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != current.Revision+1 || updated.DisplayName != "Renamed" {
		t.Fatalf("update returned revision=%d displayName=%q", updated.Revision, updated.DisplayName)
	}
	waitForObservedRevision(t, stack.url, id, updated.Revision)
	waitForToolNames(t, stack.url, want)

	if _, err := stack.lifecycle.Update(ctx, sid, current.Revision, in); !errors.Is(err, server.ErrConflict) {
		t.Fatalf("stale update error = %v, want ErrConflict", err)
	}

	if _, err := stack.lifecycle.Update(ctx, "ghost", 1, in); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("update of unknown server error = %v, want ErrNotFound", err)
	}
}

func loadServer(t *testing.T, ctx context.Context, repo server.Repository, id server.ID) server.Server {
	t.Helper()
	srv, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("load server %s: %v", id, err)
	}
	return srv
}

// waitForObservedRevision 等到运行态快照追上配置版本；ready 但 observedRevision 落后
// 说明收敛还在队列里，此时读到的 phase 不能作为终态断言。
func waitForObservedRevision(t *testing.T, baseURL, id string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		phase, observed := getServerStatus(t, baseURL, id)
		if observed == want && phase == string(server.PhaseReady) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server %s observedRevision = %d (phase %q), want %d ready", id, observed, phase, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForToolNames(t *testing.T, httpURL string, want []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := listToolNames(t, httpURL)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools/list = %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
