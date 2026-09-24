package remote

import (
	"context"
	"testing"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

func TestEnsureBuildsRemoteTargetAndCopiesHeaders(t *testing.T) {
	provider := NewProvider()
	srv := server.Server{ID: "weather", Revision: 3, Spec: server.Spec{
		Runtime: server.RuntimeSpec{Type: server.RuntimeRemote, Remote: &server.RemoteSpec{
			Endpoint: "http://127.0.0.1/mcp", Headers: map[string]string{"X-Test": "yes"},
		}},
	}}
	instance, err := provider.Ensure(context.Background(), srv)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Target.URL != srv.Spec.Runtime.Remote.Endpoint || instance.ObservedRevision != 3 {
		t.Fatalf("unexpected instance: %#v", instance)
	}
	instance.Target.Headers["X-Test"] = "changed"
	if srv.Spec.Runtime.Remote.Headers["X-Test"] != "yes" {
		t.Fatal("provider mutated source headers")
	}
}

func TestEnsureRejectsInvalidEndpoint(t *testing.T) {
	provider := NewProvider()
	for _, endpoint := range []string{"relative", "file:///tmp/mcp", "http://user:pass@example.com/mcp"} {
		srv := server.Server{ID: "x", Spec: server.Spec{Runtime: server.RuntimeSpec{Type: server.RuntimeRemote, Remote: &server.RemoteSpec{Endpoint: endpoint}}}}
		if _, err := provider.Ensure(context.Background(), srv); err == nil {
			t.Fatalf("expected %q to be rejected", endpoint)
		}
	}
}
