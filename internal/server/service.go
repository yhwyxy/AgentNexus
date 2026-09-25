// 负责 MCP Server 注册的应用编排：defaulting → 构造 → 校验 → 持久化。
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalid: 注册输入未通过领域校验；调用方可据此与存储错误区分。
var ErrInvalid = errors.New("invalid server")

// v0.1 冻结默认值（详细设计 3.3）。Validate 只检查不修改，默认值只在这里填充。
const (
	defaultNamespace      = "default"
	defaultConnectTimeout = 5 * time.Second
	defaultListTimeout    = 10 * time.Second
	defaultCallTimeout    = 60 * time.Second
	defaultMaxInFlight    = 16

	// Docker 运行时的 v0.1 冻结默认值；区间校验在 Validate(validation.go)。
	defaultDockerMemoryBytes  = 256 << 20 // 256 MiB
	defaultDockerCPUs         = 1.0
	defaultDockerEndpointPath = "/mcp"
)

// RegisterInput 是 Service.Register 的输入。零值字段按冻结默认值填充；
// Enabled 使用指针以区分"未提供"（默认 true）与显式 false。
type RegisterInput struct {
	Namespace    string
	Name         string
	DisplayName  string
	Description  string
	Labels       map[string]string
	Enabled      *bool
	Transport    Transport
	Runtime      RuntimeSpec
	CredentialID *string
	Timeouts     TimeoutSpec
	Limits       LimitSpec
	DesiredState DesiredState
}

// Service 是 MCP Server 注册的应用服务。它只依赖 Repository 接口，
// 不接触 SQL、MCP SDK、HTTP 或 Docker。
type Service struct {
	repo   Repository
	policy SpecPolicy
	newID  func() string
	now    func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, newID: uuid.NewString, now: time.Now}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func (s *Service) WithIDGenerator(newID func() string) *Service {
	s.newID = newID
	return s
}

