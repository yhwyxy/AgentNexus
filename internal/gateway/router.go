package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
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
}

func NewToolCaller(catalog Catalog, servers server.Repository, runtimes runtime.Manager, clients mcpclient.Manager) (*ToolCaller, error) {
	if catalog == nil || servers == nil || runtimes == nil || clients == nil {
		return nil, errors.New("tool caller requires catalog, server repository, runtime manager, and MCP client manager")
	}
	return &ToolCaller{catalog: catalog, servers: servers, runtimes: runtimes, clients: clients}, nil
}

func (r *ToolCaller) Call(ctx context.Context, req CallToolRequest) (mcpclient.CallResult, error) {
	route, err := r.catalog.Resolve(ctx, req.Name)
	if err != nil {
		if errors.Is(err, tool.ErrToolNotFound) {
			return mcpclient.CallResult{}, ErrToolNotFound
		}
		return mcpclient.CallResult{}, fmt.Errorf("resolve public tool: %w", err)
	}
	snapshot, err := r.catalog.Snapshot(ctx)
	if err != nil {
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
		return mcpclient.CallResult{}, ErrToolNotFound
	}
	args, err := decodeArguments(req.Arguments)
	if err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("load tool input schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("resolve tool input schema: %w", err)
	}
	if err := resolved.Validate(args); err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	srv, err := r.servers.GetByID(ctx, route.ServerID)
	if err != nil || !srv.Enabled || srv.Spec.DesiredState != server.DesiredRunning {
		return mcpclient.CallResult{}, ErrServerUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, srv.Spec.Timeouts.Call)
	defer cancel()
	instance, err := r.runtimes.EnsureReady(callCtx, srv)
	if err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("ensure tool server ready: %w", err)
	}
	lease, err := r.clients.Acquire(callCtx, srv, instance)
	if err != nil {
		return mcpclient.CallResult{}, fmt.Errorf("acquire MCP session: %w", err)
	}
	defer lease.Release()
	arguments := req.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	result, err := lease.Session().CallTool(callCtx, mcpclient.CallRequest{Name: route.BackendName, Arguments: arguments})
	if err != nil {
		r.clients.Invalidate(route.ServerID, err)
		return mcpclient.CallResult{}, fmt.Errorf("call backend tool: %w", err)
	}
	return result, nil
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
