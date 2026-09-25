package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/network"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

const (
	testImage   = "ghcr.io/example/mcp-server:1.0"
	testServer  = server.ID("11111111-1111-1111-1111-111111111111")
	containerID = "aaaaaaaaaaaa9999999999999999999999999999999999999999999999999999"
)

func testDockerSpec(transport server.Transport) server.DockerSpec {
	docker := server.DockerSpec{
		Image:       testImage,
		Command:     []string{"--stdio"},
		Env:         map[string]string{"TOKEN_FILE": "/run/secret/token"},
		Mounts:      []server.Mount{{Source: "/srv/data", Target: "/data", ReadOnly: true}},
		MemoryBytes: 256 << 20,
		CPUs:        1,
	}
	if transport == server.TransportStreamableHTTP {
		docker.Port = 8080
		docker.EndpointPath = "/mcp"
	}
	return docker
}

func testServerSpec(transport server.Transport) server.Server {
	return server.Server{
		ID:        testServer,
		Namespace: "default",
		Name:      "weather",
		Enabled:   true,
		Revision:  1,
		Spec: server.Spec{
			Transport: transport,
			Runtime: server.RuntimeSpec{
				Type:   server.RuntimeDocker,
				Docker: new(testDockerSpec(transport)),
			},
			Timeouts:     server.TimeoutSpec{Connect: 2 * time.Second, List: 10 * time.Second, Call: 60 * time.Second},
			Limits:       server.LimitSpec{MaxInFlight: 16},
			DesiredState: server.DesiredRunning,
		},
	}
}

func newTestProvider(t *testing.T, api api, opts ...func(*Options)) *Provider {
	t.Helper()
	options := Options{API: api}
	for _, apply := range opts {
		apply(&options)
	}
	provider, err := NewProvider(options)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return provider
}

type recordingPolicy struct {
	seen []server.DockerSpec
	err  error
}

func (p *recordingPolicy) CheckDocker(spec server.DockerSpec) error {
	p.seen = append(p.seen, spec)
	return p.err
}

func TestProviderEnsureCreatesStdioContainer(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	policy := &recordingPolicy{}
	provider := newTestProvider(t, api, func(opts *Options) { opts.Policy = policy })
	srv := testServerSpec(server.TransportStdio)

	instance, err := provider.Ensure(context.Background(), srv)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if got := api.containerCount(); got != 1 {
		t.Fatalf("container count = %d, want 1", got)
	}
	created := api.onlyContainer()
	if created.config.Image != testImage {
		t.Fatalf("image = %q", created.config.Image)
	}
	if diff := cmpStrings(created.config.Cmd, []string{"--stdio"}); diff != "" {
		t.Fatalf("cmd = %v", created.config.Cmd)
	}
	if diff := cmpStrings(created.config.Env, []string{"TOKEN_FILE=/run/secret/token"}); diff != "" {
		t.Fatalf("env = %v", created.config.Env)
	}
	if created.config.Labels[labelServerID] != string(testServer) {
		t.Fatalf("server label = %q", created.config.Labels[labelServerID])
	}
	if created.config.Labels[labelRevision] != "1" {
		t.Fatalf("revision label = %q", created.config.Labels[labelRevision])
	}
	if !created.config.OpenStdin || !created.config.AttachStdin {
		t.Fatalf("stdio container must keep stdin attached")
	}
	if created.config.Tty {
		t.Fatalf("stdio container must not allocate a tty")
	}
	if len(created.hostConfig.Mounts) != 1 || created.hostConfig.Mounts[0].Source != "/srv/data" || !created.hostConfig.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v", created.hostConfig.Mounts)
	}
	if created.hostConfig.Resources.Memory != 256<<20 || created.hostConfig.Resources.NanoCPUs != 1_000_000_000 {
		t.Fatalf("resources = %+v", created.hostConfig.Resources)
	}
	if len(api.started) != 1 || api.started[0] != created.id {
		t.Fatalf("started = %v", api.started)
	}
	if instance.Provider != string(server.RuntimeDocker) || instance.Phase != phaseRunning || instance.ObservedRevision != 1 {
		t.Fatalf("instance = %+v", instance)
	}
	if instance.ServerID != testServer || instance.ExternalID != created.id {
		t.Fatalf("instance = %+v", instance)
	}
	if !strings.HasPrefix(instance.ID, string(testServer)+"-docker-") {
		t.Fatalf("instance ID = %q", instance.ID)
	}
	if instance.Target.Streams == nil || instance.Target.Streams.Stdin == nil || instance.Target.Streams.Stdout == nil {
		t.Fatal("stdio target must expose streams")
	}
	if instance.Target.Transport != string(server.TransportStdio) || instance.Target.Command != testImage {
		t.Fatalf("target = %+v", instance.Target)
	}
	if len(policy.seen) != 1 {
		t.Fatalf("policy calls = %d, want 1", len(policy.seen))
	}

	// 同一 revision 的第二次 Ensure 必须复用,不再创建/启动。
	if _, err := provider.Ensure(context.Background(), srv); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := api.containerCount(); got != 1 {
		t.Fatalf("container count after reuse = %d, want 1", got)
	}
	if len(api.started) != 1 {
		t.Fatalf("started = %v, want single start", api.started)
	}
}

