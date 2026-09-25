// MCP Server 管理端点：注册与查询。领域错误到 HTTP 状态码的映射只在这里。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

// ServerRegistry 是 HTTP 层对应用服务的最小依赖（消费者定义接口）。
type ServerRegistry interface {
	Register(ctx context.Context, in server.RegisterInput) (server.Server, error)
	Get(ctx context.Context, id server.ID) (server.Server, error)
	List(ctx context.Context) ([]server.Server, error)
	Update(ctx context.Context, id server.ID, expectedRevision int64, in server.UpdateInput) (server.Server, error)
}

// ServerRefresher 是 HTTP 层对运行态刷新的最小依赖（消费者定义接口）。
type ServerRefresher interface {
	RefreshTools(ctx context.Context, id server.ID) (server.Server, error)
}

// ServerLifecycle 是 HTTP 层对运行意图控制的最小依赖（消费者定义接口）。
// 每个方法返回写入后的 Server 快照，HTTP 层原样回给调用方（不做二次查询）。
// 未装配（nil）时五个生命周期动作不参与分发：动作路由仍可能因 Refresher 而注册，
// 此时这些动作按"未知动作"处理，与未知 ID 保持同一语义。
type ServerLifecycle interface {
	SetEnabled(ctx context.Context, id server.ID, enabled bool) (server.Server, error)
	Start(ctx context.Context, id server.ID) (server.Server, error)
	Stop(ctx context.Context, id server.ID) (server.Server, error)
	Restart(ctx context.Context, id server.ID) (server.Server, error)
}

// 管理 API 的动作名（详细设计 §13 的 AIP 自定义方法）。
const (
	actionRefreshTools = "refresh-tools"
	actionEnable       = "enable"
	actionDisable      = "disable"
	actionStart        = "start"
	actionStop         = "stop"
	actionRestart      = "restart"
)

// 对外错误码，对应详细设计 11.1 的子集。
const (
	codeInvalidArgument = "invalid_argument"
	codeNotFound        = "not_found"
	codeConflict        = "conflict"
	codeInternal        = "internal"
)

// 管理请求体上限;防止异常客户端耗尽内存。
const maxBodyBytes = 1 << 20

type serverHandler struct {
	registry  ServerRegistry
	refresher ServerRefresher
	lifecycle ServerLifecycle
	auditor   AuditRecorder
	logger    *slog.Logger
}

func (h *serverHandler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
		return
	}

	created, err := h.registry.Register(r.Context(), req.toInput())
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	// 审计先于响应写入：状态变更必须在它可见之前留下记录。
	// Detail 只放非敏感形态字段——注册请求里可能带 endpoint、命令行与环境变量。
	h.record(r.Context(), audit.Input{
		Type:      audit.EventServerRegistered,
		Outcome:   audit.OutcomeSuccess,
		AssetID:   string(created.ID),
		AssetName: created.Name,
		Target:    string(created.ID),
		Detail: audit.Detail(map[string]any{
			"namespace":    created.Namespace,
			"transport":    string(created.Spec.Transport),
			"runtime_type": string(created.Spec.Runtime.Type),
			"revision":     created.Revision,
		}),
	})

	// 202：Runtime 启动与后端初始化在请求之后异步完成（详细设计 10.1）。
	w.Header().Set("Location", selfLink(created.ID))
	writeJSON(w, http.StatusAccepted, toServerResponse(created))
}

func (h *serverHandler) get(w http.ResponseWriter, r *http.Request) {
	srv, err := h.registry.Get(r.Context(), server.ID(r.PathValue("id")))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerResponse(srv))
}

// list 返回全部 Server，顺序由存储层保证（namespace, name）。
// v0.1 不分页：自托管规模下稳定排序比游标更有用；未知 query 参数一律忽略。
func (h *serverHandler) list(w http.ResponseWriter, r *http.Request) {
	servers, err := h.registry.List(r.Context())
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	// 预分配并把空集合渲染成 []：items 为 null 会迫使调用方多写一条分支。
	items := make([]serverResponse, 0, len(servers))
	for _, srv := range servers {
		items = append(items, toServerResponse(srv))
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items, Count: len(items)})
}

