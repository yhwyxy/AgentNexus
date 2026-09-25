// 运行态协调：把注册、启动重放、周期巡检与手工刷新收敛到一条异步同步队列，
// 由 SyncService 内部的 RuntimeManager 完成 Runtime 启动与状态收敛。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// ServerRegistry 是 Lifecycle 对注册应用服务的最小依赖。
// 与 edge/httpapi.ServerRegistry 结构一致，可直接交给 HTTP 层。
type ServerRegistry interface {
	Register(ctx context.Context, in server.RegisterInput) (server.Server, error)
	Get(ctx context.Context, id server.ID) (server.Server, error)
	List(ctx context.Context) ([]server.Server, error)
	Update(ctx context.Context, id server.ID, expectedRevision int64, in server.UpdateInput) (server.Server, error)
	SetEnabled(ctx context.Context, id server.ID, enabled bool) (server.Server, error)
	SetDesiredState(ctx context.Context, id server.ID, state server.DesiredState) (server.Server, error)
}

// RuntimeConverger 是 Lifecycle 对运行态收敛的最小依赖（*runtime.ProviderManager 实现）。
// Reconcile 按库里的当前配置收敛实例：非运行态的 Server 会停掉已加载实例并写入 phase=stopped。
// Stop 只做同步回收，用于 Restart 重建前的拆除。
type RuntimeConverger interface {
	Reconcile(ctx context.Context, id server.ID) error
	Stop(ctx context.Context, id server.ID) error
}

// CatalogInvalidator 是 Lifecycle 对 Tool 目录缓存的最小依赖（*tool.CatalogCache 实现）。
type CatalogInvalidator interface {
	Invalidate()
}

// AuditRecorder 是 Lifecycle 对审计写入的最小依赖（消费者定义接口）。
type AuditRecorder interface {
	Record(ctx context.Context, in audit.Input) error
}

// ToolSynchronizer 是 Lifecycle 对 Tool 同步服务的最小依赖。
type ToolSynchronizer interface {
	Sync(ctx context.Context, id server.ID) (tool.SyncResult, error)
}

// LifecycleOptions 是 Lifecycle 的装配参数。
type LifecycleOptions struct {
	// Registry、Syncer、Auditor、Runtime 与 Catalog 必填。
	Registry ServerRegistry
	Syncer   ToolSynchronizer
	Auditor  AuditRecorder
	Runtime  RuntimeConverger
	Catalog  CatalogInvalidator
	// Lister 为 nil 时关闭启动重放与周期巡检，只保留注册触发。
	Lister ServerLister
	// ReconcileInterval <= 0 时只做启动重放，不做周期巡检。
	ReconcileInterval time.Duration
	Logger            *slog.Logger
}

// Lifecycle 把注册、启动重放、周期巡检与手工刷新产生的同步请求排入同一条 FIFO 队列，
// 由单个 worker 串行消费。注册响应保持 202 Accepted / pending 语义：
// 运行态启动与后端初始化在响应之后完成，失败只记日志并写入 Server 状态，不回写已返回的响应。
type Lifecycle struct {
	registry ServerRegistry
	syncer   ToolSynchronizer
	auditor  AuditRecorder
	runtimes RuntimeConverger
	catalog  CatalogInvalidator
	logger   *slog.Logger

	lister            ServerLister
	reconcileInterval time.Duration

	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	reconcileDone chan struct{}
	wake          chan struct{}

	mu       sync.Mutex
	queue    []server.ID
	queued   map[server.ID]struct{}
	inFlight map[server.ID]struct{}
	closed   bool
}

