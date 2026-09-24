// 定义 AgentNexus 内部对 MCP Server 的核心表示
package server

import "time"

type ID string

type Transport string

const (
	TransportStreamableHTTP Transport = "streamable_http"
	TransportStdio          Transport = "stdio"
)

type RuntimeType string

const (
	RuntimeRemote  RuntimeType = "remote"
	RuntimeProcess RuntimeType = "process"
	RuntimeDocker  RuntimeType = "docker"
)

type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

type Phase string

const (
	PhasePending  Phase = "pending"
	PhaseStarting Phase = "starting"
	PhaseReady    Phase = "ready"
	PhaseDegraded Phase = "degraded"
	PhaseStopped  Phase = "stopped"
	PhaseFailed   Phase = "failed"
)

type Server struct {
	ID          ID
	Namespace   string
	Name        string
	DisplayName string
	Description string
	Labels      map[string]string
	Enabled     bool
	Revision    int64

	Spec   Spec
	Status Status

	CreatedAt time.Time
	UpdatedAt time.Time
}

type Spec struct {
	Transport    Transport
	Runtime      RuntimeSpec
	CredentialID *string
	Timeouts     TimeoutSpec
	Limits       LimitSpec
	DesiredState DesiredState
}

type RuntimeSpec struct {
	Type    RuntimeType
	Remote  *RemoteSpec
	Process *ProcessSpec
	Docker  *DockerSpec
}

type RemoteSpec struct {
	Endpoint string
	Headers  map[string]string
}

type ProcessSpec struct {
	Command    string
	Args       []string
	Env        map[string]string
	WorkingDir string
}

type DockerSpec struct {
	Image       string
	Command     []string
	Env         map[string]string
	Mounts      []Mount
	NetworkMode string
	MemoryBytes int64
	CPUs        float64
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type TimeoutSpec struct {
	Connect time.Duration
	List    time.Duration
	Call    time.Duration
}

type LimitSpec struct {
	MaxInFlight int
}

// Runnable 表示该 Server 期望有运行态实例：enabled 且 desiredState=running。
// 运行态工作（启动、Tool 同步、刷新）只对 Runnable 的 Server 有意义，
// 该谓词是这一判定的唯一定义处。
func (s Server) Runnable() bool {
	return s.Enabled && s.Spec.DesiredState == DesiredRunning
}

type Status struct {
	Phase               Phase
	Message             string
	ObservedRevision    int64
	LastHealthAt        *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
}
