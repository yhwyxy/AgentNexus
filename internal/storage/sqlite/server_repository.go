package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
	"modernc.org/sqlite"
)

// ServerRepository 实现 server.Repository，持久化到 SQLite。
type ServerRepository struct {
	db  *sql.DB
	now func() time.Time
}

var _ server.Repository = (*ServerRepository)(nil)

func NewServerRepository(db *sql.DB) *ServerRepository {
	return &ServerRepository{db: db, now: time.Now}
}

func (r *ServerRepository) WithClock(now func() time.Time) *ServerRepository {
	r.now = now
	return r
}

func (r *ServerRepository) Create(ctx context.Context, s server.Server) error {
	if s.ID == "" {
		return errors.New("create server: ID must be assigned by caller")
	}

	if s.Revision < 1 {
		s.Revision = 1
	}

	specJSON, err := json.Marshal(s.Spec.Runtime)
	if err != nil {
		return fmt.Errorf("marshal runtime spec: %w", err)
	}
	labelsJSON, err := json.Marshal(s.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	if s.Labels == nil {
		labelsJSON = []byte("{}")
	}

	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = r.now()
	}

	updatedAt := s.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO assets (id, namespace, name, display_name, description,
				labels_json, enabled, revision, created_at, updated_at, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'MCPServer')`,
		string(s.ID), s.Namespace, s.Name, s.DisplayName, s.Description,
		string(labelsJSON), boolToInt(s.Enabled), s.Revision,
		formatTime(createdAt), formatTime(updatedAt),
	)
	if err != nil {
		if mapped := mapConstraintErr(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("insert asset: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO mcp_servers (asset_id, transport, runtime_type, runtime_spec_json,
			credential_id, connect_timeout_ms, list_timeout_ms, call_timeout_ms,
			max_in_flight, desired_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(s.ID), string(s.Spec.Transport), string(s.Spec.Runtime.Type), string(specJSON),
		nullableString(s.Spec.CredentialID),
		s.Spec.Timeouts.Connect.Milliseconds(),
		s.Spec.Timeouts.List.Milliseconds(),
		s.Spec.Timeouts.Call.Milliseconds(),
		s.Spec.Limits.MaxInFlight,
		string(s.Spec.DesiredState),
	)

	if err != nil {
		if mapped := mapConstraintErr(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("insert mcp_servers: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
			INSERT INTO server_status (asset_id, phase, message, observed_revision,
				consecutive_failures, updated_at)
			VALUES (?, 'pending', '', 0, 0, ?)`,
		string(s.ID), formatTime(r.now()),
	)
	if err != nil {
		return fmt.Errorf("insert server_status: %w", err)
	}

	return tx.Commit()
}

func (r *ServerRepository) GetByID(ctx context.Context, id server.ID) (server.Server, error) {
	return r.get(ctx, r.db, "id = ?", string(id))
}

func (r *ServerRepository) GetByName(ctx context.Context, namespace, name string) (server.Server, error) {
	return r.get(ctx, r.db, "namespace = ? AND name = ?", namespace, name)
}

func (r *ServerRepository) ListEnabled(ctx context.Context) ([]server.Server, error) {
	return r.list(ctx, "a.enabled = 1")
}

func (r *ServerRepository) List(ctx context.Context) ([]server.Server, error) {
	return r.list(ctx, "")
}

