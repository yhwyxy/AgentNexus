package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// fakeAPI 是内存版 Engine:只实现 Provider 需要的端点,并记录调用顺序,
// 让容器归属、复用与回收的判定可以在不依赖 daemon 的情况下断言。
type fakeAPI struct {
	mu sync.Mutex

	images   map[string]bool
	pulled   []string
	imageErr error

	containers map[string]*fakeContainer
	nextID     int

	created []*container.Config
	started []string
	stopped []string
	removed []string

	nextPort func() string

	attachPayload []byte
	// attachStayOpen 让 attach 流在前置负载读完后再阻塞到 Close,
	// 模拟真实容器"连接保持到 detach"的行为(否则解复用立即 EOF,实例被判失效)。
	attachStayOpen bool
	attachErr      error
	attachConns    map[string]*fakeConn

	inspectErr error
	listErr    error
	pullErr    error
	logs       []byte
	logsErr    error

	closed bool
}

type fakeContainer struct {
	id         string
	name       string
	config     *container.Config
	hostConfig *container.HostConfig
	running    bool
	port       string // 发布到宿主的端口;空表示未发布
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		images:      map[string]bool{},
		containers:  map[string]*fakeContainer{},
		attachConns: map[string]*fakeConn{},
	}
}

// containerState 返回容器的当前状态(不存在时为 nil),供断言容器是否仍在运行。
func (f *fakeAPI) containerState(id string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[id]
}

func (f *fakeAPI) ImageInspect(_ context.Context, ref string) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imageErr != nil {
		return client.ImageInspectResult{}, f.imageErr
	}
	if !f.images[ref] {
		return client.ImageInspectResult{}, fmt.Errorf("no such image %q: %w", ref, errdefs.ErrNotFound)
	}
	return client.ImageInspectResult{}, nil
}

func (f *fakeAPI) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imageErr != nil {
		return nil, f.imageErr
	}
	f.pulled = append(f.pulled, ref)
	f.images[ref] = true
	return fakePullResponse{Reader: strings.NewReader(`{"status":"pulled"}`), err: f.pullErr}, nil
}

func (f *fakeAPI) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	wanted := options.Filters["label"]
	var out []container.Summary
	for _, item := range f.containers {
		if !matchLabels(item.config.Labels, wanted) {
			continue
		}
		state := container.StateExited
		if item.running {
			state = container.StateRunning
		}
		out = append(out, container.Summary{ID: item.id, Names: []string{item.name}, Labels: item.config.Labels, State: state})
	}
	return client.ContainerListResult{Items: out}, nil
}

func (f *fakeAPI) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.containers {
		if item.name == options.Name {
			return client.ContainerCreateResult{}, fmt.Errorf("container name %q is already in use: %w", options.Name, errdefs.ErrConflict)
		}
	}
	f.nextID++
	id := fmt.Sprintf("%08d-container-%d", f.nextID, len(f.containers))
	item := &fakeContainer{id: id, name: options.Name, config: options.Config, hostConfig: options.HostConfig}
	if len(options.HostConfig.PortBindings) > 0 && f.nextPort != nil {
		item.port = f.nextPort()
	}
	f.containers[id] = item
	f.created = append(f.created, options.Config)
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.containers[id]
	if !ok {
		return client.ContainerStartResult{}, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	item.running = true
	f.started = append(f.started, id)
	return client.ContainerStartResult{}, nil
}

func (f *fakeAPI) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.containers[id]
	if !ok {
		return client.ContainerStopResult{}, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	item.running = false
	f.stopped = append(f.stopped, id)
	return client.ContainerStopResult{}, nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[id]; !ok {
		return client.ContainerRemoveResult{}, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	delete(f.containers, id)
	f.removed = append(f.removed, id)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return client.ContainerInspectResult{}, f.inspectErr
	}
	item := f.byIDOrName(id)
	if item == nil {
		return client.ContainerInspectResult{}, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID:              item.id,
		Name:            item.name,
		State:           &container.State{Running: item.running, Status: container.StateRunning},
		Config:          item.config,
		NetworkSettings: &container.NetworkSettings{Ports: natPortMap(item)},
	}}, nil
}

