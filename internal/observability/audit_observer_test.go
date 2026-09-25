package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/auth"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

type memoryRepo struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *memoryRepo) Append(_ context.Context, event audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)

	return nil
}

func (r *memoryRepo) recorded() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]audit.Event(nil), r.events...)
}

func newObserver(t *testing.T) (*observability.AuditObserver, *memoryRepo) {
	t.Helper()

	repo := &memoryRepo{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return observability.NewAuditObserver(audit.NewRecorder(repo), logger), repo
}

// requestContext 模拟请求路径：身份由 withAuth 写入，request_id 由审计中间件写入。
func requestContext() context.Context {
	ctx := observability.WithRequestID(context.Background(), "req-1")

	return auth.WithPrincipal(ctx, auth.Principal{Name: "test-admin", Role: auth.RoleAdmin})
}

func TestToolCalledMapsObservationToEvent(t *testing.T) {
	tests := []struct {
		name        string
		observation gateway.ToolCallObservation
		wantOutcome audit.Outcome
		wantCode    string
		wantTarget  string
	}{
		{
			name: "resolved success",
			observation: gateway.ToolCallObservation{
				PublicName: "weather.get_forecast",
				Route: tool.Route{
					ServerID: "srv-1", ServerName: "weather",
					BackendName: "get_forecast", PublicName: "weather.get_forecast",
				},
				Resolved: true,
				Outcome:  audit.OutcomeSuccess,
				Duration: 12 * time.Millisecond,
			},
			wantOutcome: audit.OutcomeSuccess,
			wantTarget:  "weather.get_forecast",
		},
		{
			name: "unresolved name",
			observation: gateway.ToolCallObservation{
				PublicName: "weather.missing",
				Outcome:    audit.OutcomeError,
				ErrorCode:  audit.CodeNotFound,
			},
			wantOutcome: audit.OutcomeError,
			wantCode:    audit.CodeNotFound,
			wantTarget:  "weather.missing",
		},
		{
			name: "cancelled call",
			observation: gateway.ToolCallObservation{
				PublicName: "weather.get_forecast",
				Route:      tool.Route{ServerID: "srv-1", ServerName: "weather", BackendName: "get_forecast"},
				Resolved:   true,
				Outcome:    audit.OutcomeCancelled,
			},
			wantOutcome: audit.OutcomeCancelled,
			wantTarget:  "weather.get_forecast",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observer, repo := newObserver(t)
			observer.ToolCalled(requestContext(), tt.observation)

			events := repo.recorded()
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			event := events[0]
			if event.Type != audit.EventToolCalled {
				t.Errorf("type = %q, want tool.called", event.Type)
			}
			if event.Outcome != tt.wantOutcome || event.ErrorCode != tt.wantCode {
				t.Errorf("outcome/code = %q/%q, want %q/%q", event.Outcome, event.ErrorCode, tt.wantOutcome, tt.wantCode)
			}
			if event.Target != tt.wantTarget {
				t.Errorf("target = %q, want %q", event.Target, tt.wantTarget)
			}
			if event.ActorName != "test-admin" || event.ActorRole != "admin" || event.RequestID != "req-1" {
				t.Errorf("actor/request = %q/%q/%q, want test-admin/admin/req-1", event.ActorName, event.ActorRole, event.RequestID)
			}
			// 未解析成功时不得把请求里的名字当资产：Route 为零值，资产字段必须为空。
			if !tt.observation.Resolved && (event.AssetID != "" || event.AssetName != "") {
				t.Errorf("asset = %q/%q, want empty for an unresolved call", event.AssetID, event.AssetName)
			}
		})
	}
}

