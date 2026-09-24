package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
)

func TestConnectorListAndCallFakeServer(t *testing.T) {
	fake := fakemcp.New()
	defer fake.Close()

	connector := NewConnector()
	session, err := connector.Connect(context.Background(), runtime.ConnectTarget{
		Transport: "streamable_http",
		URL:       fake.HTTP.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, next, err := session.ListTools(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(tools) != 2 {
		t.Fatalf("tools = %#v, next = %q", tools, next)
	}

	result, err := session.CallTool(ctx, mcpclient.CallRequest{
		Name:      "demo.echo",
		Arguments: json.RawMessage(`{"message":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
