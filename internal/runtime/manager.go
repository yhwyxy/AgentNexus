package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"time"
)

var (
	ErrServerDisabled      = errors.New("runtime server is disabled")
	ErrDesiredStateStopped = errors.New("runtime server is not desired running")
	ErrProviderNotFound    = errors.New("runtime provider not found")
	ErrInvalidInstance     = errors.New("runtime provider returned an invalid instance")
	ErrManagerClosed       = errors.New("runtime manager is closed")
)

const runtimePhaseRunning = "running"

type managedInstance struct {
	instance Instance
	provider Provider
}

// ProviderManager 按 Server 协调 Provider，并仅在进程内保留活动实例。
type ProviderManager struct {
	servers   server.Repository
	providers map[server.RuntimeType]Provider
	locks     keyedLocker

	mu     sync.Mutex
	active map[server.ID]managedInstance
	closed bool
}

var _ Manager = (*ProviderManager)(nil)

func NewManager(servers server.Repository, providers ...Provider) (*ProviderManager, error) {
	if servers == nil {
		return nil, errors.New("runtime manager requires a server repository")
	}
	if len(providers) == 0 {
		return nil, errors.New("runtime manager requires at least one provider")
	}

	registered := make(map[server.RuntimeType]Provider, len(providers))
	for _, provider := range providers {
		if provider == nil {
			return nil, errors.New("runtime manager provider must not be nil")
		}
		t := provider.Type()
		if t == "" {
			return nil, errors.New("runtime manager provider type must not be empty")
		}
		if _, exists := registered[t]; exists {
			return nil, fmt.Errorf("runtime manager has duplicate provider for %q", t)
		}
		registered[t] = provider
	}

	return &ProviderManager{
		servers:   servers,
		providers: registered,
		active:    make(map[server.ID]managedInstance),
	}, nil
}

func (m *ProviderManager) EnsureReady(ctx context.Context, requested server.Server) (Instance, error) {
	if requested.ID == "" {
		return Instance{}, errors.New("ensure runtime: server ID is required")
	}
	if m.isClosed() {
		return Instance{}, ErrManagerClosed
	}
	unlock := m.locks.lock(requested.ID)
	defer unlock()

	srv, err := m.servers.GetByID(ctx, requested.ID)
	if err != nil {
		return Instance{}, fmt.Errorf("load server for runtime ensure: %w", err)
	}
	return m.ensureLoaded(ctx, srv)
}

func (m *ProviderManager) ensureLoaded(ctx context.Context, srv server.Server) (Instance, error) {
	if !srv.Enabled {
		return Instance{}, ErrServerDisabled
	}
	if srv.Spec.DesiredState != server.DesiredRunning {
		return Instance{}, ErrDesiredStateStopped
	}

	cached, hasCached := m.getActive(srv.ID)
	if hasCached {
		sameConfig := cached.instance.ObservedRevision == srv.Revision && cached.provider.Type() == srv.Spec.Runtime.Type
		if sameConfig {
			inspected, err := cached.provider.Inspect(ctx, cached.instance)
			if err == nil && validInstance(inspected, srv, cached.provider) && inspected.ID == cached.instance.ID {
				m.setActive(srv.ID, managedInstance{instance: inspected, provider: cached.provider})
				if err := m.persistEnsured(ctx, srv, inspected); err != nil {
					return Instance{}, err
				}
				return inspected, nil
			}
		}

		if err := cached.provider.Stop(ctx, cached.instance); err != nil {
			stopErr := fmt.Errorf("stop stale runtime instance: %w", err)
			return Instance{}, m.recordFailure(ctx, srv, "runtime ensure failed", stopErr)
		}
		m.deleteActive(srv.ID)
	}

	provider, ok := m.providers[srv.Spec.Runtime.Type]
	if !ok {
		err := fmt.Errorf("%w: %q", ErrProviderNotFound, srv.Spec.Runtime.Type)
		return Instance{}, m.recordFailure(ctx, srv, "runtime ensure failed", err)
	}

	instance, err := provider.Ensure(ctx, srv)
	if err != nil {
		return Instance{}, m.recordFailure(ctx, srv, "runtime ensure failed", fmt.Errorf("ensure runtime provider: %w", err))
	}
	if !validInstance(instance, srv, provider) {
		cleanupErr := provider.Stop(ctx, instance)
		invalidErr := fmt.Errorf("%w for server %q", ErrInvalidInstance, srv.ID)
		if cleanupErr != nil {
			invalidErr = errors.Join(invalidErr, fmt.Errorf("clean up invalid runtime instance: %w", cleanupErr))
		}
		return Instance{}, m.recordFailure(ctx, srv, "runtime ensure failed", invalidErr)
	}

	m.setActive(srv.ID, managedInstance{instance: instance, provider: provider})
	if err := m.persistEnsured(ctx, srv, instance); err != nil {
		return Instance{}, err
	}
	return instance, nil
}

