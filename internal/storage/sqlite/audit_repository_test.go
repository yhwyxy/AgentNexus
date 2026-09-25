package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

// 审计行的读回形态被外部工具（sqlite3、后续的查询 API）直接依赖：
// asset_id 为空必须是 NULL（否则 ON DELETE SET NULL 语义失效）、
// detail_json 必须有默认 '{}'（否则 json_extract 报错）、
// occurred_at 必须是 UTC 且单调可比较（字典序 = 时间序）。
func TestAuditRepositoryAppendNormalizesRow(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	// audit_events.asset_id 有指向 assets 的外键；引用不存在的资产必须失败。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO assets (id, kind, namespace, name, created_at, updated_at)
		VALUES ('asset-1', 'MCPServer', 'default', 'weather', '2026-09-25T05:00:00Z', '2026-09-25T05:00:00Z')`,
	); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	fallback := time.Date(2026, 9, 25, 5, 0, 0, 0, time.UTC)
	repo := sqlite.NewAuditRepository(db).WithClock(func() time.Time { return fallback })

	minimal := audit.Event{
		ID:         "evt-1",
		Type:       audit.EventToolCalled,
		Outcome:    audit.OutcomeSuccess,
		AssetID:    "",
		OccurredAt: time.Time{},
	}
	if err := repo.Append(ctx, minimal); err != nil {
		t.Fatalf("append minimal event: %v", err)
	}

	if err := repo.Append(ctx, audit.Event{
		ID: "evt-2", Type: audit.EventServerRegistered, Outcome: audit.OutcomeError,
		AssetID: "asset-1", AssetName: "weather", ActorName: "admin", ActorRole: "admin",
		RequestID: "req-1", Target: "asset-1", RuntimeID: "rt-1", ErrorCode: "conflict",
		DurationMs: 12, OccurredAt: fallback.In(time.FixedZone("CST", 8*3600)),
		Detail: json.RawMessage(`{"revision":2}`),
	}); err != nil {
		t.Fatalf("append full event: %v", err)
	}

	var (
		assetID    *string
		detail     string
		occurredAt string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT asset_id, detail_json, occurred_at FROM audit_events WHERE id = 'evt-1'`,
	).Scan(&assetID, &detail, &occurredAt); err != nil {
		t.Fatalf("read minimal event: %v", err)
	}
	if assetID != nil {
		t.Fatalf("empty asset id stored as %q, want NULL", *assetID)
	}
	if detail != "{}" {
		t.Fatalf("detail = %q, want {}", detail)
	}
	if occurredAt != "2026-09-25T05:00:00Z" {
		t.Fatalf("occurred_at = %q, want the injected clock in UTC", occurredAt)
	}

	// 带时区的 OccurredAt 也要落到同一 UTC 表示上，否则跨时区的行无法按字典序排序。
	var converted string
	if err := db.QueryRowContext(ctx,
		`SELECT occurred_at FROM audit_events WHERE id = 'evt-2'`,
	).Scan(&converted); err != nil {
		t.Fatalf("read full event: %v", err)
	}
	if converted != occurredAt {
		t.Fatalf("occurred_at = %q, want %q", converted, occurredAt)
	}

	if err := repo.Append(ctx, audit.Event{Type: audit.EventToolCalled}); err == nil {
		t.Fatal("append without ID succeeded, want caller-assigned ID requirement")
	}
}

// 资产删除后审计行必须保留，只把 asset_id 置空：事件历史不随资产生命周期消失。
func TestAuditRepositoryKeepsEventAfterAssetDeletion(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO assets (id, kind, namespace, name, created_at, updated_at)
		VALUES ('asset-1', 'MCPServer', 'default', 'weather', '2026-09-25T05:00:00Z', '2026-09-25T05:00:00Z')`,
	); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	repo := sqlite.NewAuditRepository(db)
	if err := repo.Append(ctx, audit.Event{
		ID: "evt-1", Type: audit.EventServerRegistered, Outcome: audit.OutcomeSuccess,
		AssetID: "asset-1", AssetName: "weather",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM assets WHERE id = 'asset-1'`); err != nil {
		t.Fatalf("delete asset: %v", err)
	}

	var (
		assetID   *string
		assetName string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT asset_id, asset_name FROM audit_events WHERE id = 'evt-1'`,
	).Scan(&assetID, &assetName); err != nil {
		t.Fatalf("read event after asset deletion: %v", err)
	}
	if assetID != nil {
		t.Fatalf("asset_id = %q after deletion, want NULL", *assetID)
	}
	if assetName != "weather" {
		t.Fatalf("asset_name = %q, want the redundant copy to survive deletion", assetName)
	}
}
