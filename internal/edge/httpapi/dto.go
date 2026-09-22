// 管理 API 的显式 DTO 及其与领域对象的双向映射。领域对象不直接暴露给 HTTP。
package httpapi

import (
	"net/url"
	"time"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

const serverRoute = "/api/v1/mcp-servers"

// registerRequest 对应详细设计 3.2 的 RegisterServerRequest。
// desiredState 不在注册请求中：注册后由 start/stop 端点控制。
type registerRequest struct {
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName"`
	Description  string            `json:"description"`
	Labels       map[string]string `json:"labels"`
	Enabled      *bool             `json:"enabled"`
	Transport    string            `json:"transport"`
	Runtime      runtimeDTO        `json:"runtime"`
	CredentialID *string           `json:"credentialId"`
	Timeouts     *timeoutDTO       `json:"timeouts"`
	Limits       *limitDTO         `json:"limits"`
}

// runtimeDTO 是 RuntimeSpec tagged union 的线上表示；请求与响应共用，
// 只序列化实际存在的变体。
type runtimeDTO struct {
	Type    string      `json:"type"`
	Remote  *remoteDTO  `json:"remote,omitempty"`
	Process *processDTO `json:"process,omitempty"`
	Docker  *dockerDTO  `json:"docker,omitempty"`
}

type remoteDTO struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type processDTO struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
}

type dockerDTO struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Mounts      []mountDTO        `json:"mounts,omitempty"`
	NetworkMode string            `json:"networkMode,omitempty"`
	MemoryBytes int64             `json:"memoryBytes,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
}

type mountDTO struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

// 超时以整秒表示（详细设计 3.2）。
type timeoutDTO struct {
	ConnectSeconds int `json:"connectSeconds"`
	ListSeconds    int `json:"listSeconds"`
	CallSeconds    int `json:"callSeconds"`
}

type limitDTO struct {
	MaxInFlight int `json:"maxInFlight"`
}

type serverResponse struct {
	ID          string            `json:"id"`
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	Enabled     bool              `json:"enabled"`
	Revision    int64             `json:"revision"`
	Spec        specDTO           `json:"spec"`
	Status      statusDTO         `json:"status"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	Links       linksDTO          `json:"links"`
}

type specDTO struct {
	Transport    string     `json:"transport"`
	Runtime      runtimeDTO `json:"runtime"`
	CredentialID *string    `json:"credentialId"`
	Timeouts     timeoutDTO `json:"timeouts"`
	Limits       limitDTO   `json:"limits"`
	DesiredState string     `json:"desiredState"`
}

type statusDTO struct {
	Phase               string     `json:"phase"`
	Message             string     `json:"message"`
	ObservedRevision    int64      `json:"observedRevision"`
	LastHealthAt        *time.Time `json:"lastHealthAt"`
	LastSuccessAt       *time.Time `json:"lastSuccessAt"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
}

type linksDTO struct {
	Self string `json:"self"`
}

func (req registerRequest) toInput() server.RegisterInput {
	in := server.RegisterInput{
		Namespace:    req.Namespace,
		Name:         req.Name,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Labels:       req.Labels,
		Enabled:      req.Enabled,
		Transport:    server.Transport(req.Transport),
		Runtime:      req.Runtime.toSpec(),
		CredentialID: req.CredentialID,
	}
	if req.Timeouts != nil {
		in.Timeouts = server.TimeoutSpec{
			Connect: seconds(req.Timeouts.ConnectSeconds),
			List:    seconds(req.Timeouts.ListSeconds),
			Call:    seconds(req.Timeouts.CallSeconds),
		}
	}
	if req.Limits != nil {
		in.Limits = server.LimitSpec{MaxInFlight: req.Limits.MaxInFlight}
	}
	return in
}

func (d runtimeDTO) toSpec() server.RuntimeSpec {
	spec := server.RuntimeSpec{Type: server.RuntimeType(d.Type)}
	if d.Remote != nil {
		spec.Remote = &server.RemoteSpec{
			Endpoint: d.Remote.Endpoint,
			Headers:  d.Remote.Headers,
		}
	}
	if d.Process != nil {
		spec.Process = &server.ProcessSpec{
			Command:    d.Process.Command,
			Args:       d.Process.Args,
			Env:        d.Process.Env,
			WorkingDir: d.Process.WorkingDir,
		}
	}
	if d.Docker != nil {
		var mounts []server.Mount
		for _, m := range d.Docker.Mounts {
			mounts = append(mounts, server.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
		}
		spec.Docker = &server.DockerSpec{
			Image:       d.Docker.Image,
			Command:     d.Docker.Command,
			Env:         d.Docker.Env,
			Mounts:      mounts,
			NetworkMode: d.Docker.NetworkMode,
			MemoryBytes: d.Docker.MemoryBytes,
			CPUs:        d.Docker.CPUs,
		}
	}
	return spec
}

func toServerResponse(s server.Server) serverResponse {
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	return serverResponse{
		ID:          string(s.ID),
		Namespace:   s.Namespace,
		Name:        s.Name,
		DisplayName: s.DisplayName,
		Description: s.Description,
		Labels:      labels,
		Enabled:     s.Enabled,
		Revision:    s.Revision,
		Spec: specDTO{
			Transport:    string(s.Spec.Transport),
			Runtime:      runtimeFromSpec(s.Spec.Runtime),
			CredentialID: s.Spec.CredentialID,
			Timeouts: timeoutDTO{
				ConnectSeconds: wholeSeconds(s.Spec.Timeouts.Connect),
				ListSeconds:    wholeSeconds(s.Spec.Timeouts.List),
				CallSeconds:    wholeSeconds(s.Spec.Timeouts.Call),
			},
			Limits:       limitDTO{MaxInFlight: s.Spec.Limits.MaxInFlight},
			DesiredState: string(s.Spec.DesiredState),
		},
		Status: statusDTO{
			Phase:               string(s.Status.Phase),
			Message:             s.Status.Message,
			ObservedRevision:    s.Status.ObservedRevision,
			LastHealthAt:        s.Status.LastHealthAt,
			LastSuccessAt:       s.Status.LastSuccessAt,
			ConsecutiveFailures: s.Status.ConsecutiveFailures,
		},
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Links:     linksDTO{Self: selfLink(s.ID)},
	}
}

func runtimeFromSpec(spec server.RuntimeSpec) runtimeDTO {
	d := runtimeDTO{Type: string(spec.Type)}
	if spec.Remote != nil {
		d.Remote = &remoteDTO{Endpoint: spec.Remote.Endpoint, Headers: spec.Remote.Headers}
	}
	if spec.Process != nil {
		d.Process = &processDTO{
			Command:    spec.Process.Command,
			Args:       spec.Process.Args,
			Env:        spec.Process.Env,
			WorkingDir: spec.Process.WorkingDir,
		}
	}
	if spec.Docker != nil {
		var mounts []mountDTO
		for _, m := range spec.Docker.Mounts {
			mounts = append(mounts, mountDTO{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
		}
		d.Docker = &dockerDTO{
			Image:       spec.Docker.Image,
			Command:     spec.Docker.Command,
			Env:         spec.Docker.Env,
			Mounts:      mounts,
			NetworkMode: spec.Docker.NetworkMode,
			MemoryBytes: spec.Docker.MemoryBytes,
			CPUs:        spec.Docker.CPUs,
		}
	}
	return d
}

func selfLink(id server.ID) string {
	return serverRoute + "/" + url.PathEscape(string(id))
}

func seconds(n int) time.Duration {
	return time.Duration(n) * time.Second
}

func wholeSeconds(d time.Duration) int {
	return int(d / time.Second)
}