func (f *fakeAPI) ContainerAttach(_ context.Context, id string, _ client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return client.ContainerAttachResult{}, f.attachErr
	}
	if f.byIDOrName(id) == nil {
		return client.ContainerAttachResult{}, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	conn := newFakeConn(f.attachPayload, f.attachStayOpen)
	f.attachConns[id] = conn
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "application/vnd.docker.raw-stream")}, nil
}

func (f *fakeAPI) ContainerLogs(_ context.Context, id string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.byIDOrName(id) == nil {
		return nil, fmt.Errorf("no such container %q: %w", id, errdefs.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(f.logs)), nil
}

func (f *fakeAPI) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// byIDOrName 复刻 daemon 的行为:inspect 既接受 ID 也接受容器名。
func (f *fakeAPI) byIDOrName(ref string) *fakeContainer {
	if item, ok := f.containers[ref]; ok {
		return item
	}
	for _, item := range f.containers {
		if item.name == ref {
			return item
		}
	}
	return nil
}

func (f *fakeAPI) containerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

func (f *fakeAPI) stdinFor(id string) *fakeConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attachConns[id]
}

func (f *fakeAPI) onlyContainer() *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.containers {
		return item
	}
	return nil
}

func (f *fakeAPI) setRunning(id string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item, ok := f.containers[id]; ok {
		item.running = running
	}
}

func (f *fakeAPI) addContainer(id, name string, labels map[string]string, running bool, port string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := &fakeContainer{
		id:         id,
		name:       name,
		config:     &container.Config{Labels: labels},
		hostConfig: &container.HostConfig{},
		running:    running,
		port:       port,
	}
	f.containers[id] = item
	return item
}

// matchLabels 复刻 daemon 的 label 过滤器语义:"k" 表示存在,"k=v" 表示等值,
// 多个值之间取或,多个条件之间取与。
func matchLabels(labels map[string]string, wanted map[string]bool) bool {
	for pair := range wanted {
		key, value, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			if _, exists := labels[key]; !exists {
				return false
			}
			continue
		}
		if labels[key] != value {
			return false
		}
	}
	return true
}

func natPortMap(item *fakeContainer) network.PortMap {
	if item.port == "" || item.config == nil {
		return nil
	}
	out := network.PortMap{}
	for port := range item.config.ExposedPorts {
		out[port] = []network.PortBinding{{HostIP: netip.MustParseAddr(defaultPublishHost), HostPort: item.port}}
	}
	return out
}

// fakePullResponse 实现 client.ImagePullResponse:读取返回进度,Wait 返回预置错误。
type fakePullResponse struct {
	*strings.Reader
	err error
}

func (r fakePullResponse) Close() error { return nil }

func (r fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

func (r fakePullResponse) Wait(context.Context) error { return r.err }

// fakeConn 是 attach 连接的内存替身:写入落缓冲(测试可断言按帧写入的 stdin),
// 读取返回预置负载,关闭是幂等的。
type fakeConn struct {
	mu      sync.Mutex
	reader  *bytes.Reader
	written bytes.Buffer
	closed  bool

	stayOpen  bool
	closedCh  chan struct{}
	closeOnce sync.Once
}

func newFakeConn(payload []byte, stayOpen bool) *fakeConn {
	conn := &fakeConn{reader: bytes.NewReader(payload), stayOpen: stayOpen}
	if stayOpen {
		conn.closedCh = make(chan struct{})
	}
	return conn
}

func (c *fakeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.reader.Len() > 0 {
		n, err := c.reader.Read(p)
		c.mu.Unlock()
		return n, err
	}
	closedCh := c.closedCh
	c.mu.Unlock()
	if closedCh == nil {
		return 0, io.EOF
	}
	<-closedCh
	return 0, io.EOF
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.written.Write(p)
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	closedCh := c.closedCh
	c.mu.Unlock()
	if closedCh != nil {
		c.closeOnce.Do(func() { close(closedCh) })
	}
	return nil
}

func (c *fakeConn) writeOutput() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.String()
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
