// docker Provider 负责容器归属:创建/复用容器、按 transport 组装连接目标、
// 观测容器状态并回收。它不决定 public tool name,也不执行 SQL。
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

const (
	phaseRunning = "running"

	// defaultConnectHost 是控制面访问容器发布端口时使用的宿主地址;
	// 容器发布端口默认只绑定回环,集群场景由后续切片引入 publishHost 配置。
	defaultConnectHost  = "127.0.0.1"
	defaultPublishHost  = "127.0.0.1"
	defaultStopGrace    = 5 * time.Second
	defaultLogBuffer    = 64 << 10
	defaultEndpointPath = "/mcp"

	pollInterval = 50 * time.Millisecond

	// 容器标签:`agentnexus.server-id` 用于重启后接管自己的容器,
	// `agentnexus.revision` 用于判定容器是否已经承载期望配置。
	labelServerID     = "agentnexus.server-id"
	labelRevision     = "agentnexus.revision"
	containerNameBase = "agentnexus-"
)

var (
	// ErrInstanceUnavailable:实例仍登记在本 Provider 中但已不可用
	// (attach 断开,或容器已退出/被移除)。ProviderManager 据此重建。
	ErrInstanceUnavailable = errors.New("docker instance unavailable")

	// ErrProviderClosed:Provider 已关闭,不再接受新的 Ensure。
	ErrProviderClosed = errors.New("docker provider is closed")
)

// Options 是 Provider 的构造参数。API 与 Host 二选一:测试注入假实现,
// 生产按 DOCKER_HOST(空则用平台默认 socket)构造真实客户端。
type Options struct {
	API    api
	Host   string
	Policy server.SpecPolicy

	ConnectHost string // 控制面连接发布端口用的宿主地址,默认 127.0.0.1
	PublishHost string // 容器端口发布到宿主的绑定地址,默认 127.0.0.1
	StopGrace   time.Duration
	LogBuffer   int
}

// Provider 实现 runtime.Provider 与 runtime.Releaser。
type Provider struct {
	api         api
	policy      server.SpecPolicy
	connectHost string
	publishHost netip.Addr
	stopGrace   time.Duration
	logBuffer   int

	mu     sync.Mutex
	states map[string]*instanceState
	closed bool
}

var (
	_ runtime.Provider = (*Provider)(nil)
	_ runtime.Releaser = (*Provider)(nil)
)

func NewProvider(opts Options) (*Provider, error) {
	engine := opts.API
	if engine == nil {
		created, err := newClient(opts.Host)
		if err != nil {
			return nil, fmt.Errorf("create docker client: %w", err)
		}
		engine = created
	}

	provider := &Provider{
		api:         engine,
		policy:      opts.Policy,
		connectHost: opts.ConnectHost,
		stopGrace:   opts.StopGrace,
		logBuffer:   opts.LogBuffer,
		states:      map[string]*instanceState{},
	}
	if provider.connectHost == "" {
		provider.connectHost = defaultConnectHost
	}
	publishHost := opts.PublishHost
	if publishHost == "" {
		publishHost = defaultPublishHost
	}
	parsedHost, err := netip.ParseAddr(publishHost)
	if err != nil {
		return nil, fmt.Errorf("invalid publish host %q: %w", publishHost, err)
	}
	provider.publishHost = parsedHost
	if provider.stopGrace <= 0 {
		provider.stopGrace = defaultStopGrace
	}
	if provider.logBuffer <= 0 {
		provider.logBuffer = defaultLogBuffer
	}
	return provider, nil
}

func (p *Provider) Type() server.RuntimeType {
	return server.RuntimeDocker
}

// Ensure 让容器处于运行态并返回可用的连接目标:
// 校验 spec -> 按标签接管/复用容器 -> 创建并启动 -> 组装 ConnectTarget。
// 它不写数据库;失败原因由 ProviderManager 记录为运行态事实。
func (p *Provider) Ensure(ctx context.Context, srv server.Server) (runtime.Instance, error) {
	docker := srv.Spec.Runtime.Docker
	if srv.Spec.Runtime.Type != server.RuntimeDocker || docker == nil {
		return runtime.Instance{}, errors.New("docker provider requires docker runtime")
	}
	// 运行态只信任当前 spec:默认值由注册期填充,这里只做形状校验。
	if err := srv.Validate(); err != nil {
		return runtime.Instance{}, fmt.Errorf("validate docker runtime spec: %w", err)
	}
	// 策略在注册期已判一次,这里再判一次:配置校验与运行态之间可能被改库或换配置。
	if p.policy != nil {
		if err := p.policy.CheckDocker(*docker); err != nil {
			return runtime.Instance{}, err
		}
	}
	if p.isClosed() {
		return runtime.Instance{}, ErrProviderClosed
	}

	containerID, reused, err := p.acquireContainer(ctx, srv, *docker)
	if err != nil {
		return runtime.Instance{}, err
	}
	if !reused {
		if _, err := p.api.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
			return runtime.Instance{}, fmt.Errorf("start container: %w", err)
		}
	}

	state, err := p.newInstanceState(ctx, srv, *docker, containerID)
	if err != nil {
		return runtime.Instance{}, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = p.stopState(context.WithoutCancel(ctx), state)
		return runtime.Instance{}, ErrProviderClosed
	}
	p.states[state.instance.ID] = state
	p.mu.Unlock()

	return state.instance, nil
}

