package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

func TestNormalizeToolsCanonicalizesAndRejectsInvalidDefinitions(t *testing.T) {
	first := []mcpclient.Tool{
		{Name: "zeta", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)},
		{Name: "alpha", InputSchema: json.RawMessage(`{"required":[],"type":"object"}`)},
	}
	second := []mcpclient.Tool{
		{Name: "alpha", InputSchema: json.RawMessage(` { "type" : "object", "required" : [] } `)},
		{Name: "zeta", InputSchema: json.RawMessage(`{"properties":{"n":{"type":"integer"}},"type":"object"}`)},
	}
	newID := func() string { return "tool-id" }
	definitionsA, digestA, err := NormalizeTools(server.ID("server-1"), "weather", first, newID)
	if err != nil {
		t.Fatal(err)
	}
	definitionsB, digestB, err := NormalizeTools(server.ID("server-1"), "weather", second, newID)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("equivalent schemas have different catalog digests: %s != %s", digestA, digestB)
	}
	if string(definitionsA[0].InputSchema) != `{"properties":{"n":{"type":"integer"}},"type":"object"}` {
		t.Fatalf("input schema not canonicalized: %s", definitionsA[0].InputSchema)
	}
	if definitionsB[0].PublicName != "weather.alpha" || definitionsB[1].PublicName != "weather.zeta" {
		t.Fatalf("normalized definitions = %#v", definitionsB)
	}
	for _, invalid := range [][]mcpclient.Tool{
		{{Name: "bad name", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		{{Name: "x", InputSchema: json.RawMessage(`[]`)}},
		{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"} {}`)}},
		{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	} {
		if _, _, err := NormalizeTools(server.ID("server-1"), "weather", invalid, newID); !errors.Is(err, ErrInvalidTool) {
			t.Errorf("NormalizeTools(%#v) error = %v, want ErrInvalidTool", invalid, err)
		}
	}
}

type pageSession struct {
	pages map[string]struct {
		tools []mcpclient.Tool
		next  string
	}
	requested []string
}

func (s *pageSession) ListTools(_ context.Context, cursor string) ([]mcpclient.Tool, string, error) {
	s.requested = append(s.requested, cursor)
	page, ok := s.pages[cursor]
	if !ok {
		return nil, "", errors.New("unexpected cursor")
	}
	return page.tools, page.next, nil
}
func (*pageSession) CallTool(context.Context, mcpclient.CallRequest) (mcpclient.CallResult, error) {
	return mcpclient.CallResult{}, nil
}
func (*pageSession) Ping(context.Context) error { return nil }
func (*pageSession) Close() error               { return nil }

func TestListAllToolsFollowsPagesAndRejectsCursorCycle(t *testing.T) {
	first := mcpclient.Tool{Name: "first", InputSchema: json.RawMessage(`{"type":"object"}`)}
	second := mcpclient.Tool{Name: "second", InputSchema: json.RawMessage(`{"type":"object"}`)}
	session := &pageSession{pages: map[string]struct {
		tools []mcpclient.Tool
		next  string
	}{"": {tools: []mcpclient.Tool{first}, next: "next"}, "next": {tools: []mcpclient.Tool{second}}}}
	tools, err := listAllTools(context.Background(), session, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || session.requested[0] != "" || session.requested[1] != "next" {
		t.Fatalf("listed tools=%#v cursors=%#v", tools, session.requested)
	}

	cycle := &pageSession{pages: map[string]struct {
		tools []mcpclient.Tool
		next  string
	}{"": {next: "again"}, "again": {next: "again"}}}
	if _, err := listAllTools(context.Background(), cycle, 0); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("cycle error = %v, want ErrInvalidTool", err)
	}
}
