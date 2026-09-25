package observability_test

import (
	"context"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

// recordingObserver 记录收到的调用顺序，用于断言扇出的分发语义。
type recordingObserver struct {
	name  string
	calls *[]string
}

func (o recordingObserver) note(what string) {
	*o.calls = append(*o.calls, o.name+":"+what)
}

func (o recordingObserver) ToolCalled(context.Context, gateway.ToolCallObservation) { o.note("tool") }

func (o recordingObserver) RuntimeStarted(_ context.Context, srv server.Server, _ runtime.Instance) {
	o.note("started:" + srv.Name)
}

func (o recordingObserver) RuntimeRestarted(_ context.Context, srv server.Server, _, _ runtime.Instance) {
	o.note("restarted:" + srv.Name)
}

func (o recordingObserver) RuntimeStopped(_ context.Context, srv server.Server, _ runtime.Instance) {
	o.note("stopped:" + srv.Name)
}

func (o recordingObserver) RuntimeFailed(_ context.Context, srv server.Server, _ error) {
	o.note("failed:" + srv.Name)
}

// TestMultiObserversFanOutInOrder 断言扇出按声明顺序分发，并跳过 nil
// （测试或某条装配路径没有审计/metrics 时不应 panic）。
func TestMultiObserversFanOutInOrder(t *testing.T) {
	var calls []string
	first := recordingObserver{name: "audit", calls: &calls}
	second := recordingObserver{name: "metrics", calls: &calls}

	runtimes := observability.MultiRuntimeObserver{nil, first, nil, second}
	srv := server.Server{Name: "weather"}
	instance := runtime.Instance{ID: "inst-1", ServerID: srv.ID}

	runtimes.RuntimeStarted(context.Background(), srv, instance)
	runtimes.RuntimeRestarted(context.Background(), srv, instance, instance)
	runtimes.RuntimeStopped(context.Background(), srv, instance)
	runtimes.RuntimeFailed(context.Background(), srv, context.Canceled)

	callFanout := observability.MultiCallObserver{nil, first, second}
	callFanout.ToolCalled(context.Background(), gateway.ToolCallObservation{
		PublicName: "weather.demo.echo",
		Route:      tool.Route{ServerName: "weather"},
		Outcome:    audit.OutcomeSuccess,
	})

	want := []string{
		"audit:started:weather", "metrics:started:weather",
		"audit:restarted:weather", "metrics:restarted:weather",
		"audit:stopped:weather", "metrics:stopped:weather",
		"audit:failed:weather", "metrics:failed:weather",
		"audit:tool", "metrics:tool",
	}
	if len(calls) != len(want) {
		t.Fatalf("fan-out calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}

// TestEmptyFanOutIsNoOp 断言空扇出不 panic：Options 可选的中间装配形态。
func TestEmptyFanOutIsNoOp(t *testing.T) {
	var runtimes observability.MultiRuntimeObserver
	var calls observability.MultiCallObserver
	srv := server.Server{Name: "weather"}

	runtimes.RuntimeStarted(context.Background(), srv, runtime.Instance{})
	runtimes.RuntimeFailed(context.Background(), srv, context.Canceled)
	calls.ToolCalled(context.Background(), gateway.ToolCallObservation{})
}