// acquireContainer 返回应当承载当前 revision 的容器 ID:
// 已跑着且 revision 匹配的容器直接复用,其余的按标签清理;
// 创建前还要清掉同名的无标签残留(上次进程崩溃或手工 `docker run --name`)。
func (p *Provider) acquireContainer(ctx context.Context, srv server.Server, docker server.DockerSpec) (string, bool, error) {
	owned, err := p.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", labelServerID+"="+string(srv.ID)),
	})
	if err != nil {
		return "", false, fmt.Errorf("list owned containers: %w", err)
	}

	revision := strconv.FormatInt(srv.Revision, 10)
	reusable := ""
	for _, summary := range owned.Items {
		if reusable == "" && summary.State == container.StateRunning && summary.Labels[labelRevision] == revision {
			reusable = summary.ID
			continue
		}
		if err := p.removeContainer(ctx, summary.ID); err != nil {
			return "", false, err
		}
	}
	if reusable != "" {
		return reusable, true, nil
	}

	name := containerNameBase + string(srv.ID)
	stale, err := p.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case err == nil && stale.Container.ID != "":
		if err := p.removeContainer(ctx, stale.Container.ID); err != nil {
			return "", false, err
		}
	case err != nil && !errdefs.IsNotFound(err):
		return "", false, fmt.Errorf("inspect container by name: %w", err)
	}

	created, err := p.createContainer(ctx, srv, docker, name)
	if err != nil {
		return "", false, err
	}
	return created, false, nil
}

func (p *Provider) createContainer(ctx context.Context, srv server.Server, docker server.DockerSpec, name string) (string, error) {
	if err := p.ensureImage(ctx, srv, docker.Image); err != nil {
		return "", err
	}

	config := &container.Config{
		Image: docker.Image,
		Env:   sortedEnv(docker.Env),
		Labels: map[string]string{
			labelServerID: string(srv.ID),
			labelRevision: strconv.FormatInt(srv.Revision, 10),
		},
	}
	if len(docker.Command) > 0 {
		config.Cmd = docker.Command
	}

	hostConfig := &container.HostConfig{
		Mounts:    mounts(docker.Mounts),
		Resources: container.Resources{Memory: docker.MemoryBytes, NanoCPUs: int64(docker.CPUs * 1e9)},
	}
	if docker.NetworkMode != "" {
		hostConfig.NetworkMode = container.NetworkMode(docker.NetworkMode)
	}

	switch srv.Spec.Transport {
	case server.TransportStdio:
		// MCP 协议独占 stdin/stdout,因此容器必须保持 stdin 打开且不做 tty 分配
		// (分配 tty 会破坏 stdcopy 的帧格式)。
		config.AttachStdin, config.AttachStdout, config.AttachStderr = true, true, true
		config.OpenStdin = true
	case server.TransportStreamableHTTP:
		port := network.MustParsePort(fmt.Sprintf("%d/tcp", docker.Port))
		config.ExposedPorts = network.PortSet{port: struct{}{}}
		hostConfig.PortBindings = network.PortMap{port: []network.PortBinding{{HostIP: p.publishHost}}}
	default:
		return "", fmt.Errorf("unsupported transport %q for docker runtime", srv.Spec.Transport)
	}

	created, err := p.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name,
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return created.ID, nil
}

