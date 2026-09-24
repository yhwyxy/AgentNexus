package tool_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/runtime/remote"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/testsupport/fakemcp"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func TestSyncPersistsAndCachesFakeMCPTools(t *testing.T) {
	ctx := context.Background()
	fake := fakemcp.New()
	defer fake.Close()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "agentnexus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	servers := sqlite.NewServerRepository(db)
	registered, err := server.NewService(servers).WithIDGenerator(func() string { return "server-1" }).Register(ctx, server.RegisterInput{
		Name: "weather", Transport: server.TransportStreamableHTTP,
		Runtime: server.RuntimeSpec{
			Type:   server.RuntimeRemote,
			Remote: &server.RemoteSpec{Endpoint: fake.HTTP.URL},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo).WithClock(func() time.Time { return time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC) })
	manager := mcpclient.NewManager(mcpadapter.NewConnector())
	defer manager.Close(ctx)
	nextID := 0
	syncer := tool.NewSyncService(servers, remote.NewProvider(), manager, toolRepo, catalog).WithIDGenerator(func() string {
		nextID++
		return "id-" + strconv.Itoa(nextID)
	})

	first, err := syncer.Sync(ctx, registered.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Snapshot.ToolCount != 2 || first.Snapshot.Generation != 1 {
		t.Fatalf("first sync = changed %v, snapshot %#v", first.Changed, first.Snapshot)
	}
	catalogSnapshot, err := catalog.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogSnapshot.Ordered) != 2 || catalogSnapshot.BuiltAt.Year() != 2026 {
		t.Fatalf("catalog snapshot = %#v", catalogSnapshot)
	}
	route, err := catalog.Resolve(ctx, "weather.demo.echo")
	if err != nil {
		t.Fatal(err)
	}
	if route.ServerID != registered.ID || route.BackendName != "demo.echo" || route.SnapshotID != first.Snapshot.ID {
		t.Fatalf("route = %#v", route)
	}

	second, err := syncer.Sync(ctx, registered.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || second.Snapshot.Generation != first.Snapshot.Generation {
		t.Fatalf("unchanged sync = %#v", second)
	}
	cached, err := catalog.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cached != catalogSnapshot {
		t.Fatal("unchanged sync invalidated the catalog cache")
	}
}
