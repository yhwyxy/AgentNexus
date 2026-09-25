// audit 包是审计数据的唯一入口：这里的校验与脱敏规则决定了"什么能落库"。
// 因此测试覆盖三件事：非法事件被拒（宁缺勿滥）、Detail 的 deny-list 真的删值、
// 以及错误码映射不会把未分类错误伪装成某个具体原因。
package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

type recordingRepository struct {
	appended []audit.Event
	err      error
}

func (r *recordingRepository) Append(_ context.Context, event audit.Event) error {
	if r.err != nil {
		return r.err
	}
	r.appended = append(r.appended, event)

	return nil
}

func TestRecorderAssignsIDAndUTCClock(t *testing.T) {
	repo := &recordingRepository{}
	// 非 UTC 时区的时钟:落库前必须换算,否则跨时区的行无法按字典序排序。
	zone := time.FixedZone("CST", 8*3600)
	recorder := audit.NewRecorder(repo).
		WithIDGenerator(func() string { return "evt-1" }).
		WithClock(func() time.Time { return time.Date(2026, 9, 25, 13, 0, 0, 0, zone) })

	if err := recorder.Record(context.Background(), audit.Input{
		Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, Target: "math.add",
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	if len(repo.appended) != 1 {
		t.Fatalf("appended %d events, want 1", len(repo.appended))
	}
	event := repo.appended[0]
	if event.ID != "evt-1" {
		t.Fatalf("ID = %q, want evt-1", event.ID)
	}
	if event.OccurredAt.Location() != time.UTC || !event.OccurredAt.Equal(time.Date(2026, 9, 25, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("OccurredAt = %s, want 2026-09-25T05:00:00Z", event.OccurredAt)
	}
	if string(event.Detail) != "{}" {
		t.Fatalf("Detail = %s, want {}", event.Detail)
	}
}

func TestRecorderRejectsInvalidEvents(t *testing.T) {
	tests := []struct {
		name  string
		input audit.Input
	}{
		{"unknown type", audit.Input{Type: "tool.mystery", Outcome: audit.OutcomeSuccess}},
		{"empty type", audit.Input{Outcome: audit.OutcomeSuccess}},
		{"unknown outcome", audit.Input{Type: audit.EventToolCalled, Outcome: "ok"}},
		{"empty outcome", audit.Input{Type: audit.EventToolCalled}},
		{"negative duration", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, DurationMs: -1}},
		{"detail not an object", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, Detail: json.RawMessage(`[1,2]`)}},
		{"detail scalar", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, Detail: json.RawMessage(`"x"`)}},
		{"detail malformed", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, Detail: json.RawMessage(`{`)}},
		{"detail trailing value", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess, Detail: json.RawMessage(`{} {}`)}},
		{"detail too large", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess,
			Detail: json.RawMessage(`{"blob":"` + strings.Repeat("a", 5<<10) + `"}`)}},
		{"detail too deep", audit.Input{Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess,
			Detail: json.RawMessage(nestedDetail(20))}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &recordingRepository{}
			err := audit.NewRecorder(repo).Record(context.Background(), test.input)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Fatalf("err = %v, want ErrInvalidEvent", err)
			}
			if len(repo.appended) != 0 {
				t.Fatalf("invalid event was persisted: %#v", repo.appended)
			}
		})
	}
}

// nestedDetail 构造 depth 层的嵌套对象:验证递归深度上限。
func nestedDetail(depth int) string {
	return strings.Repeat(`{"x":`, depth) + `1` + strings.Repeat(`}`, depth)
}

