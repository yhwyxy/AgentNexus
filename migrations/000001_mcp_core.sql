CREATE TABLE credentials (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL
        CHECK (type IN ('bearer', 'basic', 'headers')),
    secret_ref  TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE assets (
    id           TEXT PRIMARY KEY,
    api_version  TEXT NOT NULL DEFAULT 'agentnexus.io/v1alpha1',
    kind         TEXT NOT NULL
        CHECK (kind IN ('MCPServer')),
    namespace    TEXT NOT NULL,
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    labels_json  TEXT NOT NULL DEFAULT '{}',
    enabled      INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    revision     INTEGER NOT NULL DEFAULT 1
        CHECK (revision > 0),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,

    UNIQUE (namespace, kind, name)
);

CREATE TABLE mcp_servers (
    asset_id            TEXT PRIMARY KEY,
    transport           TEXT NOT NULL
        CHECK (transport IN ('streamable_http', 'stdio')),
    runtime_type        TEXT NOT NULL
        CHECK (runtime_type IN ('remote', 'process', 'docker')),
    runtime_spec_json   TEXT NOT NULL,
    credential_id       TEXT NULL,
    connect_timeout_ms  INTEGER NOT NULL DEFAULT 5000
        CHECK (connect_timeout_ms BETWEEN 1 AND 30000),
    list_timeout_ms     INTEGER NOT NULL DEFAULT 10000
        CHECK (list_timeout_ms BETWEEN 1 AND 60000),
    call_timeout_ms     INTEGER NOT NULL DEFAULT 60000
        CHECK (call_timeout_ms BETWEEN 1 AND 300000),
    max_in_flight       INTEGER NOT NULL DEFAULT 16
        CHECK (max_in_flight BETWEEN 1 AND 256),
    desired_state       TEXT NOT NULL DEFAULT 'running'
        CHECK (desired_state IN ('running', 'stopped')),

    FOREIGN KEY (asset_id)
        REFERENCES assets(id)
        ON DELETE CASCADE,

    FOREIGN KEY (credential_id)
        REFERENCES credentials(id)
        ON DELETE RESTRICT
);

CREATE TABLE server_status (
    asset_id             TEXT PRIMARY KEY,
    phase                TEXT NOT NULL DEFAULT 'pending'
        CHECK (
            phase IN (
                'pending',
                'starting',
                'ready',
                'degraded',
                'stopped',
                'failed'
            )
        ),
    message              TEXT NOT NULL DEFAULT '',
    observed_revision    INTEGER NOT NULL DEFAULT 0,
    last_health_at       TEXT NULL,
    last_success_at      TEXT NULL,
    consecutive_failures INTEGER NOT NULL DEFAULT 0
        CHECK (consecutive_failures >= 0),
    updated_at           TEXT NOT NULL,

    FOREIGN KEY (asset_id)
        REFERENCES assets(id)
        ON DELETE CASCADE
);

CREATE INDEX idx_assets_enabled_kind
    ON assets(enabled, kind, namespace, name);
