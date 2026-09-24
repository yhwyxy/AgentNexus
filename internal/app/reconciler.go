// 运行态收敛：启动重放与周期巡检。两者都不直接同步，只向 Lifecycle 的队列触发，
// 与注册触发共用同一 worker，因此同一 Server 不会被两条队列并发同步。
package app

import (
	"context"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

// ServerLister 是 Reconciler 读取全部期望运行 Server 的最小依赖。
type ServerLister interface {
	ListEnabled(ctx context.Context) ([]server.Server, error)
}

// reconcile 先做一次启动重放，再按间隔巡检未收敛的 Server。
//
// 启动重放是无条件的：运行态实例缓存不落库（详细设计 §5），进程重启后
// 数据库里的 phase=ready 与 observedRevision=revision 都无法证明实例存在，
// 必须重新建立实例并确认 Tool 快照（Sync 在 revision/digest 未变时短路，不改写快照）。
//
// 周期巡检只挑未收敛的 Server（§3.4），因此已收敛的 Server 不会被反复 tools/list。
func (l *Lifecycle) reconcile() {
	defer close(l.reconcileDone)

	l.reconcileAll(alwaysReconcile)
	if l.reconcileInterval <= 0 {
		return
	}

	ticker := time.NewTicker(l.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.reconcileAll(needsReconcile)
		}
	}
}

// reconcileAll 列出 enabled 的 Server，对 Runnable 且通过 keep 的项触发一次异步同步。
// 列表失败只记日志：下一个 tick 会重试，进程不会因为存储暂时不可用而停摆。
func (l *Lifecycle) reconcileAll(keep func(server.Server) bool) {
	servers, err := l.lister.ListEnabled(l.ctx)
	if err != nil {
		if l.ctx.Err() == nil {
			l.logger.Error("reconcile could not list servers", "error", err)
		}
		return
	}
	for _, srv := range servers {
		if !srv.Runnable() || !keep(srv) {
			continue
		}
		l.Trigger(srv.ID)
	}
}

func alwaysReconcile(server.Server) bool { return true }

// needsReconcile 判定运行态是否已收敛于期望配置（§3.4）。
func needsReconcile(srv server.Server) bool {
	if srv.Status.ConsecutiveFailures > 0 {
		return true
	}
	if srv.Status.Phase != server.PhaseReady {
		return true
	}
	return srv.Status.ObservedRevision != srv.Revision
}
