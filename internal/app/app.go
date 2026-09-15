// 负责编排应用启动流程和启动时加载配置
/*
 Config → Logger → SQLite → Repositories → Services → HTTP Server → MCP Gateway
*/
package app

import (
	"fmt"

	"github.com/yhwyxy/AgentNexus/internal/config"
	"github.com/yhwyxy/AgentNexus/internal/observability"
)

func Run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger()

	logger.Info(
		"AgentNexus starting",
		"http_address", cfg.Server.HTTPAddress,
		"shutdown_timeout", cfg.Server.ShutdownTimeout.String(),
	)

	return nil
}
