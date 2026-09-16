package sqlite

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"
)

func TestBuildDSN(t *testing.T) {
	dsn, err := buildDSN("data/agentnexus.db")
	if err != nil {
		t.Fatalf("build DSN: %v", err)
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}

	if parsed.Scheme != "file" {
		t.Errorf("scheme = %q, want %q", parsed.Scheme, "file")
	}

	query := parsed.Query()

	tests := map[string]string{
		"_foreign_keys": "on",
		"_busy_timeout": "5000",
		"_journal_mode": "WAL",
		"_synchronous":  "NORMAL",
	}

	for key, want := range tests {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
func TestOpenAppliesPragmas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	tests := []struct {
		pragma string
		want   string
	}{
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"journal_mode", "wal"},
		{"synchronous", "1"},
	}

	for _, tt := range tests {
		var got string

		query := "PRAGMA " + tt.pragma
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}

		if got != tt.want {
			t.Errorf(
				"PRAGMA %s = %q, want %q",
				tt.pragma,
				got,
				tt.want,
			)
		}
	}
}
