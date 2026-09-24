// 注册后的运行态协调：把新注册的 Server 交给 Tool 同步服务，
// 由 SyncService 内部的 RuntimeManager 完成 Runtime 启动与状态收敛。
package app

import (
	"context"
	"log/slog"
	"sync"

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

// Lifecycle 在注册成功后异步触发一次 Tool 同步。
// 注册响应保持 202 Accepted / pending 语义：运行态启动与后端初始化在响应之后完成，
// 失败只记日志，不回写已返回的响应。
type Lifecycle struct {
	registry ServerRegistry
	syncer   ToolSynchronizer
	logger   *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}

	mu     sync.Mutex
	queue  []server.ID
	queued map[server.ID]struct{}
	closed bool
}

// NewLifecycle 启动后台 worker。registry 与 syncer 不可为 nil。
func NewLifecycle(registry ServerRegistry, syncer ToolSynchronizer, logger *slog.Logger) *Lifecycle {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := &Lifecycle{
		registry: registry,
		syncer:   syncer,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		queued:   make(map[server.ID]struct{}),
	}
	go lifecycle.run()
	return lifecycle
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

// Trigger 排队一次异步协调。同一 Server 已在队列中时忽略重复触发；
// 本方法不做阻塞发送，注册请求不会被后台同步拖慢。
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
	l.queued[id] = struct{}{}
	l.queue = append(l.queue, id)
	l.mu.Unlock()

	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Close 停止接受新触发、取消在飞同步并等待 worker 退出。
// 可重复调用；ctx 到期返回其错误，进程可继续退出。
func (l *Lifecycle) Close(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()

	l.cancel()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	return id, true
}

func (l *Lifecycle) syncServer(id server.ID) {
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