func TestProviderEnsureStreamsStdinToContainer(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	frame := []byte(`{"jsonrpc":"2.0","method":"initialize"}`)
	if _, err := instance.Target.Streams.Stdin.Write(frame); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	conn := api.stdinFor(api.onlyContainer().id)
	if got := conn.writeOutput(); got != string(frame) {
		t.Fatalf("container stdin = %q, want %q", got, frame)
	}
}

func TestProviderEnsureReusesRunningContainerAfterRestart(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	api.addContainer(containerID, "agentnexus-"+string(testServer), map[string]string{
		labelServerID: string(testServer),
		labelRevision: "1",
	}, true, "")
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if got := api.containerCount(); got != 1 {
		t.Fatalf("container count = %d, want 1", got)
	}
	if len(api.created) != 0 || len(api.started) != 0 || len(api.removed) != 0 {
		t.Fatalf("created=%d started=%d removed=%d, want 0/0/0", len(api.created), len(api.started), len(api.removed))
	}
	if instance.ExternalID != containerID {
		t.Fatalf("external ID = %q, want adopted container", instance.ExternalID)
	}
	if instance.Target.Streams == nil {
		t.Fatal("adopted container must be attachable")
	}
}

func TestProviderEnsureReplacesStaleContainers(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	api.addContainer("stale-running-1", "agentnexus-old", map[string]string{
		labelServerID: string(testServer),
		labelRevision: "0",
	}, true, "")
	api.addContainer("stale-stopped-2", "agentnexus-stopped", map[string]string{
		labelServerID: string(testServer),
		labelRevision: "1",
	}, false, "")
	provider := newTestProvider(t, api)

	if _, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if got := api.containerCount(); got != 1 {
		t.Fatalf("container count = %d, want 1", got)
	}
	if len(api.removed) != 2 {
		t.Fatalf("removed = %v, want both stale containers", api.removed)
	}
	if len(api.created) != 1 {
		t.Fatalf("created = %d, want 1", len(api.created))
	}
}

func TestProviderEnsureRecreatesContainerOnNewRevision(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider := newTestProvider(t, api)
	srv := testServerSpec(server.TransportStdio)

	first, err := provider.Ensure(context.Background(), srv)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	old := api.onlyContainer().id
	if err := provider.Stop(context.Background(), first); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	srv.Revision = 2
	second, err := provider.Ensure(context.Background(), srv)
	if err != nil {
		t.Fatalf("Ensure after revision bump: %v", err)
	}
	if second.ObservedRevision != 2 || second.ID == first.ID {
		t.Fatalf("second instance = %+v", second)
	}
	if api.onlyContainer().config.Labels[labelRevision] != "2" {
		t.Fatalf("revision label = %q", api.onlyContainer().config.Labels[labelRevision])
	}
	if got := api.onlyContainer().id; got == old {
		t.Fatal("revision bump must create a new container")
	}
}

func TestProviderEnsureCleansUnlabeledNameConflict(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	api.addContainer("unlabeled-1", "agentnexus-"+string(testServer), nil, true, "")
	provider := newTestProvider(t, api)

	if _, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(api.removed) != 1 || api.removed[0] != "unlabeled-1" {
		t.Fatalf("removed = %v, want the unlabeled leftover", api.removed)
	}
	if got := api.containerCount(); got != 1 {
		t.Fatalf("container count = %d, want 1", got)
	}
}