func (m *ProviderManager) Stop(ctx context.Context, id server.ID) error {
	if id == "" {
		return errors.New("stop runtime: server ID is required")
	}
	if m.isClosed() {
		return ErrManagerClosed
	}
	unlock := m.locks.lock(id)
	defer unlock()

	srv, err := m.servers.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load server for runtime stop: %w", err)
	}
	return m.stopLoaded(ctx, srv)
}

func (m *ProviderManager) stopLoaded(ctx context.Context, srv server.Server) error {
	cached, ok := m.getActive(srv.ID)
	if ok {
		if err := cached.provider.Stop(ctx, cached.instance); err != nil {
			return m.recordFailure(ctx, srv, "runtime stop failed", fmt.Errorf("stop runtime provider: %w", err))
		}
		m.deleteActive(srv.ID)
	}

	status := srv.Status.CreateInput()
	status.Phase = server.PhaseStopped
	status.Message = ""
	status.ConsecutiveFailures = 0
	if err := m.servers.UpdateStatus(ctx, srv.ID, status); err != nil {
		return fmt.Errorf("persist stopped runtime status: %w", err)
	}
	return nil
}

func (m *ProviderManager) Reconcile(ctx context.Context, id server.ID) error {
	if id == "" {
		return errors.New("reconcile runtime: server ID is required")
	}
	if m.isClosed() {
		return ErrManagerClosed
	}
	unlock := m.locks.lock(id)
	defer unlock()

	srv, err := m.servers.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load server for runtime reconcile: %w", err)
	}
	if srv.Enabled && srv.Spec.DesiredState == server.DesiredRunning {
		_, err := m.ensureLoaded(ctx, srv)
		return err
	}
	return m.stopLoaded(ctx, srv)
}

// Close 停止全部活动实例并调用实现了 Releaser 的 Provider 做最终回收。
// 关闭后 EnsureReady/Stop/Reconcile 一律返回 ErrManagerClosed:应用退出的顺序是
// 先停 HTTP 与生命周期队列,再 Close 本管理器,最后关闭 MCP session。
func (m *ProviderManager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	var errs []error
	for {
		pending := m.drainActive()
		if len(pending) == 0 {
			break
		}
		ids := make([]server.ID, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			managed := pending[id]
			unlock := m.locks.lock(id)
			if err := managed.provider.Stop(ctx, managed.instance); err != nil {
				errs = append(errs, fmt.Errorf("stop runtime instance for server %q: %w", id, err))
			}
			unlock()
		}
	}
	for _, provider := range m.sortedProviders() {
		releaser, ok := provider.(Releaser)
		if !ok {
			continue
		}
		if err := releaser.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close runtime provider %q: %w", provider.Type(), err))
		}
	}
	return errors.Join(errs...)
}

