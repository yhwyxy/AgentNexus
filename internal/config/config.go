// 定义 AgentNexus 配置结构、默认值，并加载 Server 配置。
package config

import (
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	defaultConfigPath        = "configs/agentnexus.yaml"
	defaultHTTPAddress       = ":8080"
	defaultShutdownTimeout   = 15 * time.Second
	defaultDatabasePath      = "data/agentnexus.db"
	defaultReconcileInterval = 30 * time.Second
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	Security SecurityConfig `yaml:"security"`
}

// SecurityConfig 描述静态 API Key 认证（详细设计 §13）。空列表合法：服务照常启动，
// 但除公开探针外全部请求都会被拒绝。
type SecurityConfig struct {
	APIKeys []APIKeyConfig `yaml:"apiKeys"`
}

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
}

type rawConfig struct {
	Server   rawServerConfig   `yaml:"server"`
	Database rawDatabaseConfig `yaml:"database"`
	Runtime  rawRuntimeConfig  `yaml:"runtime"`
	Security rawSecurityConfig `yaml:"security"`
}

type rawSecurityConfig struct {
	APIKeys []rawAPIKeyConfig `yaml:"apiKeys"`
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
	ReconcileInterval string `yaml:"reconcileInterval"`
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

	keys, err := resolveAPIKeys(raw.Security.APIKeys)
	if err != nil {
		return err
	}

	cfg.Security.APIKeys = keys

	return nil
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

	return nil
}
