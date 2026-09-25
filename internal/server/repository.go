package server

import (
	"context"
	"errors"
	"time"
)

// 领域错误哨兵。SQLite adapter 必须将底层约束/行数语义映射到这些错误。
var (
	// ErrNotFound: 指定 ID 或 namespace/name 的 Server 不存在。
	ErrNotFound = errors.New("server not found")
	// ErrAlreadyExists: namespace/name 唯一约束冲突。
	ErrAlreadyExists = errors.New("server already exists")
	// ErrConflict: 乐观锁失败（expectedRevision 不匹配或行数为 0）。
	ErrConflict = errors.New("revision conflict")
	// ErrNotRunnable: Server 未处于期望运行态（disabled 或 desiredState=stopped），
	// 运行态操作（如刷新 Tool 快照）此时不可执行。
	ErrNotRunnable = errors.New("server is not runnable")
)

// CreateStatusInput 是 UpdateStatus 的输入。
// Status 由 reconciler/health 检查写入，禁止覆盖 Spec；ObservedRevision
// 表达运行态已应用的 Revision，用于判断是否需要 reconcile。
type CreateStatusInput struct {
	Phase               Phase
	Message             string
	ObservedRevision    int64
	LastHealthAt        *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
}

// CreateInput 把当前观测状态转换为 UpdateStatus 的输入，供状态写入方
// （RuntimeManager、Tool 同步）在保留未改动字段的前提下覆盖部分字段。
func (s Status) CreateInput() CreateStatusInput {
	return CreateStatusInput{
		Phase:               s.Phase,
		Message:             s.Message,
		ObservedRevision:    s.ObservedRevision,
		LastHealthAt:        s.LastHealthAt,
		LastSuccessAt:       s.LastSuccessAt,
		ConsecutiveFailures: s.ConsecutiveFailures,
	}
}

// UpdateInput 是 Update 的可变字段集合：PUT 语义下的全量替换。
// namespace/name/transport 不可变（改名即新资产，transport 决定 runtime 矩阵）；
// Enabled 与 DesiredState 有专责写路径（SetEnabled/SetDesiredState），
// 因为它们不改变配置版本，不能递增 Revision。
type UpdateInput struct {
	DisplayName  string
	Description  string
	Labels       map[string]string
	Runtime      RuntimeSpec
	CredentialID *string
	Timeouts     TimeoutSpec
	Limits       LimitSpec
}

// Repository 是 Server 持久化的领域接口。
// 实现位于 adapter 层（internal/storage/sqlite）；本接口不得引入
// SQL、MCP SDK、HTTP 或 Docker SDK 类型。
type Repository interface {
	// Create 持久化新 Server。namespace/name 冲突返回 ErrAlreadyExists。
	Create(ctx context.Context, s Server) error

	// GetByID 按 ID 查询；不存在返回 ErrNotFound。
	GetByID(ctx context.Context, id ID) (Server, error)

	// GetByName 按 namespace/name 查询；不存在返回 ErrNotFound。
	GetByName(ctx context.Context, namespace, name string) (Server, error)

	// ListEnabled 返回所有 enabled=true 的 Server。
	ListEnabled(ctx context.Context) ([]Server, error)

	// List 返回全部 Server，按 namespace、name 排序（稳定且在存储层完成）。
	List(ctx context.Context) ([]Server, error)

	// Update 以 expectedRevision 做乐观锁，全量替换 UpdateInput 描述的可变字段
	// 并递增 Revision（配置版本）。transport 与 desired_state 不在本次写入范围内。
	// 无匹配行（revision 过期或 Server 已删除）返回 ErrConflict。
	Update(ctx context.Context, id ID, expectedRevision int64, in UpdateInput) (Server, error)

	// UpdateStatus 以观测状态覆盖 Status（不触碰 Spec/Revision）。
	// 不存在返回 ErrNotFound。
	UpdateStatus(ctx context.Context, id ID, input CreateStatusInput) error

	// SetEnabled 启用/停用 Server 并返回写入后的快照。
	// enabled 是运行意图而非配置，因此不递增 Revision。不存在返回 ErrNotFound。
	SetEnabled(ctx context.Context, id ID, enabled bool) (Server, error)

	// SetDesiredState 写入期望运行态并返回写入后的快照。
	// desiredState 是运行意图而非配置，因此不递增 Revision。不存在返回 ErrNotFound。
	SetDesiredState(ctx context.Context, id ID, state DesiredState) (Server, error)
}