func (m *ProviderManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// drainActive 取走当前全部活动实例。Close 与并发的 Ensure 可能交错,
// 因此循环取用直到为空,而不是只取一次快照。
func (m *ProviderManager) drainActive() map[server.ID]managedInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := m.active
	m.active = make(map[server.ID]managedInstance)
	return pending
}

func (m *ProviderManager) sortedProviders() []Provider {
	types := make([]server.RuntimeType, 0, len(m.providers))
	for runtimeType := range m.providers {
		types = append(types, runtimeType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	providers := make([]Provider, 0, len(types))
	for _, runtimeType := range types {
		providers = append(providers, m.providers[runtimeType])
	}
	return providers
}

// persistEnsured 记录运行态观测结果（ObservedRevision/LastSuccessAt）。
// 依据详细设计 §3.4 状态机，EnsureReady 只负责把 Server 推进到 starting；
// ready 必须等 Tool 快照刷新成功后才由 tool.SyncService 写入，否则会出现
// phase=ready 但 tools/list 仍为空的中间态（§10.1 的终态是 Catalog Reload
// 与 Server Ready 同时成立）。
func (m *ProviderManager) persistEnsured(ctx context.Context, srv server.Server, instance Instance) error {
	now := time.Now().UTC()
	status := srv.Status.CreateInput()
	status.ObservedRevision = instance.ObservedRevision
	status.LastSuccessAt = &now
	status.Message = ""
	status.ConsecutiveFailures = 0
	if entersStarting(srv.Status, instance) {
		status.Phase = server.PhaseStarting
	}
	if err := m.servers.UpdateStatus(ctx, srv.ID, status); err != nil {
		return fmt.Errorf("persist ensured runtime status: %w", err)
	}
	return nil
}

// entersStarting 判断本次 Ensure 是否代表"运行态刚具备能力"：应用了新 revision，
// 或此前状态尚未进入 starting/ready/degraded。已经 ready 的 Server 被 tools/call
// 路径重复 Ensure 时保持 ready，不产生状态抖动。
func entersStarting(status server.Status, instance Instance) bool {
	if status.ObservedRevision != instance.ObservedRevision {
		return true
	}
	switch status.Phase {
	case server.PhaseStarting, server.PhaseReady, server.PhaseDegraded:
		return false
	}
	return true
}

func (m *ProviderManager) recordFailure(ctx context.Context, srv server.Server, message string, cause error) error {
	status := srv.Status.CreateInput()
	status.Phase = server.PhaseDegraded
	status.Message = message
	status.ConsecutiveFailures++
	if err := m.servers.UpdateStatus(ctx, srv.ID, status); err != nil {
		return errors.Join(cause, fmt.Errorf("persist degraded runtime status: %w", err))
	}
	return cause
}

func validInstance(instance Instance, srv server.Server, provider Provider) bool {
	return instance.ID != "" &&
		instance.ServerID == srv.ID &&
		instance.Provider == string(provider.Type()) &&
		instance.ObservedRevision == srv.Revision &&
		instance.Phase == runtimePhaseRunning
}

func (m *ProviderManager) getActive(id server.ID) (managedInstance, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, ok := m.active[id]
	return instance, ok
}

func (m *ProviderManager) setActive(id server.ID, instance managedInstance) {
	m.mu.Lock()
	m.active[id] = instance
	m.mu.Unlock()
}

func (m *ProviderManager) deleteActive(id server.ID) {
	m.mu.Lock()
	delete(m.active, id)
	m.mu.Unlock()
}

// keyedLocker 只在持有者和等待者均退出后释放对应的锁条目。
type keyedLocker struct {
	mu    sync.Mutex
	locks map[server.ID]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (l *keyedLocker) lock(id server.ID) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[server.ID]*keyedLock)
	}
	entry := l.locks[id]
	if entry == nil {
		entry = &keyedLock{}
		l.locks[id] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, id)
		}
		l.mu.Unlock()
	}
}