// Register 填充默认值、分配 ID 与时间戳、校验并持久化新 Server。
// 返回值与持久化结果一致：Revision=1，Status 为 pending。
// 校验失败返回包装 ErrInvalid 的错误；namespace/name 冲突透传 ErrAlreadyExists。
func (s *Service) Register(ctx context.Context, in RegisterInput) (Server, error) {
	srv := s.newServer(in)

	if err := srv.Validate(); err != nil {
		return Server{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	// 宿主机资源策略(挂载允许清单、环境变量形态)只在配置里存在,
	// 因此单独注入;违规与形状错误同样归类为 ErrInvalid(HTTP 400)。
	if err := s.checkPolicy(srv.Spec); err != nil {
		return Server{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if err := s.repo.Create(ctx, srv); err != nil {
		return Server{}, fmt.Errorf("create server: %w", err)
	}

	return srv, nil
}

// SpecPolicy 是宿主机资源访问策略的消费方接口:由配置构造(见
// internal/runtime/hostaccess),在注册时判定 Docker 运行时的挂载与
// 环境变量是否被允许。实现在 Ensure 期还会再判一次。
type SpecPolicy interface {
	CheckDocker(DockerSpec) error
}

// WithPolicy 注入宿主机资源策略;未注入时不做策略判定(保持既有行为)。
func (s *Service) WithPolicy(policy SpecPolicy) *Service {
	s.policy = policy
	return s
}

func (s *Service) checkPolicy(spec Spec) error {
	if s.policy == nil || spec.Runtime.Type != RuntimeDocker || spec.Runtime.Docker == nil {
		return nil
	}
	return s.policy.CheckDocker(*spec.Runtime.Docker)
}

// Get 按 ID 查询 Server；不存在返回 ErrNotFound。
func (s *Service) Get(ctx context.Context, id ID) (Server, error) {
	return s.repo.GetByID(ctx, id)
}

// List 返回全部 Server，排序由存储层保证（namespace, name）。
func (s *Service) List(ctx context.Context) ([]Server, error) {
	return s.repo.List(ctx)
}

// Update 以 expectedRevision 乐观锁全量替换 Server 的可变配置：元数据（displayName/
// description/labels）、runtime、credential 与超时/限流。namespace/name/transport 不可变，
// Enabled 与 DesiredState 不走这条路径。
//
// 语义与 Register 对齐：零值字段回落到冻结默认值（超时、maxInFlight、Docker 内存/CPU/
// endpointPath），校验与宿主机策略判定在写入前完成。配置变更递增 Revision（配置版本），
// 运行态收敛由调用方在成功后触发。
// revision 过期返回 ErrConflict；id 不存在返回 ErrNotFound；校验失败返回 ErrInvalid。
func (s *Service) Update(ctx context.Context, id ID, expectedRevision int64, in UpdateInput) (Server, error) {
	current, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Server{}, err
	}

	next := current
	next.DisplayName = in.DisplayName
	next.Description = in.Description
	next.Labels = in.Labels
	next.Spec.Runtime = in.Runtime
	next.Spec.CredentialID = in.CredentialID
	next.Spec.Timeouts = in.Timeouts
	next.Spec.Limits = in.Limits
	// updated_at 不由服务层决定：写入时钟属于仓储，返回的快照来自仓储回读。
	// 默认值只在 applyDefaults 一处填充；此处保留 current 的 DesiredState/Transport。
	applyDefaults(&next)

	if err := next.Validate(); err != nil {
		return Server{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := s.checkPolicy(next.Spec); err != nil {
		return Server{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	updated, err := s.repo.Update(ctx, id, expectedRevision, UpdateInput{
		DisplayName:  next.DisplayName,
		Description:  next.Description,
		Labels:       next.Labels,
		Runtime:      next.Spec.Runtime,
		CredentialID: next.Spec.CredentialID,
		Timeouts:     next.Spec.Timeouts,
		Limits:       next.Spec.Limits,
	})
	if err != nil {
		return Server{}, err
	}

	return updated, nil
}

// SetEnabled 启用/停用 Server 并返回写入后的快照。
// 幂等：值未变化时不写库，直接返回当前快照。enabled 不改变配置版本。
func (s *Service) SetEnabled(ctx context.Context, id ID, enabled bool) (Server, error) {
	return s.setState(ctx, id, func(current Server) bool { return current.Enabled == enabled },
		func(current Server) (Server, error) { return s.repo.SetEnabled(ctx, id, enabled) })
}

// SetDesiredState 写入期望运行态并返回写入后的快照。
// 幂等：值未变化时不写库，直接返回当前快照。desiredState 不改变配置版本。
func (s *Service) SetDesiredState(ctx context.Context, id ID, state DesiredState) (Server, error) {
	return s.setState(ctx, id, func(current Server) bool { return current.Spec.DesiredState == state },
		func(current Server) (Server, error) { return s.repo.SetDesiredState(ctx, id, state) })
}

// setState 收敛两个运行意图写路径的公共形状：先读当前快照判断是否已是目标值，
// 是则跳过写入；否则执行写入并返回新快照。
func (s *Service) setState(ctx context.Context, id ID, settled func(Server) bool,
	write func(Server) (Server, error)) (Server, error) {
	current, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Server{}, err
	}
	if settled(current) {
		return current, nil
	}

	return write(current)
}

func (s *Service) newServer(in RegisterInput) Server {
	// 与存储层的 UTC 表示一致，避免注册响应与后续查询渲染出不同时区。
	now := s.now().UTC()

	srv := Server{
		ID:          ID(s.newID()),
		Namespace:   in.Namespace,
		Name:        in.Name,
		DisplayName: in.DisplayName,
		Description: in.Description,
		Labels:      in.Labels,
		Enabled:     true,
		Revision:    1,
		Spec: Spec{
			Transport:    in.Transport,
			Runtime:      in.Runtime,
			CredentialID: in.CredentialID,
			Timeouts:     in.Timeouts,
			Limits:       in.Limits,
			DesiredState: in.DesiredState,
		},
		Status:    Status{Phase: PhasePending},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if in.Enabled != nil {
		srv.Enabled = *in.Enabled
	}
	applyDefaults(&srv)

	return srv
}

// applyDefaults 是 v0.1 冻结默认值的唯一填充点：Register 与 Update 共用，
// 避免出现第二套默认值。入参必须已是完整的 Server（Update 传入的是当前快照的副本）。
func applyDefaults(srv *Server) {
	if srv.Namespace == "" {
		srv.Namespace = defaultNamespace
	}
	// 与持久化后读回的形态一致：labels_json 默认 '{}'，读回为空 map 而非 nil。
	if srv.Labels == nil {
		srv.Labels = map[string]string{}
	}
	applySpecDefaults(&srv.Spec)
}

// applySpecDefaults 填充 Spec 的默认值。DesiredState 为空时回落到 running：
// 注册时未表态即视为期望运行，而更新路径传入的当前快照必有非空值。
func applySpecDefaults(spec *Spec) {
	if spec.Timeouts.Connect == 0 {
		spec.Timeouts.Connect = defaultConnectTimeout
	}
	if spec.Timeouts.List == 0 {
		spec.Timeouts.List = defaultListTimeout
	}
	if spec.Timeouts.Call == 0 {
		spec.Timeouts.Call = defaultCallTimeout
	}
	if spec.Limits.MaxInFlight == 0 {
		spec.Limits.MaxInFlight = defaultMaxInFlight
	}
	if spec.DesiredState == "" {
		spec.DesiredState = DesiredRunning
	}
	docker := spec.Runtime.Docker
	if docker == nil {
		return
	}
	if docker.MemoryBytes == 0 {
		docker.MemoryBytes = defaultDockerMemoryBytes
	}
	if docker.CPUs == 0 {
		docker.CPUs = defaultDockerCPUs
	}
	if docker.EndpointPath == "" && spec.Transport == TransportStreamableHTTP {
		docker.EndpointPath = defaultDockerEndpointPath
	}
}