func TestProviderEnsurePullsMissingImage(t *testing.T) {
	api := newFakeAPI()
	provider := newTestProvider(t, api)

	if _, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(api.pulled) != 1 || api.pulled[0] != testImage {
		t.Fatalf("pulled = %v", api.pulled)
	}

	// 镜像已存在时不再拉取。
	api.images[testImage] = true
	api.pulled = nil
	if _, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio)); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if len(api.pulled) != 0 {
		t.Fatalf("pulled = %v, want none", api.pulled)
	}
}

func TestProviderEnsureRejectsViolatingSpec(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	policy := &recordingPolicy{err: errors.New("docker mount source \"/srv/data\" is not allowed")}
	provider := newTestProvider(t, api, func(opts *Options) { opts.Policy = policy })

	_, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("Ensure error = %v, want policy rejection", err)
	}
	if api.containerCount() != 0 {
		t.Fatal("policy rejection must not create a container")
	}

	// spec 形状错误(容器端口出现在 stdio)同样在 Provider 侧拦下。
	broken := testServerSpec(server.TransportStdio)
	broken.Spec.Runtime.Docker.Port = 8080
	if _, err := provider.Ensure(context.Background(), broken); err == nil {
		t.Fatal("Ensure must reject an invalid docker spec")
	}
}

func TestProviderEnsureStreamableHTTPPublishesPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	api := newFakeAPI()
	api.images[testImage] = true
	api.nextPort = func() string { return fmt.Sprint(port) }
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStreamableHTTP))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	created := api.onlyContainer()
	if _, ok := created.config.ExposedPorts[network.MustParsePort("8080/tcp")]; !ok {
		t.Fatalf("exposed ports = %v", created.config.ExposedPorts)
	}
	bindings := created.hostConfig.PortBindings[network.MustParsePort("8080/tcp")]
	if len(bindings) != 1 || bindings[0].HostIP != netip.MustParseAddr(defaultPublishHost) {
		t.Fatalf("port bindings = %+v", bindings)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d/mcp", port); instance.Target.URL != want {
		t.Fatalf("target URL = %q, want %q", instance.Target.URL, want)
	}
	if instance.Target.Streams != nil {
		t.Fatal("streamable_http target must not expose streams")
	}
}

func TestProviderEnsureStreamableHTTPFailsWhenPortNeverListens(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	api := newFakeAPI()
	api.images[testImage] = true
	api.nextPort = func() string { return fmt.Sprint(port) }
	provider := newTestProvider(t, api)

	srv := testServerSpec(server.TransportStreamableHTTP)
	srv.Spec.Timeouts.Connect = 200 * time.Millisecond
	start := time.Now()
	_, err = provider.Ensure(context.Background(), srv)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Ensure error = %v, want readiness failure", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Ensure waited %s, want bounded by connect timeout", elapsed)
	}
}

func TestProviderInspectReportsUnavailable(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	inspected, err := provider.Inspect(context.Background(), instance)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspected.ID != instance.ID || inspected.ObservedRevision != instance.ObservedRevision {
		t.Fatalf("inspect = %+v", inspected)
	}

	if _, err := provider.Inspect(context.Background(), runtime.Instance{ID: "unknown"}); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("unknown instance error = %v, want ErrInstanceUnavailable", err)
	}
	if _, err := provider.Inspect(context.Background(), runtime.Instance{ID: instance.ID, ServerID: "other"}); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("foreign instance error = %v, want ErrInstanceUnavailable", err)
	}

	// 容器退出:Inspect 必须报不可用,交由 ProviderManager 重建。
	api.setRunning(instance.ExternalID, false)
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("exited container error = %v, want ErrInstanceUnavailable", err)
	}
	api.setRunning(instance.ExternalID, true)

	// revision 标签被外部改动:同样视为不可用。
	api.mu.Lock()
	api.containers[instance.ExternalID].config.Labels[labelRevision] = "9"
	api.mu.Unlock()
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("stale revision error = %v, want ErrInstanceUnavailable", err)
	}
}