// NewLifecycle 启动后台 worker；配置了 Lister 时同时启动启动重放与周期巡检。
func NewLifecycle(opts LifecycleOptions) (*Lifecycle, error) {
	if opts.Registry == nil {
		return nil, errors.New("new lifecycle: server registry is required")
	}
	if opts.Syncer == nil {
		return nil, errors.New("new lifecycle: tool synchronizer is required")
	}
	if opts.Auditor == nil {
		return nil, errors.New("new lifecycle: audit recorder is required")
	}
	if opts.Runtime == nil {
		return nil, errors.New("new lifecycle: runtime converger is required")
	}
	if opts.Catalog == nil {
		return nil, errors.New("new lifecycle: catalog invalidator is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := &Lifecycle{
		registry:          opts.Registry,
		syncer:            opts.Syncer,
		auditor:           opts.Auditor,
		runtimes:          opts.Runtime,
		catalog:           opts.Catalog,
		logger:            logger,
		lister:            opts.Lister,
		reconcileInterval: opts.ReconcileInterval,
		ctx:               ctx,
		cancel:            cancel,
		done:              make(chan struct{}),
		wake:              make(chan struct{}, 1),
		queued:            make(map[server.ID]struct{}),
		inFlight:          make(map[server.ID]struct{}),
	}
	go lifecycle.run()
	if opts.Lister != nil {
		lifecycle.reconcileDone = make(chan struct{})
		go lifecycle.reconcile()
	}
	return lifecycle, nil
}

// Register 持久化 Server，并在成功后异步触发运行态协调。
func (l *Lifecycle) Register(ctx context.Context, in server.RegisterInput) (server.Server, error) {
	srv, err := l.registry.Register(ctx, in)
	if err != nil {
		return server.Server{}, err
	}
	l.Trigger(srv.ID)
	return srv, nil
}

// Get 直接委托注册服务。
func (l *Lifecycle) Get(ctx context.Context, id server.ID) (server.Server, error) {
	return l.registry.Get(ctx, id)
}

// List 直接委托注册服务；排序由仓储保证。
func (l *Lifecycle) List(ctx context.Context) ([]server.Server, error) {
	return l.registry.List(ctx)
}

// Update 全量替换 Server 的可变配置（revision 乐观锁）。成功后失效目录缓存并把运行态收敛排入队列：
// 配置变了，实例与工具集都得按新配置重建。
// 写库失败时既不失效缓存也不触发收敛——失败的写不产生任何可见性变化。
func (l *Lifecycle) Update(ctx context.Context, id server.ID, expectedRevision int64, in server.UpdateInput) (server.Server, error) {
	srv, err := l.registry.Update(ctx, id, expectedRevision, in)
	if err != nil {
		return server.Server{}, err
	}
	l.invalidateCatalog()
	l.Trigger(srv.ID)
	return srv, nil
}

// SetEnabled 切换启用位（运行意图，不递增 Revision）。启用后只有期望运行态的 Server 才排队收敛；
// 停用走同步回收，返回的快照必须带上收敛后的 phase（stopped），否则调用方会以为实例还在跑。
// 幂等：已是目标值时注册服务不写库，但回收/收敛仍会执行一次，让 degraded 的 Server 有自愈机会。
func (l *Lifecycle) SetEnabled(ctx context.Context, id server.ID, enabled bool) (server.Server, error) {
	srv, err := l.registry.SetEnabled(ctx, id, enabled)
	if err != nil {
		return server.Server{}, err
	}
	l.invalidateCatalog()
	if enabled {
		if srv.Runnable() {
			l.Trigger(srv.ID)
		}
		return srv, nil
	}
	if err := l.runtimes.Reconcile(ctx, id); err != nil {
		return server.Server{}, err
	}
	return l.registry.Get(ctx, id)
}

// Start 把期望运行态置为 running 并排队收敛。disabled 的 Server 直接拒绝且不写库、不触发：
// 启用是 :enable 的职责，start 不该顺手改 enabled。
// 已是 running 时写库幂等，但仍触发一次收敛，让 degraded 的 Server 有自愈机会。
func (l *Lifecycle) Start(ctx context.Context, id server.ID) (server.Server, error) {
	srv, err := l.registry.Get(ctx, id)
	if err != nil {
		return server.Server{}, err
	}
	if !srv.Enabled {
		return server.Server{}, fmt.Errorf("%w: server is disabled", server.ErrNotRunnable)
	}
	updated, err := l.registry.SetDesiredState(ctx, id, server.DesiredRunning)
	if err != nil {
		return server.Server{}, err
	}
	l.invalidateCatalog()
	l.Trigger(id)
	return updated, nil
}

// Stop 把期望运行态置为 stopped，并同步回收实例：响应里的 phase 必须已经是 stopped，
// 排队回收会让调用方在停止失败时误以为已经停止。
func (l *Lifecycle) Stop(ctx context.Context, id server.ID) (server.Server, error) {
	if _, err := l.registry.Get(ctx, id); err != nil {
		return server.Server{}, err
	}
	if _, err := l.registry.SetDesiredState(ctx, id, server.DesiredStopped); err != nil {
		return server.Server{}, err
	}
	l.invalidateCatalog()
	if err := l.runtimes.Reconcile(ctx, id); err != nil {
		return server.Server{}, err
	}
	return l.registry.Get(ctx, id)
}

// Restart 换一个实例而不改配置：同步停掉旧实例，再排队让 worker 重建并复核 Tool 快照。
// 不写库、不递增 Revision，因此巡检不会把 observedRevision != revision 当成漂移。
// 仅对 Runnable 的 Server 有意义，否则返回 ErrNotRunnable（由 HTTP 层映射为 409）。
func (l *Lifecycle) Restart(ctx context.Context, id server.ID) (server.Server, error) {
	srv, err := l.registry.Get(ctx, id)
	if err != nil {
		return server.Server{}, err
	}
	if !srv.Runnable() {
		return server.Server{}, fmt.Errorf("%w: server is not runnable", server.ErrNotRunnable)
	}
	if err := l.runtimes.Stop(ctx, id); err != nil {
		return server.Server{}, err
	}
	l.invalidateCatalog()
	l.Trigger(id)
	return l.registry.Get(ctx, id)
}

// RefreshTools 为运行中的 Server 排队一次 Tool 快照刷新，并返回当前 Server。
// 未运行（disabled 或 desiredState=stopped）时返回 ErrNotRunnable，
// 避免把必然失败的同步塞进队列。
func (l *Lifecycle) RefreshTools(ctx context.Context, id server.ID) (server.Server, error) {
	srv, err := l.registry.Get(ctx, id)
	if err != nil {
		return server.Server{}, err
	}
	if !srv.Runnable() {
		return server.Server{}, fmt.Errorf("%w: disabled or desired state is stopped", server.ErrNotRunnable)
	}
	l.Trigger(srv.ID)
	return srv, nil
}

// Trigger 排队一次异步协调。同一 Server 已在队列中或正在同步时忽略重复触发；
// 本方法不做阻塞发送，请求不会被后台同步拖慢。
func (l *Lifecycle) Trigger(id server.ID) {
	if id == "" {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if _, exists := l.queued[id]; exists {
		l.mu.Unlock()
		return
	}
	// 正在同步的 Server 不再入队：本次同步的失败会由下一次巡检重新触发，
	// 否则长同步会在每个 tick 后立刻被重复消费一遍。
	if _, busy := l.inFlight[id]; busy {
		l.mu.Unlock()
		return
	}
	l.queued[id] = struct{}{}
	l.queue = append(l.queue, id)
	l.mu.Unlock()

	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Close 停止接受新触发、取消在飞同步并等待 worker 与 Reconciler 退出。
// 可重复调用；ctx 到期返回其错误，进程可继续退出。
func (l *Lifecycle) Close(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()

	l.cancel()
	for _, done := range []chan struct{}{l.done, l.reconcileDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (l *Lifecycle) run() {
	defer close(l.done)
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.wake:
		}
		// 排空队列后再等待下一个信号；期间的 Trigger 会在本轮被消费。
		for {
			id, ok := l.next()
			if !ok {
				break
			}
			l.syncServer(id)
		}
	}
}

func (l *Lifecycle) next() (server.ID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		return "", false
	}
	id := l.queue[0]
	l.queue = l.queue[1:]
	delete(l.queued, id)
	l.inFlight[id] = struct{}{}
	return id, true
}

func (l *Lifecycle) syncServer(id server.ID) {
	defer func() {
		l.mu.Lock()
		delete(l.inFlight, id)
		l.mu.Unlock()
	}()

	// 审计用不可取消的 ctx：同步失败常常正是因为生命周期被取消（关停），
	// 而这种失败恰恰是最需要留下记录的事实。
	auditCtx := context.WithoutCancel(l.ctx)

	result, err := l.syncer.Sync(l.ctx, id)
	if err != nil {
		l.logger.Error("tool sync failed", "server_id", id, "error", err)
		l.record(auditCtx, audit.Input{
			Type:      audit.EventToolSyncFailed,
			Outcome:   audit.OutcomeOf(err),
			AssetID:   string(id),
			AssetName: l.assetName(auditCtx, id),
			Target:    string(id),
			ErrorCode: audit.ErrorCode(err),
		})

		return
	}
	l.logger.Info("tool catalog refreshed",
		"server_id", id,
		"changed", result.Changed,
		"generation", result.Snapshot.Generation,
		"tool_count", result.Snapshot.ToolCount,
	)
	l.record(auditCtx, audit.Input{
		Type:      audit.EventToolSnapshotPublished,
		Outcome:   audit.OutcomeSuccess,
		AssetID:   string(id),
		AssetName: l.assetName(auditCtx, id),
		Target:    result.Snapshot.ID,
		Detail: audit.Detail(map[string]any{
			"changed":    result.Changed,
			"generation": result.Snapshot.Generation,
			"tool_count": result.Snapshot.ToolCount,
		}),
	})
}

// assetName 为审计事件补一个可读的资产名。查不到就留空：
// 事件的价值在 id 与结局，名字只是给人看的冗余字段。
func (l *Lifecycle) assetName(ctx context.Context, id server.ID) string {
	srv, err := l.registry.Get(ctx, id)
	if err != nil {
		return ""
	}

	return srv.Name
}

// invalidateCatalog 让进程内目录缓存失效，下一次读取会从仓储重建。
// 可见性变化与队列收敛是两个独立后果：写库成功后缓存必须失效，即使后续动作失败。
func (l *Lifecycle) invalidateCatalog() {
	if l.catalog == nil {
		return
	}
	l.catalog.Invalidate()
}

func (l *Lifecycle) record(ctx context.Context, in audit.Input) {
	if l.auditor == nil {
		return
	}
	if err := l.auditor.Record(ctx, in); err != nil {
		l.logger.Error("record audit event", "event_type", in.Type, "error", err)
	}
}
