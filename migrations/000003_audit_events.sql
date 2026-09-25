-- 审计事件：只追加表，记录状态变更与工具调用这类需要长期留存的事实。
-- 约定：
--   * occurred_at 为 UTC RFC3339Nano 字符串（与其它表一致）；
--   * asset_id 用 ON DELETE SET NULL：资产删除后事件保留，asset_name 是为此冗余的；
--   * error_code 取详细设计 §11.1 的内部错误码；outcome 只有 success/error/cancelled；
--   * detail_json 已按 deny-list 脱敏，不得包含 endpoint、宿主机路径、命令行、env、凭证。
CREATE TABLE audit_events (
    id          TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    outcome     TEXT NOT NULL CHECK (outcome IN ('success', 'error', 'cancelled')),
    actor_name  TEXT NOT NULL DEFAULT '',
    actor_role  TEXT NOT NULL DEFAULT '',
    request_id  TEXT NOT NULL DEFAULT '',
    asset_id    TEXT NULL REFERENCES assets(id) ON DELETE SET NULL,
    asset_name  TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',
    runtime_id  TEXT NOT NULL DEFAULT '',
    error_code  TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NULL CHECK (duration_ms IS NULL OR duration_ms >= 0),
    detail_json TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_audit_events_occurred_at ON audit_events(occurred_at DESC);
CREATE INDEX idx_audit_events_asset ON audit_events(asset_id, occurred_at DESC);
CREATE INDEX idx_audit_events_actor ON audit_events(actor_name, occurred_at DESC);