// update 全量替换 Server 的可变配置：请求体的 revision 是乐观锁载体（必填）。
// 配置变更需要重建实例并重拉 Tool 快照，因此与注册同为 202 + Location；
// 收敛由队列与巡检完成，调用方通过 GET 观察 revision 与 phase。
func (h *serverHandler) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
		return
	}
	// 乐观锁与 runtime 的存在性是 HTTP 契约的一部分：在调用应用服务之前拒绝，
	// 让 400 的语义与"仓储层冲突"（409）明确分开。
	if req.Revision <= 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "revision is required")
		return
	}
	if req.Runtime == nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "runtime is required")
		return
	}

	updated, err := h.registry.Update(r.Context(), server.ID(r.PathValue("id")), req.Revision, req.toInput())
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	// Detail 只放 revision 水位：请求体里的 runtime/credential 可能含敏感上下文。
	h.record(r.Context(), audit.Input{
		Type:      audit.EventServerUpdated,
		Outcome:   audit.OutcomeSuccess,
		AssetID:   string(updated.ID),
		AssetName: updated.Name,
		Target:    string(updated.ID),
		Detail: audit.Detail(map[string]any{
			"from_revision": req.Revision,
			"to_revision":   updated.Revision,
		}),
	})

	w.Header().Set("Location", selfLink(updated.ID))
	writeJSON(w, http.StatusAccepted, toServerResponse(updated))
}

// action 处理 AIP 风格的动作路由 POST /api/v1/mcp-servers/{id}:{action}。
// 路径由 {rest...} 通配段承接，这里解析 "<id>:<action>" 并分发；
// 未识别的动作、以及所需依赖未装配的动作，一律返回 not_found，
// 与未知 ID 保持同一语义（不暴露"这个动作存在但没装配"的内部信息）。
func (h *serverHandler) action(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseAction(r.PathValue("rest"))
	if !ok {
		writeError(w, r, http.StatusNotFound, codeNotFound, "unknown action")
		return
	}

	switch action {
	case actionRefreshTools:
		if h.refresher == nil {
			writeError(w, r, http.StatusNotFound, codeNotFound, "unknown action")
			return
		}
		h.refreshTools(w, r, id)
	case actionEnable, actionDisable, actionStart, actionStop, actionRestart:
		if h.lifecycle == nil {
			writeError(w, r, http.StatusNotFound, codeNotFound, "unknown action")
			return
		}
		h.lifecycleAction(w, r, id, action)
	default:
		writeError(w, r, http.StatusNotFound, codeNotFound, "unknown action")
	}
}

// lifecycleAction 分发五个运行意图动作。状态码按"结果能否立即观测"区分：
// enable/start/restart 需要后台重建实例（202 + Location），
// disable/stop 是同步回收、终态（phase=stopped）在响应里立即可见（200）。
// 动作语义完全在路径里，因此带非空请求体一律拒绝：静默忽略体字段会让调用方
// 误以为体里的配置生效了。
func (h *serverHandler) lifecycleAction(w http.ResponseWriter, r *http.Request, id server.ID, action string) {
	if r.ContentLength > 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "action does not accept a request body")
		return
	}

	var (
		eventType audit.EventType
		accepted  bool
		apply     func(ctx context.Context, id server.ID) (server.Server, error)
	)
	switch action {
	case actionEnable:
		eventType, accepted = audit.EventServerEnabled, true
		apply = func(ctx context.Context, id server.ID) (server.Server, error) {
			return h.lifecycle.SetEnabled(ctx, id, true)
		}
	case actionDisable:
		eventType, accepted = audit.EventServerDisabled, false
		apply = func(ctx context.Context, id server.ID) (server.Server, error) {
			return h.lifecycle.SetEnabled(ctx, id, false)
		}
	case actionStart:
		eventType, accepted = audit.EventServerStarted, true
		apply = h.lifecycle.Start
	case actionStop:
		eventType, accepted = audit.EventServerStopped, false
		apply = h.lifecycle.Stop
	case actionRestart:
		eventType, accepted = audit.EventServerRestarted, true
		apply = h.lifecycle.Restart
	default:
		// 调用方只放行上面五个动作；这里兜底是为了让"新增动作忘了补表"表现为 404，
		// 而不是空事件类型 + nil 函数调用。
		writeError(w, r, http.StatusNotFound, codeNotFound, "unknown action")
		return
	}

	srv, err := apply(r.Context(), id)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	// 五个动作共用同一形状的 Detail：id 与结局在事件头部，revision 表达配置水位，
	// phase 表达写库后调用方能看到的状态。
	h.record(r.Context(), audit.Input{
		Type:      eventType,
		Outcome:   audit.OutcomeSuccess,
		AssetID:   string(srv.ID),
		AssetName: srv.Name,
		Target:    string(srv.ID),
		Detail: audit.Detail(map[string]any{
			"revision": srv.Revision,
			"phase":    string(srv.Status.Phase),
		}),
	})

	if accepted {
		w.Header().Set("Location", selfLink(srv.ID))
		writeJSON(w, http.StatusAccepted, toServerResponse(srv))
		return
	}
	writeJSON(w, http.StatusOK, toServerResponse(srv))
}