func TestRuntimeEventsMapToAudit(t *testing.T) {
	srv := server.Server{ID: "srv-1", Name: "weather"}
	instance := runtime.Instance{
		ID: "inst-1", Provider: string(server.RuntimeRemote), Phase: string(server.PhaseReady),
		ObservedRevision: 3, ExternalID: "http://backend.internal:9000/mcp",
	}
	previous := runtime.Instance{ID: "inst-0", Provider: string(server.RuntimeRemote)}

	tests := []struct {
		name     string
		fire     func(*observability.AuditObserver)
		wantType audit.EventType
		wantCode string
	}{
		{
			name:     "started",
			fire:     func(o *observability.AuditObserver) { o.RuntimeStarted(context.Background(), srv, instance) },
			wantType: audit.EventRuntimeStarted,
		},
		{
			name: "restarted",
			fire: func(o *observability.AuditObserver) {
				o.RuntimeRestarted(context.Background(), srv, previous, instance)
			},
			wantType: audit.EventRuntimeRestarted,
		},
		{
			name:     "stopped",
			fire:     func(o *observability.AuditObserver) { o.RuntimeStopped(context.Background(), srv, instance) },
			wantType: audit.EventRuntimeStopped,
		},
		{
			name: "failed",
			fire: func(o *observability.AuditObserver) {
				o.RuntimeFailed(context.Background(), srv, errors.Join(errors.New("boom"), runtime.ErrProviderNotFound))
			},
			wantType: audit.EventRuntimeFailed,
			wantCode: audit.CodeBackendUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observer, repo := newObserver(t)
			tt.fire(observer)

			events := repo.recorded()
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			event := events[0]
			if event.Type != tt.wantType {
				t.Errorf("type = %q, want %q", event.Type, tt.wantType)
			}
			if event.ErrorCode != tt.wantCode {
				t.Errorf("error code = %q, want %q", event.ErrorCode, tt.wantCode)
			}
			if event.AssetID != "srv-1" || event.AssetName != "weather" {
				t.Errorf("asset = %q/%q, want srv-1/weather", event.AssetID, event.AssetName)
			}
			// 后台巡检没有请求上下文：actor 与 request_id 必须留空而不是猜。
			if event.ActorName != "" || event.RequestID != "" {
				t.Errorf("actor/request = %q/%q, want empty for background events", event.ActorName, event.RequestID)
			}
			// ExternalID 可能是 endpoint / pid / 命令行语境，绝不进审计。
			if strings.Contains(string(event.Detail), instance.ExternalID) {
				t.Errorf("detail leaked ExternalID: %s", event.Detail)
			}
		})
	}
}

func TestRuntimeRestartedRecordsPreviousInstance(t *testing.T) {
	observer, repo := newObserver(t)
	srv := server.Server{ID: "srv-1", Name: "weather"}

	observer.RuntimeRestarted(context.Background(), srv,
		runtime.Instance{ID: "inst-0", Provider: string(server.RuntimeRemote)},
		runtime.Instance{ID: "inst-1", Provider: string(server.RuntimeRemote), ObservedRevision: 4},
	)

	var detail map[string]any
	if err := json.Unmarshal(repo.recorded()[0].Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["previous_runtime_id"] != "inst-0" {
		t.Errorf("previous_runtime_id = %v, want inst-0", detail["previous_runtime_id"])
	}
	if repo.recorded()[0].RuntimeID != "inst-1" {
		t.Errorf("runtime id = %q, want inst-1", repo.recorded()[0].RuntimeID)
	}
}

// 审计存储故障只写日志：观察者不得 panic，也不得改变调用链的行为。
func TestObserverSwallowsStorageErrors(t *testing.T) {
	logs := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	observer := observability.NewAuditObserver(audit.NewRecorder(failingRepo{}), logger)

	observer.ToolCalled(context.Background(), gateway.ToolCallObservation{
		PublicName: "weather.get_forecast", Outcome: audit.OutcomeSuccess,
	})
	if !strings.Contains(logs.String(), "record audit event") {
		t.Fatalf("storage error was not logged: %s", logs.String())
	}
}

type failingRepo struct{}

func (failingRepo) Append(context.Context, audit.Event) error {
	return errors.New("audit storage unavailable")
}