// ensureImage 只在镜像缺失时拉取;拉取受 connect 超时约束,避免启动被无限阻塞。
func (p *Provider) ensureImage(ctx context.Context, srv server.Server, ref string) error {
	_, err := p.api.ImageInspect(ctx, ref)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %q: %w", ref, err)
	}

	pullCtx, cancel := pullContext(ctx, srv.Spec.Timeouts.Connect)
	defer cancel()
	response, err := p.api.ImagePull(pullCtx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	defer response.Close()
	// Wait 读完进度流并回报 daemon 侧的失败,比单纯 drain 更早暴露鉴权/网络错误。
	if err := response.Wait(pullCtx); err != nil {
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	return nil
}

func (p *Provider) newInstanceState(ctx context.Context, srv server.Server, docker server.DockerSpec, containerID string) (*instanceState, error) {
	state := &instanceState{
		containerID: containerID,
		instance: runtime.Instance{
			ID:               fmt.Sprintf("%s-docker-%s", srv.ID, shortID(containerID)),
			ServerID:         srv.ID,
			Provider:         string(server.RuntimeDocker),
			ExternalID:       containerID,
			Phase:            phaseRunning,
			ObservedRevision: srv.Revision,
		},
	}

	switch srv.Spec.Transport {
	case server.TransportStdio:
		attached, err := p.api.ContainerAttach(ctx, containerID, client.ContainerAttachOptions{
			Stream: true, Stdin: true, Stdout: true, Stderr: true,
		})
		if err != nil {
			return nil, fmt.Errorf("attach container %s: %w", shortID(containerID), err)
		}
		reader, writer := io.Pipe()
		state.resp = &attached.HijackedResponse
		state.reader = reader
		state.writer = writer
		state.attached = true
		streams := state.newStreams()
		state.instance.Target = runtime.ConnectTarget{
			Transport: string(server.TransportStdio),
			Command:   docker.Image,
			Args:      append([]string(nil), docker.Command...),
			Env:       sortedEnv(docker.Env),
			Streams:   &streams,
		}
		state.startDemux()
	case server.TransportStreamableHTTP:
		addr, err := p.publishedAddr(ctx, containerID, docker.Port)
		if err != nil {
			return nil, err
		}
		path := docker.EndpointPath
		if path == "" {
			path = defaultEndpointPath
		}
		// 容器进程可能还在监听前就被 attach 到;先探活再交给 MCP 客户端,
		// 否则 connect 阶段的失败原因会被 SDK 的错误信息掩盖。
		if err := waitReady(ctx, addr, srv.Spec.Timeouts.Connect); err != nil {
			return nil, err
		}
		state.instance.Target = runtime.ConnectTarget{
			Transport: string(server.TransportStreamableHTTP),
			URL:       "http://" + addr + path,
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q for docker runtime", srv.Spec.Transport)
	}

	return state, nil
}

// publishedAddr 从 daemon 读回容器端口的实际绑定,而不是复用 spec 里的端口:
// 端口冲突时 daemon 会换一个宿主端口,只有 inspect 才知道最终地址。
func (p *Provider) publishedAddr(ctx context.Context, containerID string, port int) (string, error) {
	inspect, err := p.api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect container port binding: %w", err)
	}
	settings := inspect.Container.NetworkSettings
	if settings == nil {
		return "", fmt.Errorf("container %s has no network settings", shortID(containerID))
	}
	for _, binding := range settings.Ports[network.MustParsePort(fmt.Sprintf("%d/tcp", port))] {
		if binding.HostPort != "" {
			return net.JoinHostPort(p.connectHost, binding.HostPort), nil
		}
	}
	return "", fmt.Errorf("container port %d is not published on the host", port)
}

// Inspect 只报告事实:实例已被本 Provider 淘汰、attach 断开、容器不再运行、
// 容器标签的 revision 与实例不一致,都返回 ErrInstanceUnavailable。
func (p *Provider) Inspect(ctx context.Context, instance runtime.Instance) (runtime.Instance, error) {
	state, ok := p.lookup(instance.ID)
	if !ok {
		return runtime.Instance{}, fmt.Errorf("%w: instance %q is not managed by this provider", ErrInstanceUnavailable, instance.ID)
	}
	if instance.ServerID != "" && instance.ServerID != state.instance.ServerID {
		return runtime.Instance{}, fmt.Errorf("%w: instance %q belongs to another server", ErrInstanceUnavailable, instance.ID)
	}
	if err := state.unavailable(); err != nil {
		return runtime.Instance{}, err
	}

	inspect, err := p.api.ContainerInspect(ctx, state.containerID, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return runtime.Instance{}, fmt.Errorf("%w: container %s no longer exists", ErrInstanceUnavailable, shortID(state.containerID))
		}
		return runtime.Instance{}, fmt.Errorf("inspect container %s: %w", shortID(state.containerID), err)
	}
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		return runtime.Instance{}, fmt.Errorf("%w: container %s is not running", ErrInstanceUnavailable, shortID(state.containerID))
	}
	if labels := containerLabels(inspect.Container); labels[labelRevision] != strconv.FormatInt(state.instance.ObservedRevision, 10) {
		return runtime.Instance{}, fmt.Errorf("%w: container %s does not match instance revision %d",
			ErrInstanceUnavailable, shortID(state.containerID), state.instance.ObservedRevision)
	}
	return state.instance, nil
}

