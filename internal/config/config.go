// 定义 AgentNexus 配置结构、默认值，并加载 Server 配置。
package config

import (
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	defaultConfigPath      = "configs/agentnexus.yaml"
	defaultHTTPAddress     = ":8080"
	defaultShutdownTimeout = 15 * time.Second
)

type Config struct {
	Server ServerConfig `yaml:"server"`
}

type ServerConfig struct {
	HTTPAddress     string        `yaml:"httpAddress"`
	ShutdownTimeout time.Duration `yaml:"-"`
}

type rawConfig struct {
	Server rawServerConfig `yaml:"server"`
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

	return nil
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
	return nil
}

func validate(cfg Config) error {
	if cfg.Server.HTTPAddress == "" {
		return fmt.Errorf("server.httpAddress must not be empty")
	}

	if cfg.Server.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.shutdownTimeout must be greater than zero")
	}

	return nil
}
