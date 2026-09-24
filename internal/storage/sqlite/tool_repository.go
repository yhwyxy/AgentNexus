package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/tool"
)

type ToolRepository struct {
	db  *sql.DB
	now func() time.Time
}

var _ tool.Repository = (*ToolRepository)(nil)

func NewToolRepository(db *sql.DB) *ToolRepository {
	return &ToolRepository{db: db, now: time.Now}
}

func (r *ToolRepository) WithClock(now func() time.Time) *ToolRepository {
	r.now = now
	return r
}

func (r *ToolRepository) ReplaceSnapshot(ctx context.Context, replacement tool.SnapshotReplacement) (tool.Snapshot, error) {
	if replacement.ID == "" || replacement.ServerID == "" || replacement.Revision < 1 {
		return tool.Snapshot{}, fmt.Errorf("replace tool snapshot: ID, server ID, and positive revision are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return tool.Snapshot{}, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var revision int64
	var enabled int
	var desired string
	if err := tx.QueryRowContext(ctx, `
		SELECT a.revision, a.enabled, m.desired_state
		FROM assets a JOIN mcp_servers m ON m.asset_id = a.id
		WHERE a.id = ?`, string(replacement.ServerID)).Scan(&revision, &enabled, &desired); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tool.Snapshot{}, server.ErrNotFound
		}
		return tool.Snapshot{}, fmt.Errorf("load server state for snapshot: %w", err)
	}
	if revision != replacement.Revision || enabled != 1 || desired != string(server.DesiredRunning) {
		return tool.Snapshot{}, server.ErrConflict
	}

	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM tool_snapshots WHERE server_asset_id = ?`, string(replacement.ServerID)).Scan(&generation); err != nil {
		return tool.Snapshot{}, fmt.Errorf("next snapshot generation: %w", err)
	}
	now := r.now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tool_snapshots (id, server_asset_id, server_revision, generation, state,
			tool_count, catalog_digest, created_at)
		VALUES (?, ?, ?, ?, 'building', ?, ?, ?)`, replacement.ID, string(replacement.ServerID),
		replacement.Revision, generation, len(replacement.Tools), replacement.CatalogDigest, formatTime(now))
	if err != nil {
		return tool.Snapshot{}, fmt.Errorf("insert tool snapshot: %w", err)
	}

	for ordinal, definition := range replacement.Tools {
		if definition.ID == "" || definition.ServerID != replacement.ServerID {
			return tool.Snapshot{}, fmt.Errorf("insert tool snapshot: tool ID and matching server ID are required")
		}
		var output any
		if len(definition.OutputSchema) != 0 {
			output = string(definition.OutputSchema)
		}
		annotations := definition.Annotations
		if len(annotations) == 0 {
			annotations = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tools (id, snapshot_id, server_asset_id, backend_name, public_name,
				title, description, input_schema_json, output_schema_json, annotations_json,
				schema_digest, ordinal)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, definition.ID, replacement.ID,
			string(replacement.ServerID), definition.BackendName, definition.PublicName,
			definition.Title, definition.Description, string(definition.InputSchema), output,
			string(annotations), definition.SchemaDigest, ordinal)
		if err != nil {
			return tool.Snapshot{}, fmt.Errorf("insert tool %q: %w", definition.BackendName, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE tool_snapshots SET state = 'superseded' WHERE server_asset_id = ? AND state = 'active'`, string(replacement.ServerID)); err != nil {
		return tool.Snapshot{}, fmt.Errorf("supersede active snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tool_snapshots SET state = 'active', activated_at = ? WHERE id = ?`, formatTime(now), replacement.ID); err != nil {
		return tool.Snapshot{}, fmt.Errorf("activate tool snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tool.Snapshot{}, fmt.Errorf("commit tool snapshot: %w", err)
	}
	return tool.Snapshot{
		ID: replacement.ID, ServerID: replacement.ServerID, Revision: replacement.Revision,
		Generation: generation, State: tool.SnapshotActive, ToolCount: len(replacement.Tools),
		CatalogDigest: replacement.CatalogDigest, CreatedAt: now, ActivatedAt: &now,
		Tools: append([]tool.Definition(nil), replacement.Tools...),
	}, nil
}

func (r *ToolRepository) GetActiveSnapshot(ctx context.Context, serverID server.ID) (tool.Snapshot, error) {
	var snapshot tool.Snapshot
	var id, state, createdAt string
	var activated sql.NullString
	if err := r.db.QueryRowContext(ctx, `
		SELECT id, server_asset_id, server_revision, generation, state, tool_count,
			catalog_digest, created_at, activated_at
		FROM tool_snapshots WHERE server_asset_id = ? AND state = 'active'`, string(serverID)).Scan(
		&id, &snapshot.ServerID, &snapshot.Revision, &snapshot.Generation, &state,
		&snapshot.ToolCount, &snapshot.CatalogDigest, &createdAt, &activated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tool.Snapshot{}, tool.ErrSnapshotNotFound
		}
		return tool.Snapshot{}, fmt.Errorf("get active tool snapshot: %w", err)
	}
	snapshot.ID = id
	snapshot.State = tool.SnapshotState(state)
	created, err := parseTime(createdAt)
	if err != nil {
		return tool.Snapshot{}, err
	}
	snapshot.CreatedAt = created
	if activated.Valid {
		t, err := parseTime(activated.String)
		if err != nil {
			return tool.Snapshot{}, err
		}
		snapshot.ActivatedAt = &t
	}
	tools, err := r.loadSnapshotTools(ctx, id, snapshot.ServerID)
	if err != nil {
		return tool.Snapshot{}, err
	}
	snapshot.Tools = tools
	return snapshot, nil
}

func (r *ToolRepository) loadSnapshotTools(ctx context.Context, snapshotID string, serverID server.ID) ([]tool.Definition, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.name, t.id, t.backend_name, t.public_name, t.title, t.description,
			t.input_schema_json, t.output_schema_json, t.annotations_json, t.schema_digest
		FROM tools t JOIN assets a ON a.id = t.server_asset_id
		WHERE t.snapshot_id = ? ORDER BY t.ordinal, t.public_name`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("list snapshot tools: %w", err)
	}
	defer rows.Close()
	var tools []tool.Definition
	for rows.Next() {
		var d tool.Definition
		var input, annotations string
		var output sql.NullString
		if err := rows.Scan(&d.ServerName, &d.ID, &d.BackendName, &d.PublicName, &d.Title,
			&d.Description, &input, &output, &annotations, &d.SchemaDigest); err != nil {
			return nil, fmt.Errorf("scan snapshot tool: %w", err)
		}
		d.ServerID = serverID
		d.InputSchema = []byte(input)
		if output.Valid {
			d.OutputSchema = []byte(output.String)
		}
		d.Annotations = []byte(annotations)
		tools = append(tools, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshot tools: %w", err)
	}
	return tools, nil
}

func (r *ToolRepository) ListAggregated(ctx context.Context) ([]tool.Definition, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.name, t.id, t.backend_name, t.public_name, t.title, t.description,
			t.input_schema_json, t.output_schema_json, t.annotations_json, t.schema_digest
		FROM tools t
		JOIN tool_snapshots s ON s.id = t.snapshot_id AND s.state = 'active'
		JOIN assets a ON a.id = t.server_asset_id AND a.revision = s.server_revision AND a.enabled = 1
		JOIN mcp_servers m ON m.asset_id = a.id AND m.desired_state = 'running'
		ORDER BY t.public_name`)
	if err != nil {
		return nil, fmt.Errorf("list aggregated tools: %w", err)
	}
	defer rows.Close()
	var definitions []tool.Definition
	for rows.Next() {
		var d tool.Definition
		var serverID, input, annotations string
		var output sql.NullString
		if err := rows.Scan(&serverID, &d.ServerName, &d.ID, &d.BackendName, &d.PublicName,
			&d.Title, &d.Description, &input, &output, &annotations, &d.SchemaDigest); err != nil {
			return nil, fmt.Errorf("scan aggregated tool: %w", err)
		}
		d.ServerID = server.ID(serverID)
		d.InputSchema = []byte(input)
		if output.Valid {
			d.OutputSchema = []byte(output.String)
		}
		d.Annotations = []byte(annotations)
		definitions = append(definitions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregated tools: %w", err)
	}
	return definitions, nil
}

func (r *ToolRepository) ResolvePublicName(ctx context.Context, publicName string) (tool.Route, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.name, t.backend_name, t.public_name, s.id
		FROM tools t
		JOIN tool_snapshots s ON s.id = t.snapshot_id AND s.state = 'active'
		JOIN assets a ON a.id = t.server_asset_id AND a.revision = s.server_revision AND a.enabled = 1
		JOIN mcp_servers m ON m.asset_id = a.id AND m.desired_state = 'running'
		WHERE t.public_name = ? LIMIT 2`, publicName)
	if err != nil {
		return tool.Route{}, fmt.Errorf("resolve tool route: %w", err)
	}
	defer rows.Close()
	var route tool.Route
	var serverID, snapshotID string
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return tool.Route{}, fmt.Errorf("read tool route: %w", err)
		}
		return tool.Route{}, tool.ErrToolNotFound
	}
	if err := rows.Scan(&serverID, &route.ServerName, &route.BackendName, &route.PublicName, &snapshotID); err != nil {
		return tool.Route{}, fmt.Errorf("scan tool route: %w", err)
	}
	if rows.Next() {
		return tool.Route{}, tool.ErrCatalogConflict
	}
	if err := rows.Err(); err != nil {
		return tool.Route{}, fmt.Errorf("read tool route: %w", err)
	}
	route.ServerID, route.SnapshotID = server.ID(serverID), snapshotID
	return route, nil
}
