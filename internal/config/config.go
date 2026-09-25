// 定义 AgentNexus 配置结构、默认值，并加载 Server 配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	defaultConfigPath        = "configs/agentnexus.yaml"
	defaultHTTPAddress       = ":8080"
	defaultShutdownTimeout   = 15 * time.Second
	defaultDatabasePath      = "data/agentnexus.db"
	defaultReconcileInterval = 30 * time.Second

	defaultDockerSocket      = "unix:///var/run/docker.sock"
	defaultDockerConnectHost = "127.0.0.1"
	defaultDockerPublishHost = "127.0.0.1"
	defaultDockerStopGrace   = 5 * time.Second
)

// defaultDockerHost 优先采用标准的 DOCKER_HOST 环境变量，再退化到本机 socket。
func defaultDockerHost() string {
	if value := os.Getenv("DOCKER_HOST"); value != "" {
		return value
	}
	return defaultDockerSocket
}

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	Security SecurityConfig `yaml:"security"`
}

// SecurityConfig 描述静态 API Key 认证（详细设计 §13）。空列表合法：服务照常启动，
// 但除公开探针外全部请求都会被拒绝。
type SecurityConfig struct {
	APIKeys    []APIKeyConfig   `yaml:"apiKeys"`
	HostAccess HostAccessConfig `yaml:"hostAccess"`
}

// HostAccessConfig 是架构设计 §7.3 的宿主机挂载允许清单。空清单合法但意味着
// 任何 docker 挂载都会被拒绝。
type HostAccessConfig struct {
	Mounts []HostMountConfig `yaml:"mounts"`
}

// HostMountConfig 是一条宿主目录映射；Access 只接受 "ro" / "rw"，空值按 "ro" 处理。
type HostMountConfig struct {
	Host      string `yaml:"host"`
	Container string `yaml:"container"`
	Access    string `yaml:"access"`
}

const (
	AccessReadOnly  = "ro"
	AccessReadWrite = "rw"
)

// APIKeyConfig 是一条已解析的密钥。Role 只在此处保持字符串形态，
// 语义校验（合法角色、名称/密钥唯一）在 auth.NewAuthorizer 里完成。
type APIKeyConfig struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Secret string `yaml:"-"`
}

