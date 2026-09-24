package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/yhwyxy/AgentNexus/migrations"
)

func TestMigrateAppliesEachVersionOnce(t *testing.T) {
	ctx := context.Background()

	db, err := Open(
		ctx,
		filepath.Join(t.TempDir(), "test.db"),
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	migrations := fstest.MapFS{
		"000001_test.sql": {
			Data: []byte(`
CREATE TABLE demo (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);

INSERT INTO demo(name) VALUES ('first');
`),
		},
	}

	if err := Migrate(ctx, db, migrations); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	if err := Migrate(ctx, db, migrations); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var count int

	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM demo`,
	).Scan(&count); err != nil {
		t.Fatalf("count demo rows: %v", err)
	}

	if count != 1 {
		t.Fatalf("demo row count = %d, want 1", count)
	}
}

func TestProductionMigrationCreatesCoreTables(t *testing.T) {
	ctx := context.Background()

	db, err := Open(
		ctx,
		filepath.Join(t.TempDir(), "test.db"),
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	expectedTables := []string{
		"schema_migrations",
		"credentials",
		"assets",
		"mcp_servers",
		"server_status",
		"tool_snapshots",
		"tools",
	}

	for _, table := range expectedTables {
		var name string

		err := db.QueryRowContext(
			ctx,
			`
SELECT name
FROM sqlite_master
WHERE type = 'table' AND name = ?
`,
			table,
		).Scan(&name)

		if err != nil {
			t.Fatalf("table %q not found: %v", table, err)
		}
	}
	var version int

	if err := db.QueryRowContext(
		ctx,
		`SELECT MAX(version) FROM schema_migrations`,
	).Scan(&version); err != nil {
		t.Fatalf("query migration version: %v", err)
	}

	if version != 2 {
		t.Fatalf("migration version = %d, want 2", version)
	}
}
