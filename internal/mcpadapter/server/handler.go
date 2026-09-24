package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

type VirtualServer interface {
	ListTools(context.Context, string) ([]tool.Definition, string, error)
	CallTool(context.Context, gateway.CallToolRequest) (mcpclient.CallResult, error)
}

type Handler struct {
	virtual    VirtualServer
	server     *mcp.Server
	httpServer *mcp.StreamableHTTPHandler
	mu         sync.Mutex
	tools      map[string]tool.Definition
}

func NewHandler(virtual VirtualServer) (*Handler, error) {
	if virtual == nil {
		return nil, errors.New("MCP handler requires a virtual server")
	}
	h := &Handler{
		virtual: virtual,
		server:  mcp.NewServer(&mcp.Implementation{Name: "agentnexus", Version: "0.1.0"}, nil),
		tools:   make(map[string]tool.Definition),
	}
	h.httpServer = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.server }, nil)
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.refresh(r.Context()); err != nil {
		http.Error(w, "MCP catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	h.httpServer.ServeHTTP(w, r)
}

func (h *Handler) refresh(ctx context.Context) error {
	definitions, cursor, err := h.virtual.ListTools(ctx, "")
	if err != nil {
		return err
	}
	for cursor != "" {
		var page []tool.Definition
		page, cursor, err = h.virtual.ListTools(ctx, cursor)
		if err != nil {
			return err
		}
		definitions = append(definitions, page...)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	current := make(map[string]tool.Definition, len(definitions))
	for _, definition := range definitions {
		current[definition.PublicName] = definition
		if old, exists := h.tools[definition.PublicName]; exists && reflect.DeepEqual(old, definition) {
			continue
		}
		h.server.RemoveTools(definition.PublicName)
		var annotations mcp.ToolAnnotations
		if len(definition.Annotations) > 0 {
			if err := json.Unmarshal(definition.Annotations, &annotations); err != nil {
				return fmt.Errorf("decode tool annotations: %w", err)
			}
		}
		definition := definition
		h.server.AddTool(&mcp.Tool{
			Name: definition.PublicName, Title: definition.Title,
			Description: definition.Description, InputSchema: definition.InputSchema,
			OutputSchema: definition.OutputSchema, Annotations: &annotations,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := h.virtual.CallTool(ctx, gateway.CallToolRequest{Name: req.Params.Name, Arguments: req.Params.Arguments})
			if err != nil {
				return nil, safeToolError(err)
			}
			return toSDKResult(result)
		})
	}
	for name := range h.tools {
		if _, exists := current[name]; !exists {
			h.server.RemoveTools(name)
		}
	}
	h.tools = current
	return nil
}

func safeToolError(err error) error {
	switch {
	case errors.Is(err, gateway.ErrToolNotFound):
		return gateway.ErrToolNotFound
	case errors.Is(err, gateway.ErrInvalidArguments):
		return gateway.ErrInvalidArguments
	case errors.Is(err, gateway.ErrServerUnavailable):
		return gateway.ErrServerUnavailable
	default:
		return errors.New("tool call failed")
	}
}

func toSDKResult(result mcpclient.CallResult) (*mcp.CallToolResult, error) {
	if result.IsError {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "tool execution failed"}},
		}, nil
	}
	out := &mcp.CallToolResult{}
	if len(result.StructuredContent) != 0 {
		var value any
		if err := json.Unmarshal(result.StructuredContent, &value); err != nil {
			return nil, fmt.Errorf("decode structured tool result: %w", err)
		}
		out.StructuredContent = value
	}
	for _, content := range result.Content {
		switch content.Type {
		case "text":
			out.Content = append(out.Content, &mcp.TextContent{Text: content.Text})
		case "image":
			var item mcp.ImageContent
			if err := json.Unmarshal(content.Data, &item); err != nil {
				return nil, fmt.Errorf("decode image tool content: %w", err)
			}
			out.Content = append(out.Content, &item)
		case "audio":
			var item mcp.AudioContent
			if err := json.Unmarshal(content.Data, &item); err != nil {
				return nil, fmt.Errorf("decode audio tool content: %w", err)
			}
			out.Content = append(out.Content, &item)
		case "resource":
			var item mcp.EmbeddedResource
			if err := json.Unmarshal(content.Data, &item); err != nil {
				return nil, fmt.Errorf("decode resource tool content: %w", err)
			}
			out.Content = append(out.Content, &item)
		case "resource_link":
			var item mcp.ResourceLink
			if err := json.Unmarshal(content.Data, &item); err != nil {
				return nil, fmt.Errorf("decode resource link tool content: %w", err)
			}
			out.Content = append(out.Content, &item)
		default:
			return nil, fmt.Errorf("unsupported tool content type %q", content.Type)
		}
	}
	return out, nil
}
