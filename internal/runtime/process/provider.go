// Package process 把本机 stdio 子进程转换成 SDK 无关的运行实例。
//
// 进程归属:Provider 通过 exec.CommandContext 创建并持有子进程与它的 stdin/stdout fd;
// MCP 协议独占 stdin/stdout,日志走 stderr。会话只借用包装过的标准流,关闭会话不会关掉
// 子进程 fd,但会让实例失效,由 ProviderManager 的 Inspect 失败路径负责回收重启。
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

const (
	phaseRunning = "running"

	defaultStopGrace = 3 * time.Second
	defaultLogBuffer = 64 << 10
)

var (
	// ErrInstanceUnavailable: 实例仍登记在本 Provider 中但已不可用
	// (子进程退出,或会话已关闭标准流)。ProviderManager 据此 reap 并重新 Ensure。
	ErrInstanceUnavailable = errors.New("process instance unavailable")
	// ErrProviderClosed: Provider 已经 Close,不再接受新的 Ensure。
	ErrProviderClosed = errors.New("process provider is closed")
)

// Option 覆盖 Provider 的默认行为,仅用于测试与装配。
type Option func(*Provider)

// WithStopGrace 设置 Stop 时 SIGTERM 到 SIGKILL 之间的宽限期。
func WithStopGrace(grace time.Duration) Option {
	return func(p *Provider) {
		if grace > 0 {
			p.stopGrace = grace
		}
	}
}

// WithLogBuffer 设置 stderr 环形缓冲区的字节上限(超出丢弃最旧内容)。
func WithLogBuffer(size int) Option {
	return func(p *Provider) {
		if size > 0 {
			p.logBuffer = size
		}
	}
}

// Provider 实现 runtime.Provider 与 runtime.Releaser。
type Provider struct {
	base      context.Context
	stopGrace time.Duration
	logBuffer int

	mu        sync.Mutex
	instances map[string]*instanceState
	closed    bool
}

var (
	_ runtime.Provider = (*Provider)(nil)
	_ runtime.Releaser = (*Provider)(nil)
)

// NewProvider 创建进程 Provider。base 决定子进程的存活边界:取消 base 会终止全部子进程
// (应用退出路径),因此这里传应用级 ctx,而不是单次请求 ctx。
func NewProvider(base context.Context, opts ...Option) *Provider {
	if base == nil {
		base = context.Background()
	}
	provider := &Provider{
		base:      base,
		stopGrace: defaultStopGrace,
		logBuffer: defaultLogBuffer,
		instances: make(map[string]*instanceState),
	}
	for _, opt := range opts {
		opt(provider)
	}
	return provider
}

func (p *Provider) Type() server.RuntimeType { return server.RuntimeProcess }

func (p *Provider) Ensure(_ context.Context, srv server.Server) (runtime.Instance, error) {
	if srv.Spec.Runtime.Type != server.RuntimeProcess || srv.Spec.Runtime.Process == nil {
		return runtime.Instance{}, errors.New("process provider requires process runtime")
	}
	spec := srv.Spec.Runtime.Process
	if !filepath.IsAbs(spec.Command) {
		return runtime.Instance{}, errors.New("process command must be an absolute path")
	}
	if p.isClosed() {
		return runtime.Instance{}, ErrProviderClosed
	}

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return runtime.Instance{}, fmt.Errorf("create process stdin pipe: %w", err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		return runtime.Instance{}, fmt.Errorf("create process stdout pipe: %w", err)
	}

	logs := newBoundedBuffer(p.logBuffer)
	cmd := exec.CommandContext(p.base, spec.Command, spec.Args...)
	cmd.Env = mergedEnv(spec.Env)
	cmd.Dir = spec.WorkingDir
	cmd.Stdin = stdinReader
	cmd.Stdout = stdoutWriter
	cmd.Stderr = logs
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
		return runtime.Instance{}, fmt.Errorf("start process: %w", err)
	}
	// 父进程不再需要子进程侧的 fd:留着 stdoutWriter 会让读取端永远等不到 EOF。
	stdinReader.Close()
	stdoutWriter.Close()

	pid := cmd.Process.Pid
	state := &instanceState{
		instance: runtime.Instance{
			ID:               fmt.Sprintf("%s-process-%d", srv.ID, pid),
			ServerID:         srv.ID,
			Provider:         string(server.RuntimeProcess),
			ExternalID:       strconv.Itoa(pid),
			Phase:            phaseRunning,
			ObservedRevision: srv.Revision,
			Target: runtime.ConnectTarget{
				Transport: string(server.TransportStdio),
				Command:   spec.Command,
				Args:      append([]string(nil), spec.Args...),
				Env:       sortedEnv(spec.Env),
			},
		},
		pid:    pid,
		stdin:  stdinWriter,
		stdout: stdoutReader,
		logs:   logs,
	}
	state.instance.Target.Streams = state.newStreams()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		stdinWriter.Close()
		stdoutReader.Close()
		terminateProcess(pid)
		waitForExit(cmd, p.stopGrace)
		return runtime.Instance{}, ErrProviderClosed
	}
	p.instances[state.instance.ID] = state
	p.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		state.markExited(waitErr)
	}()

	return state.instance, nil
}