type ServerConfig struct {
	HTTPAddress     string        `yaml:"httpAddress"`
	ShutdownTimeout time.Duration `yaml:"-"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// RuntimeConfig 控制运行态收敛（详细设计 §15）：
// 周期巡检在启动重放之后按 ReconcileInterval 重试未收敛的 Server，0 表示关闭周期行为。
type RuntimeConfig struct {
	ReconcileInterval time.Duration `yaml:"-"`
	Docker            DockerConfig  `yaml:"docker"`
}

// DockerConfig 描述如何连接本机（或 Compose 内）的 Docker daemon，
// 以及发布容器端口时的绑定地址。
type DockerConfig struct {
	// Host 是 daemon 端点：unix:///var/run/docker.sock 或 tcp://host:port。
	Host string
	// ConnectHost 是 AgentNexus 访问已发布端口时使用的宿主地址。
	ConnectHost string
	// PublishHost 是容器端口发布时绑定的宿主 IP，只发布到回环或指定网卡。
	PublishHost string
	// StopGrace 是 docker stop 的宽限期。
	StopGrace time.Duration
}

type rawConfig struct {
	Server   rawServerConfig   `yaml:"server"`
	Database rawDatabaseConfig `yaml:"database"`
	Runtime  rawRuntimeConfig  `yaml:"runtime"`
	Security rawSecurityConfig `yaml:"security"`
}

type rawSecurityConfig struct {
	APIKeys    []rawAPIKeyConfig   `yaml:"apiKeys"`
	HostAccess rawHostAccessConfig `yaml:"hostAccess"`
}

// rawAPIKeyConfig 故意带 key/keyEnv 两个字段：互斥校验必须在解析层完成，
// 因为它们决定 Secret 从哪来（字面量还是环境变量）。
type rawAPIKeyConfig struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Key    string `yaml:"key"`
	KeyEnv string `yaml:"keyEnv"`
}

type rawRuntimeConfig struct {
	ReconcileInterval string          `yaml:"reconcileInterval"`
	Docker            rawDockerConfig `yaml:"docker"`
}

type rawDockerConfig struct {
	Host        string `yaml:"host"`
	ConnectHost string `yaml:"connectHost"`
	PublishHost string `yaml:"publishHost"`
	StopGrace   string `yaml:"stopGrace"`
}

type rawHostAccessConfig struct {
	Mounts []rawHostMountConfig `yaml:"mounts"`
}

type rawHostMountConfig struct {
	Host      string `yaml:"host"`
	Container string `yaml:"container"`
	Access    string `yaml:"access"`
}

type rawDatabaseConfig struct {
	Path string `yaml:"path"`
}

type rawServerConfig struct {
	HTTPAddress     string `yaml:"httpAddress"`
	ShutdownTimeout string `yaml:"shutdownTimeout"`
}

func Load(path string) (Config, error) {
	cfg := defaultConfig()

	if path == "" {
		path = defaultConfigPath
	}

	if err := loadYAML(path, &cfg); err != nil {
		return Config{}, err
	}

	if err := loadEnvironment(&cfg); err != nil {
		return Config{}, err
	}

	if err := validate(cfg); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			HTTPAddress:     defaultHTTPAddress,
			ShutdownTimeout: defaultShutdownTimeout,
		},
		Database: DatabaseConfig{
			Path: defaultDatabasePath,
		},
		Runtime: RuntimeConfig{
			ReconcileInterval: defaultReconcileInterval,
			Docker: DockerConfig{
				Host:        defaultDockerHost(),
				ConnectHost: defaultDockerConnectHost,
				PublishHost: defaultDockerPublishHost,
				StopGrace:   defaultDockerStopGrace,
			},
		},
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("read config file %q: %w", path, err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}

	if raw.Server.HTTPAddress != "" {
		cfg.Server.HTTPAddress = raw.Server.HTTPAddress
	}

	if raw.Server.ShutdownTimeout != "" {
		timeout, err := time.ParseDuration(raw.Server.ShutdownTimeout)
		if err != nil {
			return fmt.Errorf("parse server.shutdownTimeout: %w", err)
		}

		cfg.Server.ShutdownTimeout = timeout
	}

	if raw.Database.Path != "" {
		cfg.Database.Path = raw.Database.Path
	}

	if raw.Runtime.ReconcileInterval != "" {
		interval, err := time.ParseDuration(raw.Runtime.ReconcileInterval)
		if err != nil {
			return fmt.Errorf("parse runtime.reconcileInterval: %w", err)
		}

		cfg.Runtime.ReconcileInterval = interval
	}

	if raw.Runtime.Docker.Host != "" {
		cfg.Runtime.Docker.Host = raw.Runtime.Docker.Host
	}

	if raw.Runtime.Docker.ConnectHost != "" {
		cfg.Runtime.Docker.ConnectHost = raw.Runtime.Docker.ConnectHost
	}

	if raw.Runtime.Docker.PublishHost != "" {
		cfg.Runtime.Docker.PublishHost = raw.Runtime.Docker.PublishHost
	}

	if raw.Runtime.Docker.StopGrace != "" {
		stopGrace, err := time.ParseDuration(raw.Runtime.Docker.StopGrace)
		if err != nil {
			return fmt.Errorf("parse runtime.docker.stopGrace: %w", err)
		}

		cfg.Runtime.Docker.StopGrace = stopGrace
	}

	mounts, err := resolveHostMounts(raw.Security.HostAccess.Mounts)
	if err != nil {
		return err
	}

	cfg.Security.HostAccess.Mounts = mounts

	keys, err := resolveAPIKeys(raw.Security.APIKeys)
	if err != nil {
		return err
	}

	cfg.Security.APIKeys = keys

	return nil
}

// resolveHostMounts 只做 access 枚举解析与默认值(空值按只读处理);
// 路径形状与允许清单合法性由 hostaccess.NewPolicy 在装配期判定。
func resolveHostMounts(raw []rawHostMountConfig) ([]HostMountConfig, error) {
	mounts := make([]HostMountConfig, 0, len(raw))
	for i, mount := range raw {
		access := mount.Access
		switch access {
		case "":
			access = AccessReadOnly
		case AccessReadOnly, AccessReadWrite:
		default:
			return nil, fmt.Errorf(
				"parse security.hostAccess.mounts[%d].access: %q must be %q or %q",
				i, mount.Access, AccessReadOnly, AccessReadWrite,
			)
		}

		mounts = append(mounts, HostMountConfig{
			Host:      mount.Host,
			Container: mount.Container,
			Access:    access,
		})
	}

	return mounts, nil
}

// resolveAPIKeys 把 key/keyEnv 解析成明文密钥。错误信息只带下标与字段名，
// 绝不回显密钥值。
func resolveAPIKeys(raw []rawAPIKeyConfig) ([]APIKeyConfig, error) {
	keys := make([]APIKeyConfig, 0, len(raw))
	for i, entry := range raw {
		key := APIKeyConfig{Name: entry.Name, Role: entry.Role}

		switch {
		case entry.Key != "" && entry.KeyEnv != "":
			return nil, fmt.Errorf("security.apiKeys[%d]: key and keyEnv are mutually exclusive", i)
		case entry.Key != "":
			key.Secret = entry.Key
		case entry.KeyEnv != "":
			secret := os.Getenv(entry.KeyEnv)
			if secret == "" {
				return nil, fmt.Errorf("security.apiKeys[%d]: environment variable %q is not set", i, entry.KeyEnv)
			}

			key.Secret = secret
		default:
			return nil, fmt.Errorf("security.apiKeys[%d]: exactly one of key or keyEnv is required", i)
		}

		keys = append(keys, key)
	}

	return keys, nil
}

func loadEnvironment(cfg *Config) error {
	if value := os.Getenv("AGENTNEXUS_HTTP_ADDRESS"); value != "" {
		cfg.Server.HTTPAddress = value
	}

	if value := os.Getenv("AGENTNEXUS_SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf(
				"parse AGENTNEXUS_SHUTDOWN_TIMEOUT: %w", err,
			)
		}

		cfg.Server.ShutdownTimeout = timeout
	}

	if value := os.Getenv("AGENTNEXUS_DATABASE_PATH"); value != "" {
		cfg.Database.Path = value
	}

	if value := os.Getenv("AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf(
				"parse AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL: %w", err,
			)
		}

		cfg.Runtime.ReconcileInterval = interval
	}
	return nil
}

func validate(cfg Config) error {
	if cfg.Server.HTTPAddress == "" {
		return fmt.Errorf("server.httpAddress must not be empty")
	}

	if cfg.Server.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.shutdownTimeout must be greater than zero")
	}

	if cfg.Database.Path == "" {
		return fmt.Errorf("database.path must not be empty")
	}

	if cfg.Runtime.ReconcileInterval < 0 {
		return fmt.Errorf("runtime.reconcileInterval must not be negative")
	}

	if cfg.Runtime.Docker.Host == "" {
		return fmt.Errorf("runtime.docker.host must not be empty")
	}

	if !strings.HasPrefix(cfg.Runtime.Docker.Host, "unix://") && !strings.HasPrefix(cfg.Runtime.Docker.Host, "tcp://") {
		return fmt.Errorf("runtime.docker.host must start with unix:// or tcp://")
	}

	if cfg.Runtime.Docker.ConnectHost == "" {
		return fmt.Errorf("runtime.docker.connectHost must not be empty")
	}

	if cfg.Runtime.Docker.PublishHost == "" {
		return fmt.Errorf("runtime.docker.publishHost must not be empty")
	}

	if cfg.Runtime.Docker.StopGrace <= 0 {
		return fmt.Errorf("runtime.docker.stopGrace must be greater than zero")
	}

	return nil
}
