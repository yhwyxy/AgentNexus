// 创建 SQLite 数据库连接，并通过 PingContext 验证连接可用
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	return db, nil
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)

	if dir == "." {
		return nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create database directory %q: %w", dir, err)
	}

	return nil
}

// 为所有 SQLite 连接统一应用 AgentNexus 的数据库基线
func buildDSN(path string) (string, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path %q: %w", path, err)
	}
	dsn := &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(absolutePath),
	}

	query := dsn.Query()
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "5000")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")

	dsn.RawQuery = query.Encode()

	return dsn.String(), nil
}