func (p *Provider) Stop(ctx context.Context, instance runtime.Instance) error {
	state, ok := p.lookup(instance.ID)
	if !ok {
		// 实例已不在本 Provider 中:重复 Stop 是幂等的。
		return nil
	}
	return p.stopInstance(ctx, state)
}

func (p *Provider) Inspect(_ context.Context, instance runtime.Instance) (runtime.Instance, error) {
	state, ok := p.lookup(instance.ID)
	if !ok {
		return runtime.Instance{}, fmt.Errorf("%w: instance %q is not active", ErrInstanceUnavailable, instance.ID)
	}
	if state.instance.ServerID != instance.ServerID {
		return runtime.Instance{}, fmt.Errorf("%w: instance %q belongs to another server", ErrInstanceUnavailable, instance.ID)
	}
	if err := state.unavailableReason(); err != nil {
		return runtime.Instance{}, err
	}
	return state.instance, nil
}

// Logs 返回 stderr 环形缓冲区的快照;缓冲区只保留最近 logBuffer 字节,
// 因此快照可能不包含更早的输出。Follow 模式尚未实现。
func (p *Provider) Logs(_ context.Context, instance runtime.Instance, opts runtime.LogOptions) (io.ReadCloser, error) {
	if opts.Follow {
		return nil, errors.New("process logs: follow mode is not supported")
	}
	state, ok := p.lookup(instance.ID)
	if !ok {
		return nil, fmt.Errorf("%w: instance %q is not active", ErrInstanceUnavailable, instance.ID)
	}
	return io.NopCloser(bytes.NewReader(state.logs.snapshot())), nil
}

// Close 终止全部活动实例并标记 Provider 关闭;之后 Ensure 返回 ErrProviderClosed。
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	alreadyClosed := p.closed
	p.closed = true
	p.mu.Unlock()
	if alreadyClosed {
		return nil
	}

	var errs []error
	for {
		states := p.drainInstances()
		if len(states) == 0 {
			return errors.Join(errs...)
		}
		for _, state := range states {
			if err := p.stopInstance(ctx, state); err != nil {
				errs = append(errs, fmt.Errorf("stop process instance %q: %w", state.instance.ID, err))
			}
		}
	}
}

func (p *Provider) stopInstance(ctx context.Context, state *instanceState) error {
	var errs []error
	if !state.hasExited() {
		terminateProcess(state.pid)
		if !waitForState(ctx, state, p.stopGrace) {
			killProcess(state.pid)
			if !waitForState(ctx, state, p.stopGrace) {
				errs = append(errs, fmt.Errorf("process %d did not exit after SIGKILL", state.pid))
			}
		}
	}
	state.closeFiles()
	p.forget(state.instance.ID)
	return errors.Join(errs...)
}

func (p *Provider) lookup(id string) (*instanceState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.instances[id]
	return state, ok
}

func (p *Provider) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.instances, id)
}

func (p *Provider) drainInstances() []*instanceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	states := make([]*instanceState, 0, len(p.instances))
	for id, state := range p.instances {
		states = append(states, state)
		delete(p.instances, id)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].instance.ID < states[j].instance.ID })
	return states
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// waitForState 轮询实例退出状态,直到退出、ctx 结束或超时。
func waitForState(ctx context.Context, state *instanceState, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if state.hasExited() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return state.hasExited()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForExit 用于 Close 路径的兜底回收:在宽限期内等待 Wait 收敛。
func waitForExit(cmd *exec.Cmd, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// mergedEnv 合并父进程环境与 Server 显式声明的环境变量(后者优先),
// 并按 key 排序输出,避免重复 key 依赖 exec 的查找顺序。
func mergedEnv(overrides map[string]string) []string {
	merged := make(map[string]string, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+merged[key])
	}
	return env
}

func sortedEnv(in map[string]string) []string {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+in[key])
	}
	return out
}
