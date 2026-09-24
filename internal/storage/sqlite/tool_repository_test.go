package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func newToolRepos(t *testing.T) (*sqlite.ServerRepository, *sqlite.ToolRepository, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return sqlite.NewServerRepository(db), sqlite.NewToolRepository(db), ctx
}

func toolServer(id, namespace, name string) server.Server {
	return server.Server{
		ID: server.ID(id), Namespace: namespace, Name: name, Enabled: true, Revision: 1,
		Spec: server.Spec{
			Transport: server.TransportStreamableHTTP,
			Runtime:   server.RuntimeSpec{Type: server.RuntimeRemote, Remote: &server.RemoteSpec{Endpoint: "http://localhost/mcp"}},
			Timeouts:  server.TimeoutSpec{Connect: time.Second, List: time.Second, Call: time.Second},
			Limits:    server.LimitSpec{MaxInFlight: 4}, DesiredState: server.DesiredRunning,
		},
	}
}

func definition(id string, srv server.Server) tool.Definition {
	return tool.Definition{
		ID: id, ServerID: srv.ID, ServerName: srv.Name, BackendName: "echo",
		PublicName: srv.Name + ".echo", InputSchema: []byte(`{"type":"object"}`),
		Annotations: []byte(`{}`), SchemaDigest: "schema-digest",
	}
}

func TestReplaceSnapshotIsAtomicAndPreservesPreviousActive(t *testing.T) {
	servers, tools, ctx := newToolRepos(t)
	srv := toolServer("server-1", "default", "demo")
	if err := servers.Create(ctx, srv); err != nil {
		t.Fatal(err)
	}
	first, err := tools.ReplaceSnapshot(ctx, tool.SnapshotReplacement{
		ID: "snapshot-1", ServerID: srv.ID, Revision: 1, CatalogDigest: "first",
		Tools: []tool.Definition{definition("tool-1", srv)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || first.State != tool.SnapshotActive || first.ToolCount != 1 {
		t.Fatalf("unexpected initial snapshot: %#v", first)
	}

	duplicate := definition("tool-3", srv)
	if _, err := tools.ReplaceSnapshot(ctx, tool.SnapshotReplacement{
		ID: "snapshot-2", ServerID: srv.ID, Revision: 1, CatalogDigest: "second",
		Tools: []tool.Definition{definition("tool-2", srv), duplicate},
	}); err == nil {
		t.Fatal("expected duplicate backend name to abort snapshot replacement")
	}

	active, err := tools.GetActiveSnapshot(ctx, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != "snapshot-1" || active.Generation != 1 || active.CatalogDigest != "first" || len(active.Tools) != 1 {
		t.Fatalf("failed replacement changed the active snapshot: %#v", active)
	}
}

func TestToolRepositoryAggregatesActiveRevisionAndResolvesExplicitRoute(t *testing.T) {
	servers, tools, ctx := newToolRepos(t)
	srv := toolServer("server-1", "default", "demo")
	if err := servers.Create(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.ReplaceSnapshot(ctx, tool.SnapshotReplacement{
		ID: "snapshot-1", ServerID: srv.ID, Revision: 1, CatalogDigest: "digest",
		Tools: []tool.Definition{definition("tool-1", srv)},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tools.ListAggregated(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].PublicName != "demo.echo" {
		t.Fatalf("aggregated tools = %#v", all)
	}
	route, err := tools.ResolvePublicName(ctx, "demo.echo")
	if err != nil {
		t.Fatal(err)
	}
	if route.ServerID != srv.ID || route.BackendName != "echo" || route.SnapshotID != "snapshot-1" {
		t.Fatalf("resolved route = %#v", route)
	}
	if _, err := tools.ResolvePublicName(ctx, "demo.missing"); !errors.Is(err, tool.ErrToolNotFound) {
		t.Fatalf("unknown route error = %v, want ErrToolNotFound", err)
	}
}

func TestReplaceSnapshotRejectsStaleRevision(t *testing.T) {
	servers, tools, ctx := newToolRepos(t)
	srv := toolServer("server-1", "default", "demo")
	if err := servers.Create(ctx, srv); err != nil {
		t.Fatal(err)
	}
	srv, err := servers.UpdateSpec(ctx, srv.ID, 1, func(spec *server.Spec) error { spec.Timeouts.List += time.Second; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = tools.ReplaceSnapshot(ctx, tool.SnapshotReplacement{
		ID: "snapshot-old", ServerID: srv.ID, Revision: 1, CatalogDigest: "stale",
	})
	if !errors.Is(err, server.ErrConflict) {
		t.Fatalf("stale replacement error = %v, want conflict", err)
	}
}