func TestRecorderRedactsSensitiveDetailKeys(t *testing.T) {
	repo := &recordingRepository{}
	recorder := audit.NewRecorder(repo)
	err := recorder.Record(context.Background(), audit.Input{
		Type: audit.EventServerRegistered, Outcome: audit.OutcomeSuccess,
		Detail: json.RawMessage(`{
			"revision": 2,
			"endpoint": "http://weather-mcp:8080/mcp",
			"Authorization": "Bearer secret",
			"api-key": "k",
			"nested": {"Env_Vars": {"TOKEN": "t"}, "path": "/host/dir", "keep": "yes"},
			"commandLine": ["/usr/bin/mcp"],
			"list": [{"url": "http://x"}, {"keep": 1}]
		}`),
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(repo.appended[0].Detail, &decoded); err != nil {
		t.Fatalf("decode stored detail: %v", err)
	}
	if _, exists := decoded["endpoint"]; exists {
		t.Fatalf("endpoint survived redaction: %#v", decoded)
	}
	if _, exists := decoded["Authorization"]; exists {
		t.Fatalf("Authorization survived redaction: %#v", decoded)
	}
	if _, exists := decoded["api-key"]; exists {
		t.Fatalf("api-key survived redaction (normalization must strip dashes): %#v", decoded)
	}
	if got := decoded["revision"]; got != float64(2) {
		t.Fatalf("revision = %v, want 2", got)
	}
	nested, _ := decoded["nested"].(map[string]any)
	if _, exists := nested["Env_Vars"]; exists {
		t.Fatalf("nested Env_Vars survived redaction: %#v", nested)
	}
	if _, exists := nested["path"]; exists {
		t.Fatalf("nested path survived redaction: %#v", nested)
	}
	if nested["keep"] != "yes" {
		t.Fatalf("nested keep = %v, want yes", nested["keep"])
	}
	list, _ := decoded["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2", len(list))
	}
	first, _ := list[0].(map[string]any)
	if len(first) != 0 {
		t.Fatalf("url inside an array survived redaction: %#v", first)
	}
	second, _ := list[1].(map[string]any)
	if second["keep"] != float64(1) {
		t.Fatalf("list keep = %v, want 1", second["keep"])
	}
}

func TestRecorderPropagatesStorageFailure(t *testing.T) {
	storageErr := errors.New("audit storage unavailable")
	err := audit.NewRecorder(&recordingRepository{err: storageErr}).Record(context.Background(), audit.Input{
		Type: audit.EventToolCalled, Outcome: audit.OutcomeSuccess,
	})
	if !errors.Is(err, storageErr) {
		t.Fatalf("err = %v, want the storage error to be wrapped", err)
	}
	if !strings.Contains(err.Error(), string(audit.EventToolCalled)) {
		t.Fatalf("err = %v, want the event type in the message", err)
	}
}

func TestDetailFallsBackToEmptyObject(t *testing.T) {
	// 无法编码的值不能让整条事件丢失:详情退化为空对象,其余字段照常落库。
	if got := string(audit.Detail(map[string]any{"bad": make(chan int)})); got != "{}" {
		t.Fatalf("Detail = %s, want {}", got)
	}
	if got := string(audit.Detail(map[string]any{"revision": 2})); got != `{"revision":2}` {
		t.Fatalf("Detail = %s, want the encoded object", got)
	}
}

func TestErrorCodeMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, ""},
		{"canceled wrapped", errors.Join(context.Canceled, errors.New("cleanup")), ""},
		{"deadline", context.DeadlineExceeded, audit.CodeDeadlineExceeded},
		{"invalid server", server.ErrInvalid, audit.CodeInvalidArgument},
		{"invalid tool", tool.ErrInvalidTool, audit.CodeInvalidArgument},
		{"not found", server.ErrNotFound, audit.CodeNotFound},
		{"tool not found", tool.ErrToolNotFound, audit.CodeNotFound},
		{"snapshot not found", tool.ErrSnapshotNotFound, audit.CodeNotFound},
		{"already exists", server.ErrAlreadyExists, audit.CodeConflict},
		{"conflict", server.ErrConflict, audit.CodeConflict},
		{"catalog conflict", tool.ErrCatalogConflict, audit.CodeConflict},
		{"not runnable", server.ErrNotRunnable, audit.CodeConflict},
		{"disabled", runtime.ErrServerDisabled, audit.CodeServerUnavailable},
		{"stopped", runtime.ErrDesiredStateStopped, audit.CodeServerUnavailable},
		{"manager closed", runtime.ErrManagerClosed, audit.CodeServerUnavailable},
		{"invalid instance", runtime.ErrInvalidInstance, audit.CodeRuntimeFailed},
		{"provider not found", runtime.ErrProviderNotFound, audit.CodeBackendUnavailable},
		{"unknown", errors.New("boom"), audit.CodeInternal},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := audit.ErrorCode(test.err); got != test.want {
				t.Fatalf("ErrorCode = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOutcomeOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want audit.Outcome
	}{
		{"nil", nil, audit.OutcomeSuccess},
		{"canceled", context.Canceled, audit.OutcomeCancelled},
		{"canceled wrapped", errors.Join(errors.New("dial"), context.Canceled), audit.OutcomeCancelled},
		{"other", errors.New("boom"), audit.OutcomeError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := audit.OutcomeOf(test.err); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("OutcomeOf = %q, want %q", got, test.want)
			}
		})
	}
}
