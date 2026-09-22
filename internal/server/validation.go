package server

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

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
		if spec.Runtime.Docker.Image == "" {
			return fmt.Errorf("docker image must not be empty")
		}
		return nil

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
