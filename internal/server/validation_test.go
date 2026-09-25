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