// list 是 List/ListEnabled 的公共实现：where 为空时不做过滤。
// 排序在存储层固定，避免调用方各自排序出不同的列表口径。
func (r *ServerRepository) list(ctx context.Context, where string) ([]server.Server, error) {
	query := `
		SELECT a.id, a.namespace, a.name, a.display_name, a.description,
		       a.labels_json, a.enabled, a.revision, a.created_at, a.updated_at,
		       m.transport, m.runtime_type, m.runtime_spec_json, m.credential_id,
		       m.connect_timeout_ms, m.list_timeout_ms, m.call_timeout_ms,
		       m.max_in_flight, m.desired_state,
		       st.phase, st.message, st.observed_revision,
		       st.last_health_at, st.last_success_at, st.consecutive_failures
		FROM assets a
		JOIN mcp_servers m ON m.asset_id = a.id
		JOIN server_status st ON st.asset_id = a.id
		WHERE a.kind = 'MCPServer'`
	if where != "" {
		query += " AND " + where
	}
	query += " ORDER BY a.namespace, a.name"

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()

	var out []server.Server
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Update 以 expectedRevision 做乐观锁，覆盖 UpdateInput 的可变字段并递增 Revision。
// transport 与 desired_state 不在写入列内：前者不可变，后者是运行意图（见 SetDesiredState）。
func (r *ServerRepository) Update(
	ctx context.Context,
	id server.ID,
	expectedRevision int64,
	in server.UpdateInput,
) (server.Server, error) {
	labels := in.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return server.Server{}, fmt.Errorf("marshal labels: %w", err)
	}
	specJSON, err := json.Marshal(in.Runtime)
	if err != nil {
		return server.Server{}, fmt.Errorf("marshal runtime spec: %w", err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return server.Server{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := r.get(ctx, tx, "id = ?", string(id))
	if errors.Is(err, server.ErrNotFound) {
		// 契约：目标行不存在与 revision 过期同样视为冲突。
		return server.Server{}, server.ErrConflict
	}
	if err != nil {
		return server.Server{}, err
	}
	// 预检查只为提前失败；真正的并发保护是下面 UPDATE 的 WHERE revision = ?。
	if current.Revision != expectedRevision {
		return server.Server{}, server.ErrConflict
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE assets
		SET display_name = ?, description = ?, labels_json = ?,
		    revision = revision + 1, updated_at = ?
		WHERE id = ? AND kind = 'MCPServer' AND revision = ?`,
		in.DisplayName, in.Description, string(labelsJSON),
		formatTime(r.now()), string(id), expectedRevision,
	)
	if err != nil {
		return server.Server{}, fmt.Errorf("update asset: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return server.Server{}, fmt.Errorf("update asset: %w", err)
	} else if n == 0 {
		return server.Server{}, server.ErrConflict
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE mcp_servers
		SET runtime_type = ?, runtime_spec_json = ?, credential_id = ?,
		    connect_timeout_ms = ?, list_timeout_ms = ?, call_timeout_ms = ?,
		    max_in_flight = ?
		WHERE asset_id = ?`,
		string(in.Runtime.Type), string(specJSON),
		nullableString(in.CredentialID),
		in.Timeouts.Connect.Milliseconds(),
		in.Timeouts.List.Milliseconds(),
		in.Timeouts.Call.Milliseconds(),
		in.Limits.MaxInFlight,
		string(id),
	)
	if err != nil {
		return server.Server{}, fmt.Errorf("update mcp_servers: %w", err)
	}

	updated, err := r.get(ctx, tx, "id = ?", string(id))
	if err != nil {
		return server.Server{}, err
	}

	if err := tx.Commit(); err != nil {
		return server.Server{}, fmt.Errorf("commit tx: %w", err)
	}
	return updated, nil
}

// UpdateStatus 只写 server_status；assets.updated_at 表示配置变更时间，
// 健康检查等高频状态更新不得把它刷新成噪音。
func (r *ServerRepository) UpdateStatus(ctx context.Context, id server.ID, input server.CreateStatusInput) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE server_status
		SET phase = ?, message = ?, observed_revision = ?,
		    last_health_at = ?, last_success_at = ?, consecutive_failures = ?,
		    updated_at = ?
		WHERE asset_id = ?`,
		string(input.Phase), input.Message, input.ObservedRevision,
		nullableTime(input.LastHealthAt), nullableTime(input.LastSuccessAt),
		input.ConsecutiveFailures, formatTime(r.now()),
		string(id),
	)
	if err != nil {
		return fmt.Errorf("update server_status: %w", err)
	}
	return requireAffected(res, server.ErrNotFound)
}

// SetEnabled 写运行意图 enabled；不递增 revision（启用状态不是配置版本）。
func (r *ServerRepository) SetEnabled(ctx context.Context, id server.ID, enabled bool) (server.Server, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE assets
		SET enabled = ?, updated_at = ?
		WHERE id = ? AND kind = 'MCPServer'`,
		boolToInt(enabled), formatTime(r.now()), string(id),
	)
	if err != nil {
		return server.Server{}, fmt.Errorf("update asset enabled: %w", err)
	}
	if err := requireAffected(res, server.ErrNotFound); err != nil {
		return server.Server{}, err
	}
	// 回读而不是拼装返回值：phase/observed_revision 等观测字段由运行态写入，
	// 返回库里的真实形态才能与紧随其后的 GET 一致。
	return r.GetByID(ctx, id)
}

// SetDesiredState 写期望运行态；同样不递增 revision。
// assets.updated_at 与 SetEnabled 保持一致:两者都是调用方发起的资产变更,
// 都应刷新"记录最后修改时间"(健康检查那类系统心跳才不该碰它)。
func (r *ServerRepository) SetDesiredState(ctx context.Context, id server.ID, state server.DesiredState) (server.Server, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return server.Server{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE assets
		SET updated_at = ?
		WHERE id = ? AND kind = 'MCPServer'`,
		formatTime(r.now()), string(id),
	)
	if err != nil {
		return server.Server{}, fmt.Errorf("update asset updated_at: %w", err)
	}
	if err := requireAffected(res, server.ErrNotFound); err != nil {
		return server.Server{}, err
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE mcp_servers
		SET desired_state = ?
		WHERE asset_id = ?`,
		string(state), string(id),
	)
	if err != nil {
		return server.Server{}, fmt.Errorf("update desired state: %w", err)
	}
	if err := requireAffected(res, server.ErrNotFound); err != nil {
		return server.Server{}, err
	}

	if err := tx.Commit(); err != nil {
		return server.Server{}, fmt.Errorf("commit tx: %w", err)
	}
	return r.GetByID(ctx, id)
}

// rowQuerier 同时适配 *sql.DB 和 *sql.Tx，使单行查询可在事务内外复用。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *ServerRepository) get(ctx context.Context, q rowQuerier, where string, args ...any) (server.Server, error) {
	row := q.QueryRowContext(ctx, `
		SELECT a.id, a.namespace, a.name, a.display_name, a.description,
		       a.labels_json, a.enabled, a.revision, a.created_at, a.updated_at,
		       m.transport, m.runtime_type, m.runtime_spec_json, m.credential_id,
		       m.connect_timeout_ms, m.list_timeout_ms, m.call_timeout_ms,
		       m.max_in_flight, m.desired_state,
		       st.phase, st.message, st.observed_revision,
		       st.last_health_at, st.last_success_at, st.consecutive_failures
		FROM assets a
		JOIN mcp_servers m ON m.asset_id = a.id
		JOIN server_status st ON st.asset_id = a.id
		WHERE a.kind = 'MCPServer' AND `+where, args...)

	s, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return server.Server{}, server.ErrNotFound
	}
	return s, err
}

