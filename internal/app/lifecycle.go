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

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// ServerRegistry 是 Lifecycle 对注册应用服务的最小依赖。
// 与 edge/httpapi.ServerRegistry 结构一致，可直接交给 HTTP 层。
type ServerRegistry interface {
	Register(ctx context.Context, in server.RegisterInput) (server.Server, error)
	Get(ctx context.Context, id server.ID) (server.Server, error)
}

// ToolSynchronizer 是 Lifecycle 对 Tool 同步服务的最小依赖。
type ToolSynchronizer interface {
	Sync(ctx context.Context, id server.ID) (tool.SyncResult, error)
}

// LifecycleOptions 是 Lifecycle 的装配参数。
type LifecycleOptions struct {
	// Registry 与 Syncer 必填。
	Registry ServerRegistry
	Syncer   ToolSynchronizer
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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := &Lifecycle{
		registry:          opts.Registry,
		syncer:            opts.Syncer,
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

	result, err := l.syncer.Sync(l.ctx, id)
	if err != nil {
		l.logger.Error("tool sync failed", "server_id", id, "error", err)
		return
	}
	l.logger.Info("tool catalog refreshed",
		"server_id", id,
		"changed", result.Changed,
		"generation", result.Snapshot.Generation,
		"tool_count", result.Snapshot.ToolCount,
	)
}