// Stop 停容器并移除,同时清理内存状态;幂等(重复调用或实例已被清理都返回 nil)。
func (p *Provider) Stop(ctx context.Context, instance runtime.Instance) error {
	state, ok := p.lookup(instance.ID)
	if !ok {
		return nil
	}
	return p.stopState(ctx, state)
}

func (p *Provider) stopState(ctx context.Context, state *instanceState) error {
	state.endSession(nil)
	p.forget(state.instance.ID)

	timeout := int(p.stopGrace.Round(time.Second) / time.Second)
	if timeout < 1 {
		timeout = 1
	}

	var errs []error
	if _, err := p.api.ContainerStop(ctx, state.containerID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("stop container %s: %w", shortID(state.containerID), err))
	}
	if _, err := p.api.ContainerRemove(ctx, state.containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("remove container %s: %w", shortID(state.containerID), err))
	}
	return errors.Join(errs...)
}

// removeContainer 用于清理"不属于当前 revision"的容器:停不掉也照样移除,避免残留占用名字。
func (p *Provider) removeContainer(ctx context.Context, containerID string) error {
	if _, err := p.api.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove stale container %s: %w", shortID(containerID), err)
	}
	return nil
}

// Close 实现 runtime.Releaser:标记关闭、回收全部容器后关闭 Engine 客户端。
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	states := make([]*instanceState, 0, len(p.states))
	for _, state := range p.states {
		states = append(states, state)
	}
	p.states = map[string]*instanceState{}
	p.mu.Unlock()

	var errs []error
	for _, state := range states {
		if err := p.stopState(ctx, state); err != nil {
			errs = append(errs, err)
		}
	}
	if err := p.api.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close docker client: %w", err))
	}
	return errors.Join(errs...)
}

func (p *Provider) lookup(id string) (*instanceState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.states[id]
	return state, ok
}

func (p *Provider) forget(id string) {
	p.mu.Lock()
	delete(p.states, id)
	p.mu.Unlock()
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Logs 返回容器日志。非 follow 时读完即返回(截断到 logBuffer 的尾部);
// follow 时用管道流式转交,由调用方 Close 结束(daemon 侧负责 tail,不会无界增长)。
func (p *Provider) Logs(ctx context.Context, instance runtime.Instance, opts runtime.LogOptions) (io.ReadCloser, error) {
	state, ok := p.lookup(instance.ID)
	if !ok {
		return nil, fmt.Errorf("%w: instance %q is not managed by this provider", ErrInstanceUnavailable, instance.ID)
	}

	reader, err := p.api.ContainerLogs(ctx, state.containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
	})
	if err != nil {
		return nil, fmt.Errorf("read container logs %s: %w", shortID(state.containerID), err)
	}

	if !opts.Follow {
		defer reader.Close()
		var buffer bytes.Buffer
		if _, err := stdcopy.StdCopy(&buffer, &buffer, reader); err != nil {
			return nil, fmt.Errorf("demultiplex container logs %s: %w", shortID(state.containerID), err)
		}
		return io.NopCloser(bytes.NewReader(lastBytes(buffer.Bytes(), p.logBuffer))), nil
	}

	pipeReader, pipeWriter := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pipeWriter, pipeWriter, reader)
		reader.Close()
		if err != nil && errors.Is(err, io.ErrClosedPipe) {
			err = nil
		}
		pipeWriter.CloseWithError(err)
	}()
	return pipeReader, nil
}

func pullContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func waitReady(ctx context.Context, addr string, timeout time.Duration) error {
	waitCtx, cancel := pullContext(ctx, timeout)
	defer cancel()

	dialer := net.Dialer{Timeout: pollInterval}
	for {
		conn, err := dialer.DialContext(waitCtx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("container endpoint %s did not become ready: %w", addr, err)
		case <-time.After(pollInterval):
		}
	}
}

func mounts(in []server.Mount) []mount.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(in))
	for _, item := range in {
		out = append(out, mount.Mount{
			Type:     mount.TypeBind,
			Source:   item.Source,
			Target:   item.Target,
			ReadOnly: item.ReadOnly,
		})
	}
	return out
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

func containerLabels(inspect container.InspectResponse) map[string]string {
	if inspect.Config == nil {
		return nil
	}
	return inspect.Config.Labels
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func lastBytes(data []byte, limit int) []byte {
	if limit <= 0 || len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}
