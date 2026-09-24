CREATE TABLE tool_snapshots (
    id             TEXT PRIMARY KEY,
    server_asset_id TEXT NOT NULL,
    server_revision INTEGER NOT NULL CHECK (server_revision > 0),
    generation     INTEGER NOT NULL CHECK (generation > 0),
    state          TEXT NOT NULL CHECK (state IN ('building', 'active', 'superseded', 'failed')),
    tool_count     INTEGER NOT NULL DEFAULT 0 CHECK (tool_count >= 0),
    catalog_digest TEXT NOT NULL DEFAULT '',
    error_message  TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    activated_at   TEXT NULL,
    FOREIGN KEY (server_asset_id) REFERENCES assets(id) ON DELETE CASCADE,
    UNIQUE (server_asset_id, generation)
);

CREATE TABLE tools (
    id                 TEXT PRIMARY KEY,
    snapshot_id        TEXT NOT NULL,
    server_asset_id    TEXT NOT NULL,
    backend_name       TEXT NOT NULL,
    public_name        TEXT NOT NULL,
    title              TEXT NOT NULL DEFAULT '',
    description        TEXT NOT NULL DEFAULT '',
    input_schema_json  TEXT NOT NULL,
    output_schema_json TEXT NULL,
    annotations_json   TEXT NOT NULL DEFAULT '{}',
    schema_digest      TEXT NOT NULL,
    ordinal            INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (snapshot_id) REFERENCES tool_snapshots(id) ON DELETE CASCADE,
    FOREIGN KEY (server_asset_id) REFERENCES assets(id) ON DELETE CASCADE,
    UNIQUE (snapshot_id, backend_name),
    UNIQUE (snapshot_id, public_name)
);

CREATE INDEX idx_snapshots_server_state
    ON tool_snapshots(server_asset_id, state, generation DESC);
CREATE UNIQUE INDEX idx_one_active_snapshot_per_server
    ON tool_snapshots(server_asset_id) WHERE state = 'active';
CREATE INDEX idx_tools_public_name ON tools(public_name);
