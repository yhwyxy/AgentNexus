package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

var (
	ErrToolNotFound      = errors.New("tool not found")
	ErrInvalidArguments  = errors.New("invalid tool arguments")
	ErrServerUnavailable = errors.New("tool server unavailable")
)

type Catalog interface {
	Snapshot(context.Context) (*tool.CatalogSnapshot, error)
	Resolve(context.Context, string) (tool.Route, error)
}

type ToolRouter interface {
	Call(context.Context, CallToolRequest) (mcpclient.CallResult, error)
}

// ToolCallObservation 是一次 tools/call 的事实:解析结果、结局、错误码与耗时。
// PublicName 总是有值(未解析成功时为调用方请求的名字),Route 只在 Resolved 为真时有效。
type ToolCallObservation struct {
	PublicName string
	Route      tool.Route
	Resolved   bool
	Outcome    audit.Outcome
	ErrorCode  string
	Duration   time.Duration
	// IsError 表达后端工具自身的 isError=true:它是合法结果(Outcome=success),
	// 与网关侧的传输/校验失败不同(详细设计 §11.2)。
	IsError bool
}

// CallObserver 接收工具调用事实。实现方是审计/metrics 适配器,必须快速返回且不得阻塞调用链。
type CallObserver interface {
	ToolCalled(ctx context.Context, obs ToolCallObservation)
}

type CallToolRequest struct {
	Name      string
	Arguments json.RawMessage
}

type VirtualServer struct {
	catalog Catalog
	router  ToolRouter
}

func NewVirtualServer(catalog Catalog, router ToolRouter) (*VirtualServer, error) {
	if catalog == nil || router == nil {
		return nil, errors.New("virtual MCP server requires catalog and router")
	}
	return &VirtualServer{catalog: catalog, router: router}, nil
}

func (s *VirtualServer) ListTools(ctx context.Context, cursor string) ([]tool.Definition, string, error) {
	const pageSize = 100
	snapshot, err := s.catalog.Snapshot(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load MCP tool catalog: %w", err)
	}
	start := 0
	if cursor != "" {
		digest, offset, ok := strings.Cut(cursor, ":")
		if !ok || digest != snapshot.Digest {
			return nil, "", errors.New("invalid MCP tools cursor")
		}
		start, err = strconv.Atoi(offset)
		if err != nil || start < 0 || start > len(snapshot.Ordered) {
			return nil, "", errors.New("invalid MCP tools cursor")
		}
	}
	end := min(start+pageSize, len(snapshot.Ordered))
	var next string
	if end < len(snapshot.Ordered) {
		next = snapshot.Digest + ":" + strconv.Itoa(end)
	}
	return append([]tool.Definition(nil), snapshot.Ordered[start:end]...), next, nil
}

func (s *VirtualServer) CallTool(ctx context.Context, req CallToolRequest) (mcpclient.CallResult, error) {
	return s.router.Call(ctx, req)
}

type ToolCaller struct {
	catalog  Catalog
	servers  server.Repository
	runtimes runtime.Manager
	clients  mcpclient.Manager
	// observer 只在构造期设置(WithObserver),之后只读;nil 表示不观察。
	observer CallObserver
}

func NewToolCaller(catalog Catalog, servers server.Repository, runtimes runtime.Manager, clients mcpclient.Manager) (*ToolCaller, error) {
	if catalog == nil || servers == nil || runtimes == nil || clients == nil {
		return nil, errors.New("tool caller requires catalog, server repository, runtime manager, and MCP client manager")
	}
	return &ToolCaller{catalog: catalog, servers: servers, runtimes: runtimes, clients: clients}, nil
}

// WithObserver 安装调用观察者,只在构造后立即调用(非并发安全)。
func (r *ToolCaller) WithObserver(observer CallObserver) *ToolCaller {
	r.observer = observer
	return r
}

// callObservation 在 Call 内累积本次调用的事实,由 defer 统一上报:
// 这样"未路由成功"的失败也有一条完整的调用记录,而不只是成功路径。
type callObservation struct {
	publicName string
	route      tool.Route
	resolved   bool
	errorCode  string
}

func (r *ToolCaller) Call(ctx context.Context, req CallToolRequest) (result mcpclient.CallResult, err error) {
	started := time.Now()
	observation := &callObservation{publicName: req.Name, errorCode: audit.CodeInternal}
	defer func() {
		r.observeToolCall(ctx, observation, result, err, time.Since(started))
	}()

	return r.call(ctx, req, observation)
}

