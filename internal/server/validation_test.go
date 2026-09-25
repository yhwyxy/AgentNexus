package server

import (
	"testing"
	"time"
)

func validRemoteServer() Server {
	return Server{
		Namespace: "default",
		Name:      "weather",
		Spec: Spec{
			Transport: TransportStreamableHTTP,
			Runtime: RuntimeSpec{
				Type: RuntimeRemote,
				Remote: &RemoteSpec{
					Endpoint: "http://weather-mcp:8080/mcp",
				},
			},
			Timeouts: TimeoutSpec{
				Connect: 5 * time.Second,
				List:    10 * time.Second,
				Call:    60 * time.Second,
			},
			Limits: LimitSpec{
				MaxInFlight: 16,
			},
			DesiredState: DesiredRunning,
		},
	}
}

func TestServerValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Server)
		wantErr bool
	}{
		{
			name: "valid remote server",
		},
		{
			name: "invalid server name",
			mutate: func(s *Server) {
				s.Name = "Weather_Server"
			},
			wantErr: true,
		},
		{
			name: "stdio cannot use remote runtime",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStdio
			},
			wantErr: true,
		},
		{
			name: "remote endpoint requires http",
			mutate: func(s *Server) {
				s.Spec.Runtime.Remote.Endpoint = "ftp://example.com/mcp"
			},
			wantErr: true,
		},
		{
			name: "multiple runtime specs",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process = &ProcessSpec{
					Command: "/usr/bin/demo",
				}
			},
			wantErr: true,
		},
		{
			name: "max in flight too large",
			mutate: func(s *Server) {
				s.Spec.Limits.MaxInFlight = 257
			},
			wantErr: true,
		},
		{
			name: "call timeout too large",
			mutate: func(s *Server) {
				s.Spec.Timeouts.Call = 301 * time.Second
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := validRemoteServer()

			if tt.mutate != nil {
				tt.mutate(&srv)
			}

			err := srv.Validate()

			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func validProcessServer() Server {
	srv := validRemoteServer()
	srv.Spec.Transport = TransportStdio
	srv.Spec.Runtime = RuntimeSpec{
		Type:    RuntimeProcess,
		Process: &ProcessSpec{Command: "/usr/local/bin/demo-mcp"},
	}
	return srv
}

func TestProcessRuntimeValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Server)
		wantErr bool
	}{
		{
			name: "absolute command with env and working dir",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process.Env = map[string]string{"DEMO_TOKEN_FILE": "/run/secrets/demo"}
				s.Spec.Runtime.Process.WorkingDir = "/srv/demo"
			},
		},
		{
			name: "relative command",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process.Command = "demo-mcp"
			},
			wantErr: true,
		},
		{
			name: "relative working dir",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process.WorkingDir = "srv/demo"
			},
			wantErr: true,
		},
		{
			name: "invalid env key",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process.Env = map[string]string{"DEMO-TOKEN": "value"}
			},
			wantErr: true,
		},
		{
			name: "empty env key",
			mutate: func(s *Server) {
				s.Spec.Runtime.Process.Env = map[string]string{"": "value"}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := validProcessServer()
			if tt.mutate != nil {
				tt.mutate(&srv)
			}
			err := srv.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func validDockerServer() Server {
	srv := validRemoteServer()
	srv.Spec.Transport = TransportStdio
	srv.Spec.Runtime = RuntimeSpec{
		Type: RuntimeDocker,
		Docker: &DockerSpec{
			Image:       "ghcr.io/example/fs:1",
			Command:     []string{"fs", "--root", "/workspace"},
			Env:         map[string]string{"LOG_LEVEL": "info"},
			Mounts:      []Mount{{Source: "/srv/sandbox", Target: "/workspace", ReadOnly: true}},
			NetworkMode: "none",
			MemoryBytes: 256 << 20,
			CPUs:        0.5,
		},
	}
	return srv
}

// Validate 只判形状与区间；挂载是否命中允许清单由 HostAccessPolicy 判定。
func TestDockerRuntimeValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Server)
		wantErr bool
	}{
		{
			name: "stdio docker with zero values left to defaults",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.MemoryBytes = 0
				s.Spec.Runtime.Docker.CPUs = 0
			},
		},
		{
			name: "streamable http docker with port and path",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStreamableHTTP
				s.Spec.Runtime.Docker.NetworkMode = ""
				s.Spec.Runtime.Docker.Port = 8080
				s.Spec.Runtime.Docker.EndpointPath = "/mcp"
			},
		},
		{
			name: "empty image",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Image = ""
			},
			wantErr: true,
		},
		{
			name: "image with whitespace",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Image = "ghcr.io/example/fs:1 --privileged"
			},
			wantErr: true,
		},
		{
			name: "empty command argument",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Command = []string{"fs", ""}
			},
			wantErr: true,
		},
		{
			name: "invalid env key",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Env = map[string]string{"LOG-LEVEL": "info"}
			},
			wantErr: true,
		},
		{
			name: "relative mount source",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Mounts = []Mount{{Source: "srv/sandbox", Target: "/workspace"}}
			},
			wantErr: true,
		},
		{
			name: "host root mount",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Mounts = []Mount{{Source: "/", Target: "/workspace"}}
			},
			wantErr: true,
		},
		{
			name: "container root target",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Mounts = []Mount{{Source: "/srv/sandbox", Target: "/"}}
			},
			wantErr: true,
		},
		{
			name: "unsupported network mode",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.NetworkMode = "host"
			},
			wantErr: true,
		},
		{
			name: "memory below the lower bound",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.MemoryBytes = 1 << 20
			},
			wantErr: true,
		},
		{
			name: "memory above the upper bound",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.MemoryBytes = 64 << 30
			},
			wantErr: true,
		},
		{
			name: "cpus above the upper bound",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.CPUs = 16
			},
			wantErr: true,
		},
		{
			name: "stdio with container port",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.Port = 8080
			},
			wantErr: true,
		},
		{
			name: "stdio with endpoint path",
			mutate: func(s *Server) {
				s.Spec.Runtime.Docker.EndpointPath = "/mcp"
			},
			wantErr: true,
		},
		{
			name: "streamable http without port",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStreamableHTTP
				s.Spec.Runtime.Docker.NetworkMode = ""
			},
			wantErr: true,
		},
		{
			name: "streamable http with out of range port",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStreamableHTTP
				s.Spec.Runtime.Docker.NetworkMode = ""
				s.Spec.Runtime.Docker.Port = 70000
			},
			wantErr: true,
		},
		{
			name: "streamable http with relative endpoint path",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStreamableHTTP
				s.Spec.Runtime.Docker.NetworkMode = ""
				s.Spec.Runtime.Docker.Port = 8080
				s.Spec.Runtime.Docker.EndpointPath = "mcp"
			},
			wantErr: true,
		},
		{
			name: "streamable http cannot use network mode none",
			mutate: func(s *Server) {
				s.Spec.Transport = TransportStreamableHTTP
				s.Spec.Runtime.Docker.Port = 8080
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := validDockerServer()
			if tt.mutate != nil {
				tt.mutate(&srv)
			}
			err := srv.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}