func TestProviderInspectReportsUnavailableAfterSessionEnds(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := instance.Target.Streams.Stdout.Close(); err != nil {
		t.Fatalf("close session stdout: %v", err)
	}
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("session-end error = %v, want ErrInstanceUnavailable", err)
	}
	if !api.stdinFor(instance.ExternalID).isClosed() {
		t.Fatal("ending a session must release the attach connection")
	}

	// 会话结束后重新 Ensure:容器还在跑,必须重新 attach 而不是重建。
	again, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure after session end: %v", err)
	}
	if again.ExternalID != instance.ExternalID {
		t.Fatalf("re-Ensure adopted %q, want %q", again.ExternalID, instance.ExternalID)
	}
	if len(api.created) != 1 || len(api.started) != 1 {
		t.Fatalf("created=%d started=%d, want 1/1", len(api.created), len(api.started))
	}
	if _, err := provider.Inspect(context.Background(), again); err != nil {
		t.Fatalf("Inspect after re-attach: %v", err)
	}
}

func TestProviderStopIsIdempotent(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	conn := api.stdinFor(instance.ExternalID)

	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.stopped) != 1 || len(api.removed) != 1 {
		t.Fatalf("stopped=%v removed=%v", api.stopped, api.removed)
	}
	if !conn.isClosed() {
		t.Fatal("Stop must release the attach connection")
	}
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("Inspect after Stop = %v", err)
	}
	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if len(api.stopped) != 1 {
		t.Fatalf("stopped = %v, want single stop", api.stopped)
	}
}

func TestProviderCloseReclaimsEveryContainer(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	provider, err := NewProvider(Options{API: api})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	first, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	second := testServerSpec(server.TransportStdio)
	second.ID = "22222222-2222-2222-2222-222222222222"
	second.Name = "calendar"
	if _, err := provider.Ensure(context.Background(), second); err != nil {
		t.Fatalf("Ensure second: %v", err)
	}
	if api.containerCount() != 2 {
		t.Fatalf("container count = %d, want 2", api.containerCount())
	}

	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if api.containerCount() != 0 {
		t.Fatalf("container count after Close = %d, want 0", api.containerCount())
	}
	if !api.closed {
		t.Fatal("Close must close the engine client")
	}
	if _, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio)); !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("Ensure after Close = %v, want ErrProviderClosed", err)
	}
	if _, err := provider.Inspect(context.Background(), first); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("Inspect after Close = %v, want ErrInstanceUnavailable", err)
	}
	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestProviderLogsDemultiplexesAndTruncates(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	api.logs = frameLogs(t, "hello", "oops")
	provider := newTestProvider(t, api, func(opts *Options) { opts.LogBuffer = 5 })

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	reader, err := provider.Logs(context.Background(), instance, runtime.LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	reader.Close()
	if want := tail("hellooops", 5); string(data) != want {
		t.Fatalf("logs = %q, want %q", data, want)
	}

	if _, err := provider.Logs(context.Background(), runtime.Instance{ID: "unknown"}, runtime.LogOptions{}); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("unknown instance logs error = %v", err)
	}
}

func TestProviderLogsFollowStreams(t *testing.T) {
	api := newFakeAPI()
	api.images[testImage] = true
	api.logs = frameLogs(t, "streaming", "")
	provider := newTestProvider(t, api)

	instance, err := provider.Ensure(context.Background(), testServerSpec(server.TransportStdio))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	reader, err := provider.Logs(context.Background(), instance, runtime.LogOptions{Follow: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read followed logs: %v", err)
	}
	reader.Close()
	if string(data) != "streaming" {
		t.Fatalf("followed logs = %q", data)
	}
}

// frameLogs 按 Docker 复用流线格式手工生成 stdout/stderr 帧:
// 8 字节头(stream,0,0,0,大端长度)+ 负载。手写而非用库内 writer,
// 这样即使上游改动写入侧实现,解复用的线格式仍被独立钉住。
func frameLogs(t *testing.T, stdout, stderr string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	for _, frame := range []struct {
		stream  stdcopy.StdType
		payload string
	}{{stdcopy.Stdout, stdout}, {stdcopy.Stderr, stderr}} {
		if frame.payload == "" {
			continue
		}
		header := []byte{byte(frame.stream), 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[4:], uint32(len(frame.payload)))
		buffer.Write(header)
		buffer.WriteString(frame.payload)
	}
	return buffer.Bytes()
}

func tail(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func cmpStrings(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("%v != %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("%v != %v", got, want)
		}
	}
	return ""
}
