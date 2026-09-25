package server

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Docker 资源区间是校验层与 HostAccessPolicy 共用的冻结值(v0.1)。
const (
	minDockerMemoryBytes = 16 << 20 // 16 MiB
	maxDockerMemoryBytes = 8 << 30  // 8 GiB
	minDockerCPUs        = 0.1
	maxDockerCPUs        = 8.0
)

// networkModeNone / networkModeBridge 是 v0.1 允许的显式网络模式(空串等同 bridge)。
const (
	networkModeNone   = "none"
	networkModeBridge = "bridge"
)

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (s Server) Validate() error {
	if !namePattern.MatchString(s.Namespace) {
		return fmt.Errorf("invalid namespace %q", s.Namespace)
	}

	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("invalid name %q", s.Name)
	}

	if err := validateRuntime(s.Spec); err != nil {
		return err
	}

	if err := validateTimeouts(s.Spec.Timeouts); err != nil {
		return err
	}

	if s.Spec.Limits.MaxInFlight < 1 || s.Spec.Limits.MaxInFlight > 256 {
		return fmt.Errorf("maxInFlight must be between 1 and 256")
	}

	return nil
}

func validateRuntime(spec Spec) error {
	switch spec.Transport {
	case TransportStreamableHTTP:
		if spec.Runtime.Type != RuntimeRemote && spec.Runtime.Type != RuntimeDocker {
			return fmt.Errorf("streamable_http requires remote or docker runtime")
		}

	case TransportStdio:
		if spec.Runtime.Type != RuntimeProcess && spec.Runtime.Type != RuntimeDocker {
			return fmt.Errorf("stdio requires process or docker runtime")
		}

	default:
		return fmt.Errorf("unsupported transport %q", spec.Transport)
	}

	if err := validateRuntimePayload(spec.Runtime); err != nil {
		return err
	}

	switch spec.Runtime.Type {
	case RuntimeRemote:
		return validateRemote(*spec.Runtime.Remote)

	case RuntimeProcess:
		return validateProcess(*spec.Runtime.Process)

	case RuntimeDocker:
		return validateDocker(spec)

	default:
		return fmt.Errorf("unsupported runtime type %q", spec.Runtime.Type)
	}
}

func validateRuntimePayload(runtime RuntimeSpec) error {
	count := 0
	if runtime.Remote != nil {
		count++
	}
	if runtime.Process != nil {
		count++
	}
	if runtime.Docker != nil {
		count++
	}

	if count != 1 {
		return fmt.Errorf("runtime must contain exactly one runtime spec")
	}

	switch runtime.Type {
	case RuntimeRemote:
		if runtime.Remote == nil {
			return fmt.Errorf("remote runtime requires remote spec")
		}
	case RuntimeProcess:
		if runtime.Process == nil {
			return fmt.Errorf("process runtime requires process spec")
		}
	case RuntimeDocker:
		if runtime.Docker == nil {
			return fmt.Errorf("docker runtime requires docker spec")
		}
	}
	return nil
}

func validateRemote(spec RemoteSpec) error {
	endpoint, err := url.Parse(spec.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid remote endpoint: %w", err)
	}

	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return fmt.Errorf("remote endpoint must use http or https")
	}

	if endpoint.Host == "" {
		return fmt.Errorf("remote endpoint must have a host")
	}

	if endpoint.User != nil {
		return fmt.Errorf("remote endpoint must not contain userinfo")
	}

	return nil
}

func validateProcess(spec ProcessSpec) error {
	if !filepath.IsAbs(spec.Command) {
		return fmt.Errorf("process command must be an absolute path")
	}
	if spec.WorkingDir != "" && !filepath.IsAbs(spec.WorkingDir) {
		return fmt.Errorf("process working directory must be an absolute path")
	}
	for key := range spec.Env {
		if !envNamePattern.MatchString(key) {
			return fmt.Errorf("process env key %q is not a valid environment variable name", key)
		}
	}
	return nil
}

// validateDocker 只检查形状;宿主机挂载是否被允许由 HostAccessPolicy 判定
// (见 internal/runtime/hostaccess),这里不重复允许清单逻辑。
func validateDocker(spec Spec) error {
	docker := spec.Runtime.Docker
	if strings.TrimSpace(docker.Image) == "" || strings.ContainsAny(docker.Image, " \t\n") {
		return fmt.Errorf("docker image must be a single non-empty reference")
	}
	for i, arg := range docker.Command {
		if arg == "" {
			return fmt.Errorf("docker command argument %d must not be empty", i)
		}
	}
	for key := range docker.Env {
		if !envNamePattern.MatchString(key) {
			return fmt.Errorf("docker env key %q is not a valid environment variable name", key)
		}
	}
	for _, mount := range docker.Mounts {
		if !filepath.IsAbs(mount.Source) {
			return fmt.Errorf("docker mount source %q must be an absolute path", mount.Source)
		}
		if filepath.Clean(mount.Source) == string(filepath.Separator) {
			return fmt.Errorf("docker mount source must not be the host root")
		}
		if !filepath.IsAbs(mount.Target) {
			return fmt.Errorf("docker mount target %q must be an absolute path", mount.Target)
		}
		if filepath.Clean(mount.Target) == string(filepath.Separator) {
			return fmt.Errorf("docker mount target must not be the container root")
		}
	}
	switch docker.NetworkMode {
	case "", string(networkModeNone), string(networkModeBridge):
	default:
		return fmt.Errorf("docker network mode %q is not supported", docker.NetworkMode)
	}
	if docker.MemoryBytes != 0 && (docker.MemoryBytes < minDockerMemoryBytes || docker.MemoryBytes > maxDockerMemoryBytes) {
		return fmt.Errorf("docker memoryBytes must be 0 or between %d and %d", minDockerMemoryBytes, maxDockerMemoryBytes)
	}
	if docker.CPUs != 0 && (docker.CPUs < minDockerCPUs || docker.CPUs > maxDockerCPUs) {
		return fmt.Errorf("docker cpus must be 0 or between %g and %g", minDockerCPUs, maxDockerCPUs)
	}

	switch spec.Transport {
	case TransportStreamableHTTP:
		if docker.Port < 1 || docker.Port > 65535 {
			return fmt.Errorf("docker streamable_http runtime requires a container port between 1 and 65535")
		}
		if docker.EndpointPath != "" && !strings.HasPrefix(docker.EndpointPath, "/") {
			return fmt.Errorf("docker endpoint path must start with /")
		}
		if docker.NetworkMode == string(networkModeNone) {
			return fmt.Errorf("docker streamable_http runtime can not publish a port with network mode none")
		}
	case TransportStdio:
		if docker.Port != 0 {
			return fmt.Errorf("docker stdio runtime must not set a container port")
		}
		if docker.EndpointPath != "" {
			return fmt.Errorf("docker stdio runtime must not set an endpoint path")
		}
	}

	return nil
}

func validateTimeouts(spec TimeoutSpec) error {
	if spec.Connect <= 0 || spec.Connect > 30*time.Second {
		return fmt.Errorf("connect timeout must be between 1ns and 30s")
	}

	if spec.List <= 0 || spec.List > 60*time.Second {
		return fmt.Errorf("list timeout must be between 1ns and 60s")
	}

	if spec.Call <= 0 || spec.Call > 300*time.Second {
		return fmt.Errorf("call timeout must be between 1ns and 300s")
	}
	return nil
}
