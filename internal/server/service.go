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

	if srv.Namespace == "" {
		srv.Namespace = defaultNamespace
	}
	// 与持久化后读回的形态一致：labels_json 默认 '{}'，读回为空 map 而非 nil。
	if srv.Labels == nil {
		srv.Labels = map[string]string{}
	}
	if in.Enabled != nil {
		srv.Enabled = *in.Enabled
	}
	if srv.Spec.Timeouts.Connect == 0 {
		srv.Spec.Timeouts.Connect = defaultConnectTimeout
	}
	if srv.Spec.Timeouts.List == 0 {
		srv.Spec.Timeouts.List = defaultListTimeout
	}
	if srv.Spec.Timeouts.Call == 0 {
		srv.Spec.Timeouts.Call = defaultCallTimeout
	}
	if srv.Spec.Limits.MaxInFlight == 0 {
		srv.Spec.Limits.MaxInFlight = defaultMaxInFlight
	}
	if srv.Spec.DesiredState == "" {
		srv.Spec.DesiredState = DesiredRunning
	}
	if docker := srv.Spec.Runtime.Docker; docker != nil {
		if docker.MemoryBytes == 0 {
			docker.MemoryBytes = defaultDockerMemoryBytes
		}
		if docker.CPUs == 0 {
			docker.CPUs = defaultDockerCPUs
		}
		if docker.EndpointPath == "" && srv.Spec.Transport == TransportStreamableHTTP {
			docker.EndpointPath = defaultDockerEndpointPath
		}
	}

	return srv
}
