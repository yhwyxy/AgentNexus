// 审计适配器：把领域事实（工具调用、运行态变化）翻译成审计事件。
//
// 这是唯一同时认识 audit 与领域 observer 接口的地方：领域包只发射事实，
// 由本适配器补齐「谁、在哪个请求里」（从 ctx 读取）后落库。
package observability

import (
	"context"
	"log/slog"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// AuditObserver 实现 gateway.CallObserver 与 runtime.Observer。
//
// 它不做任何存储决策：写失败只记 error 日志（审计是外部事实记录，
// 不得反向影响 Runtime 协调或工具调用）。
type AuditObserver struct {
	recorder *audit.Recorder
	logger   *slog.Logger
}

var (
	_ gateway.CallObserver = (*AuditObserver)(nil)
	_ runtime.Observer     = (*AuditObserver)(nil)
)

func NewAuditObserver(recorder *audit.Recorder, logger *slog.Logger) *AuditObserver {
	if logger == nil {
		logger = slog.Default()
	}

	return &AuditObserver{recorder: recorder, logger: logger}
}

func (o *AuditObserver) ToolCalled(ctx context.Context, obs gateway.ToolCallObservation) {
	o.record(ctx, audit.Input{
		Type:       audit.EventToolCalled,
		Outcome:    obs.Outcome,
		AssetID:    string(obs.Route.ServerID),
		AssetName:  obs.Route.ServerName,
		Target:     obs.PublicName,
		ErrorCode:  obs.ErrorCode,
		DurationMs: obs.Duration.Milliseconds(),
		Detail: audit.Detail(map[string]any{
			"backend_name": obs.Route.BackendName,
			"resolved":     obs.Resolved,
			"is_error":     obs.IsError,
		}),
	})
}

func (o *AuditObserver) RuntimeStarted(ctx context.Context, srv server.Server, instance runtime.Instance) {
	o.runtimeEvent(ctx, audit.EventRuntimeStarted, srv, instance, audit.OutcomeSuccess, "", "")
}

func (o *AuditObserver) RuntimeRestarted(ctx context.Context, srv server.Server, previous, current runtime.Instance) {
	o.runtimeEvent(ctx, audit.EventRuntimeRestarted, srv, current, audit.OutcomeSuccess, previous.ID, "")
}

func (o *AuditObserver) RuntimeStopped(ctx context.Context, srv server.Server, instance runtime.Instance) {
	o.runtimeEvent(ctx, audit.EventRuntimeStopped, srv, instance, audit.OutcomeSuccess, "", "")
}

func (o *AuditObserver) RuntimeFailed(ctx context.Context, srv server.Server, cause error) {
	// 只记错误码，不记错误文本：Provider 的错误里可能带 endpoint 或命令行语境。
	o.runtimeEvent(ctx, audit.EventRuntimeFailed, srv, runtime.Instance{}, audit.OutcomeError, "", audit.ErrorCode(cause))
	o.logger.Warn("runtime coordination failed", "server_id", srv.ID, "error_code", audit.ErrorCode(cause))
}

// runtimeEvent 上报运行态事件。Detail 只放进程内标识与非敏感状态字段；
// 绝不写 Instance.ExternalID（它可能是 endpoint、pid 或命令语境）。
func (o *AuditObserver) runtimeEvent(
	ctx context.Context,
	eventType audit.EventType,
	srv server.Server,
	instance runtime.Instance,
	outcome audit.Outcome,
	previousRuntimeID string,
	errorCode string,
) {
	detail := map[string]any{
		"provider":          instance.Provider,
		"observed_revision": instance.ObservedRevision,
		"phase":             instance.Phase,
	}
	if previousRuntimeID != "" {
		detail["previous_runtime_id"] = previousRuntimeID
	}
	o.record(ctx, audit.Input{
		Type:      eventType,
		Outcome:   outcome,
		AssetID:   string(srv.ID),
		AssetName: srv.Name,
		Target:    instance.ID,
		RuntimeID: instance.ID,
		ErrorCode: errorCode,
		Detail:    audit.Detail(detail),
	})
}

// record 补齐调用者与请求 ID 后落库。
func (o *AuditObserver) record(ctx context.Context, in audit.Input) {
	in = EnrichAuditInput(ctx, in)
	if err := o.recorder.Record(ctx, in); err != nil {
		o.logger.Error("record audit event", "event_type", in.Type, "error", err)
	}
}
