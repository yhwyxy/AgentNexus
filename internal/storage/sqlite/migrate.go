// 执行 SQL migration
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

type migration struct {
	version int
	name    string
}

func Migrate(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	if err := ensureMigrationTable(ctx, db); err != nil {
		return err
	}

	files, err := discoverMigrations(migrations)
	if err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	for _, m := range files {
		if applied[m.version] {
			continue
		}

		if err := applyMigration(ctx, db, migrations, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, m migration) error {
	sqlBytes, err := fs.ReadFile(fsys, m.name)
	if err != nil {
		return fmt.Errorf("read migration %q: %w", m.name, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %q: %w", m.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("execute migration %q: %w", m.name, err)
	}
	appliedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
		m.version,
		appliedAt,
	); err != nil {
		return fmt.Errorf("record migration %q: %w", m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %q: %w", m.name, err)
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT version FROM schema_migrations`,
	)
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)

	for rows.Next() {
		var version int

		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}

		applied[version] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration version: %w", err)
	}
	return applied, nil
}

func discoverMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var migrations []migration

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		name := entry.Name()
		if len(name) < 8 || name[6] != '_' {
			return nil, fmt.Errorf("invalid migration filename %q", name)
		}

		version, err := strconv.Atoi(name[:6])
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", name, err)
		}

		migrations = append(migrations, migration{
			version: version,
			name:    name,
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

func ensureMigrationTable(ctx context.Context, db *sql.DB) error {
	const query = `
	CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
	);`

	if _, err := db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	return nil
}