// scanServer 同时适配 *sql.Row 和 *sql.Rows
func scanServer(row interface{ Scan(...any) error }) (server.Server, error) {
	var (
		s                                       server.Server
		labelsJSON                              string
		enabled                                 int
		createdAtSt, updatedAtSt                string
		transport, runtimeType, runtimeSpecJSON string
		credentialID                            sql.NullString
		connectMS, listMS, callMS, maxInFlight  int64
		desiredState                            string
		phase, message                          string
		observedRevision, consecutiveFailures   int64
		lastHealthAt, lastSuccessAt             sql.NullString
	)
	if err := row.Scan(
		&s.ID, &s.Namespace, &s.Name, &s.DisplayName, &s.Description,
		&labelsJSON, &enabled, &s.Revision, &createdAtSt, &updatedAtSt,
		&transport, &runtimeType, &runtimeSpecJSON, &credentialID,
		&connectMS, &listMS, &callMS, &maxInFlight, &desiredState,
		&phase, &message, &observedRevision,
		&lastHealthAt, &lastSuccessAt, &consecutiveFailures,
	); err != nil {
		return server.Server{}, err
	}

	s.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(labelsJSON), &s.Labels); err != nil {
		return server.Server{}, fmt.Errorf("unmarshal labels: %w", err)
	}
	if err := json.Unmarshal([]byte(runtimeSpecJSON), &s.Spec.Runtime); err != nil {
		return server.Server{}, fmt.Errorf("unmarshal runtime spec: %w", err)
	}

	var err error
	if s.CreatedAt, err = parseTime(createdAtSt); err != nil {
		return server.Server{}, err
	}
	if s.UpdatedAt, err = parseTime(updatedAtSt); err != nil {
		return server.Server{}, err
	}

	s.Spec.Transport = server.Transport(transport)
	s.Spec.Runtime.Type = server.RuntimeType(runtimeType)
	s.Spec.Timeouts = server.TimeoutSpec{
		Connect: time.Duration(connectMS) * time.Millisecond,
		List:    time.Duration(listMS) * time.Millisecond,
		Call:    time.Duration(callMS) * time.Millisecond,
	}
	s.Spec.Limits = server.LimitSpec{MaxInFlight: int(maxInFlight)}
	s.Spec.DesiredState = server.DesiredState(desiredState)
	if credentialID.Valid {
		id := credentialID.String
		s.Spec.CredentialID = &id
	}

	s.Status = server.Status{
		Phase:               server.Phase(phase),
		Message:             message,
		ObservedRevision:    observedRevision,
		ConsecutiveFailures: int(consecutiveFailures),
	}
	if lastHealthAt.Valid {
		t, err := parseTime(lastHealthAt.String)
		if err != nil {
			return server.Server{}, err
		}
		s.Status.LastHealthAt = &t
	}
	if lastSuccessAt.Valid {
		t, err := parseTime(lastSuccessAt.String)
		if err != nil {
			return server.Server{}, err
		}
		s.Status.LastSuccessAt = &t
	}
	return s, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// nullableString 把可选字符串映射为 SQL NULL 或具体值。
func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// nullableTime 把可选时间映射为 SQL NULL 或 RFC3339Nano 字符串。
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// requireAffected 把"UPDATE 未命中任何行"翻译为调用方给定的领域错误。
func requireAffected(res sql.Result, notMatched error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return notMatched
	}
	return nil
}

func mapConstraintErr(err error) error {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return nil
	}
	if sqliteErr.Code() == 2067 {
		return server.ErrAlreadyExists
	}
	return nil
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}
