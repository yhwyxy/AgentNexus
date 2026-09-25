package process

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

func stdioServer(name string, command string, args ...string) server.Server {
	return server.Server{
		ID:        server.ID(name),
		Namespace: "default",
		Name:      name,
		Revision:  1,
		Spec: server.Spec{
			Runtime: server.RuntimeSpec{
				Type:    server.RuntimeProcess,
				Process: &server.ProcessSpec{Command: command, Args: args},
			},
		},
	}
}

// echoLoop 把 stdin 的每一行原样写回 stdout,用于验证标准流是活的。
func echoLoop() []string {
	return []string{"-c", `while IFS= read -r line; do printf 'echo:%s\n' "$line"; done`}
}

func mustEnsure(t *testing.T, provider *Provider, srv server.Server) runtime.Instance {
	t.Helper()
	instance, err := provider.Ensure(context.Background(), srv)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if instance.Target.Streams == nil {
		t.Fatal("instance has no streams")
	}
	return instance
}

func readLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		done <- result{line: line, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("read stdout: %v", got.err)
		}
		return got.line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading child stdout")
		return ""
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

func TestEnsureExposesLiveStreamsAndInspects(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	instance := mustEnsure(t, provider, stdioServer("echo", "/bin/sh", echoLoop()...))

	if _, err := instance.Target.Streams.Stdin.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if line := readLine(t, bufio.NewReader(instance.Target.Streams.Stdout)); line != "echo:hello\n" {
		t.Fatalf("stdout line = %q", line)
	}

	inspected, err := provider.Inspect(context.Background(), instance)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.ID != instance.ID || inspected.Phase != phaseRunning {
		t.Fatalf("inspected instance = %+v", inspected)
	}
	if inspected.ObservedRevision != 1 || inspected.Provider != string(server.RuntimeProcess) {
		t.Fatalf("inspected instance lost metadata: %+v", inspected)
	}
	if inspected.Target.Streams != instance.Target.Streams {
		t.Fatal("inspect must hand out the live streams")
	}
}

func TestInspectFailsWhenSessionStreamsClosed(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	instance := mustEnsure(t, provider, stdioServer("echo", "/bin/sh", echoLoop()...))

	if err := instance.Target.Streams.Stdout.Close(); err != nil {
		t.Fatalf("close stdout wrapper: %v", err)
	}
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("inspect after session close = %v, want ErrInstanceUnavailable", err)
	}
	// 会话关闭不等于进程已死:这里必须仍能 Stop 掉(由 manager 的 reap 路径触发)。
	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("stop after session close: %v", err)
	}
	if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("inspect after stop = %v", err)
	}
}

func TestInspectFailsAfterChildExit(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	instance := mustEnsure(t, provider, stdioServer("quick", "/bin/sh", "-c", "exit 3"))

	if !waitForCondition(t, 5*time.Second, func() bool {
		_, err := provider.Inspect(context.Background(), instance)
		return errors.Is(err, ErrInstanceUnavailable)
	}) {
		t.Fatal("inspect never noticed the exited child")
	}
	if _, err := provider.Inspect(context.Background(), instance); !strings.Contains(err.Error(), "exited") {
		t.Fatalf("inspect error = %v", err)
	}
}

func TestLogsCaptureStderrAndFollowIsUnsupported(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	srv := stdioServer("logs", "/bin/sh", "-c", `printf 'boom\n' >&2; sleep 30`)
	srv.Spec.Runtime.Process.Env = map[string]string{"AGENTNEXUS_FAKE_ENV": "from-spec"}
	instance := mustEnsure(t, provider, srv)

	var logs string
	if !waitForCondition(t, 5*time.Second, func() bool {
		reader, err := provider.Logs(context.Background(), instance, runtime.LogOptions{})
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read logs: %v", err)
		}
		logs = string(content)
		return strings.Contains(logs, "boom")
	}) {
		t.Fatalf("stderr snapshot = %q", logs)
	}
	if _, err := provider.Logs(context.Background(), instance, runtime.LogOptions{Follow: true}); err == nil {
		t.Fatal("follow mode must be rejected until implemented")
	}
}

func TestEnsureAppliesEnvAndWorkingDir(t *testing.T) {
	dir := t.TempDir()
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	srv := stdioServer("env", "/bin/sh", "-c", `printf 'env:%s\n' "$AGENTNEXUS_FAKE_ENV" >&2; printf x > created.txt; sleep 30`)
	srv.Spec.Runtime.Process.Env = map[string]string{"AGENTNEXUS_FAKE_ENV": "from-spec"}
	srv.Spec.Runtime.Process.WorkingDir = dir
	instance := mustEnsure(t, provider, srv)

	// Logs 返回的是快照,因此每次等待都重新取一份。
	if !waitForCondition(t, 5*time.Second, func() bool {
		reader, err := provider.Logs(context.Background(), instance, runtime.LogOptions{})
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read logs: %v", err)
		}
		return strings.Contains(string(content), "env:from-spec")
	}) {
		t.Fatal("explicit env var was not visible to the child")
	}
	if _, err := os.Stat(filepath.Join(dir, "created.txt")); err != nil {
		t.Fatalf("working directory was not applied: %v", err)
	}
}

