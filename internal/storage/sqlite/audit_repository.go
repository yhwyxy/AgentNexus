package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/audit"
)

// AuditRepository 实现 audit.Repository：审计事件只追加，没有更新与删除路径。
type AuditRepository struct {
	db  *sql.DB
	now func() time.Time
}

var _ audit.Repository = (*AuditRepository)(nil)

func NewAuditRepository(db *sql.DB) *AuditRepository {
	return &AuditRepository{db: db, now: time.Now}
}

func (r *AuditRepository) WithClock(now func() time.Time) *AuditRepository {
	r.now = now
	return r
}

func (r *AuditRepository) Append(ctx context.Context, event audit.Event) error {
	if event.ID == "" {
		return fmt.Errorf("append audit event: ID must be assigned by caller")
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = r.now()
	}
	detail := event.Detail
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	var assetID any
	if event.AssetID != "" {
		assetID = event.AssetID
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, occurred_at, event_type, outcome, actor_name, actor_role,
			request_id, asset_id, asset_name, target, runtime_id, error_code, duration_ms, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, formatTime(occurredAt.UTC()), string(event.Type), string(event.Outcome),
		event.ActorName, event.ActorRole, event.RequestID, assetID, event.AssetName,
		event.Target, event.RuntimeID, event.ErrorCode, event.DurationMs, string(detail)); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