// refreshTools 排队一次 Tool 快照刷新。与注册端点同为 202：
// 真正的拉取在后台完成，调用方通过 GET 观察 phase 与 consecutiveFailures。
func (h *serverHandler) refreshTools(w http.ResponseWriter, r *http.Request, id server.ID) {
	srv, err := h.refresher.RefreshTools(r.Context(), id)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	h.record(r.Context(), audit.Input{
		Type:      audit.EventServerRefreshRequested,
		Outcome:   audit.OutcomeSuccess,
		AssetID:   string(srv.ID),
		AssetName: srv.Name,
		Target:    string(srv.ID),
		Detail:    audit.Detail(map[string]any{"revision": srv.Revision}),
	})
	w.Header().Set("Location", selfLink(srv.ID))
	writeJSON(w, http.StatusAccepted, toServerResponse(srv))
}

// record 写审计事件：Auditor 未装配（测试）时静默跳过。
// 写失败只记 error 日志，绝不改变已经成功的业务结果。
func (h *serverHandler) record(ctx context.Context, in audit.Input) {
	if h.auditor == nil {
		return
	}
	// 调用者与 request_id 从请求 ctx 补齐：注册请求的 Detail 里没有任何身份信息。
	if err := h.auditor.Record(ctx, observability.EnrichAuditInput(ctx, in)); err != nil {
		h.logger.Error("record audit event", "event_type", in.Type, "error", err)
	}
}

// parseAction 拆分 "{id}:{action}"；ID 必须非空且不含 "/"（后者说明路径更深，非本路由语义）。
func parseAction(rest string) (server.ID, string, bool) {
	id, action, found := strings.Cut(rest, ":")
	if !found || id == "" || action == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return server.ID(id), action, true
}

// writeDomainError 把领域哨兵错误映射为状态码；未知错误只记日志，
// 对客户端只返回固定文案，不泄漏内部细节。
func (h *serverHandler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, server.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
	case errors.Is(err, server.ErrAlreadyExists):
		writeError(w, r, http.StatusConflict, codeConflict, "server already exists")
	case errors.Is(err, server.ErrConflict):
		// 乐观锁失败：请求里的 revision 与当前配置版本不符（含并发 PUT）。
		writeError(w, r, http.StatusConflict, codeConflict, "server revision conflict")
	case errors.Is(err, server.ErrNotRunnable):
		writeError(w, r, http.StatusConflict, codeConflict, "server is not running")
	case errors.Is(err, server.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "server not found")
	default:
		h.logger.Error("request failed",
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError 是错误信封的唯一写入点：它同时把错误码写进请求记录，
// 让请求日志（以及后续 metrics）不需要解析响应体就能拿到 error_code。
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	setRecordErrorCode(r.Context(), code)
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