func TestStopIsIdempotentAndUnknownInstanceIsNotFound(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	instance := mustEnsure(t, provider, stdioServer("stop", "/bin/sh", echoLoop()...))

	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := provider.Stop(context.Background(), instance); err != nil {
		t.Fatalf("second stop must be idempotent: %v", err)
	}
	if err := provider.Stop(context.Background(), runtime.Instance{ID: "unknown"}); err != nil {
		t.Fatalf("stop of unknown instance = %v, want nil", err)
	}
}

func TestCloseStopsEveryInstanceAndRejectsEnsure(t *testing.T) {
	provider := NewProvider(context.Background())
	first := mustEnsure(t, provider, stdioServer("first", "/bin/sh", echoLoop()...))
	second := mustEnsure(t, provider, stdioServer("second", "/bin/sh", echoLoop()...))

	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, instance := range []runtime.Instance{first, second} {
		if _, err := provider.Inspect(context.Background(), instance); !errors.Is(err, ErrInstanceUnavailable) {
			t.Fatalf("inspect %s after close = %v", instance.ID, err)
		}
	}
	if _, err := provider.Ensure(context.Background(), stdioServer("third", "/bin/sh", echoLoop()...)); !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("ensure after close = %v, want ErrProviderClosed", err)
	}
	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("second close must be idempotent: %v", err)
	}
}

// TestConcurrentSessionCloseAndInspect 覆盖"会话关闭"与"巡检/回收"并发的竞态,
// 配合 -race 使用:关闭标准流的瞬间恰好有 Inspect、Stop、Ensure 在跑。
func TestConcurrentSessionCloseAndInspect(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	srv := stdioServer("race", "/bin/sh", echoLoop()...)

	for range 20 {
		instance := mustEnsure(t, provider, srv)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = instance.Target.Streams.Stdout.Close()
			_ = instance.Target.Streams.Stdin.Close()
		}()
		go func() {
			defer wg.Done()
			_, _ = provider.Inspect(context.Background(), instance)
		}()
		go func() {
			defer wg.Done()
			// 模拟 manager 的 reap 路径:先 Inspect,再 Stop。
			if _, err := provider.Inspect(context.Background(), instance); err != nil {
				_ = provider.Stop(context.Background(), instance)
			}
		}()
		wg.Wait()
		_ = provider.Stop(context.Background(), instance)
	}
}

func TestEnsureRejectsNonProcessRuntime(t *testing.T) {
	provider := NewProvider(context.Background())
	t.Cleanup(func() { _ = provider.Close(context.Background()) })

	remote := stdioServer("remote", "/bin/sh")
	remote.Spec.Runtime = server.RuntimeSpec{Type: server.RuntimeRemote, Remote: &server.RemoteSpec{Endpoint: "http://example/mcp"}}
	if _, err := provider.Ensure(context.Background(), remote); err == nil {
		t.Fatal("process provider accepted a remote runtime")
	}
	relative := stdioServer("relative", "sh")
	if _, err := provider.Ensure(context.Background(), relative); err == nil {
		t.Fatal("process provider accepted a relative command")
	}
}

func TestMergedEnvOverridesAndSorts(t *testing.T) {
	t.Setenv("AGENTNEXUS_MERGE_PROBE", "parent")
	env := mergedEnv(map[string]string{"AGENTNEXUS_MERGE_PROBE": "spec", "AGENTNEXUS_MERGE_EXTRA": "1"})

	var probe, extra int
	for _, entry := range env {
		switch entry {
		case "AGENTNEXUS_MERGE_PROBE=spec":
			probe++
		case "AGENTNEXUS_MERGE_PROBE=parent":
			t.Fatal("parent value must be overridden instead of duplicated")
		case "AGENTNEXUS_MERGE_EXTRA=1":
			extra++
		}
	}
	if probe != 1 || extra != 1 {
		t.Fatalf("merged env probe=%d extra=%d", probe, extra)
	}
	for index := 1; index < len(env); index++ {
		previous, _, _ := strings.Cut(env[index-1], "=")
		current, _, _ := strings.Cut(env[index], "=")
		if previous >= current {
			t.Fatalf("env is not sorted and unique: %q before %q", previous, current)
		}
	}
}

func TestBoundedBufferDropsOldestBytes(t *testing.T) {
	buffer := newBoundedBuffer(8)
	if _, err := buffer.Write([]byte("abcdefghij")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(buffer.snapshot()); got != "cdefghij" {
		t.Fatalf("snapshot = %q", got)
	}
}