// call 是真正的调用链:所有失败返回点都显式标注错误码,避免依赖错误字符串推断。
func (r *ToolCaller) call(ctx context.Context, req CallToolRequest, observation *callObservation) (mcpclient.CallResult, error) {
	route, err := r.catalog.Resolve(ctx, req.Name)
	if err != nil {
		if errors.Is(err, tool.ErrToolNotFound) {
			observation.errorCode = audit.CodeNotFound

			return mcpclient.CallResult{}, ErrToolNotFound
		}
		observation.errorCode = audit.CodeInternal

		return mcpclient.CallResult{}, fmt.Errorf("resolve public tool: %w", err)
	}
	snapshot, err := r.catalog.Snapshot(ctx)
	if err != nil {
		observation.errorCode = audit.CodeInternal

		return mcpclient.CallResult{}, fmt.Errorf("load tool definition: %w", err)
	}
	var definition *tool.Definition
	for i := range snapshot.Ordered {
		if snapshot.Ordered[i].PublicName == req.Name {
			definition = &snapshot.Ordered[i]
			break
		}
	}
	if definition == nil || definition.ServerID != route.ServerID || definition.BackendName != route.BackendName {
		observation.errorCode = audit.CodeNotFound

		return mcpclient.CallResult{}, ErrToolNotFound
	}
	observation.route = route
	observation.resolved = true
	args, err := decodeArguments(req.Arguments)
	if err != nil {
		observation.errorCode = audit.CodeInvalidArgument

		return mcpclient.CallResult{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		observation.errorCode = audit.CodeInternal

		return mcpclient.CallResult{}, fmt.Errorf("load tool input schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		observation.errorCode = audit.CodeInternal

		return mcpclient.CallResult{}, fmt.Errorf("resolve tool input schema: %w", err)
	}
	if err := resolved.Validate(args); err != nil {
		observation.errorCode = audit.CodeInvalidArgument

		return mcpclient.CallResult{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	srv, err := r.servers.GetByID(ctx, route.ServerID)
	if err != nil || !srv.Enabled || srv.Spec.DesiredState != server.DesiredRunning {
		observation.errorCode = audit.CodeServerUnavailable

		return mcpclient.CallResult{}, ErrServerUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, srv.Spec.Timeouts.Call)
	defer cancel()
	instance, err := r.runtimes.EnsureReady(callCtx, srv)
	if err != nil {
		observation.errorCode = audit.ErrorCode(err)

		return mcpclient.CallResult{}, fmt.Errorf("ensure tool server ready: %w", err)
	}
	lease, err := r.clients.Acquire(callCtx, srv, instance)
	if err != nil {
		observation.errorCode = audit.CodeBackendUnavailable

		return mcpclient.CallResult{}, fmt.Errorf("acquire MCP session: %w", err)
	}
	defer lease.Release()
	arguments := req.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	result, err := lease.Session().CallTool(callCtx, mcpclient.CallRequest{Name: route.BackendName, Arguments: arguments})
	if err != nil {
		observation.errorCode = audit.CodeBackendUnavailable
		r.clients.Invalidate(route.ServerID, err)

		return mcpclient.CallResult{}, fmt.Errorf("call backend tool: %w", err)
	}
	return result, nil
}

// observeToolCall 派生结局并上报。取消(context.Canceled)只由 Outcome 表达:
// 详细设计 §11.1 没有 cancelled 码,因此此时 ErrorCode 为空。
func (r *ToolCaller) observeToolCall(ctx context.Context, observation *callObservation, result mcpclient.CallResult, err error, duration time.Duration) {
	if r.observer == nil {
		return
	}
	outcome := audit.OutcomeOf(err)
	errorCode := observation.errorCode
	if err == nil || outcome == audit.OutcomeCancelled {
		errorCode = ""
	}
	r.observer.ToolCalled(ctx, ToolCallObservation{
		PublicName: observation.publicName,
		Route:      observation.route,
		Resolved:   observation.resolved,
		Outcome:    outcome,
		ErrorCode:  errorCode,
		Duration:   duration,
		IsError:    result.IsError,
	})
}

func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return args, nil
}
