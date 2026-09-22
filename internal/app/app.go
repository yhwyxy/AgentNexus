// 负责编排应用启动流程和启动时加载配置
/*
 Config → Logger → SQLite → Repositories → Services → HTTP Server → MCP Gateway
*/
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/yhwyxy/AgentNexus/internal/config"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func Run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger()

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	db, err := sqlite.Open(context.Background(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	logger.Info(
		"database opened",
		"database_path", cfg.Database.Path,
	)

	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	logger.Info(
		"database migrations applied",
		"database_path", cfg.Database.Path,
	)

	registry := server.NewService(sqlite.NewServerRepository(db))

	httpServer := httpapi.NewServer(
		cfg.Server.HTTPAddress,
		httpapi.NewHandler(registry, logger),
	)

	serverErr := make(chan error, 1)

	go func() {
		logger.Info(
			"HTTP server starting",
			"http_address", cfg.Server.HTTPAddress,
			"database_path", cfg.Database.Path,
		)
		serverErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil

	case <-ctx.Done():
		logger.Info("shutdown signal received")

		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			cfg.Server.ShutdownTimeout,
		)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}

		logger.Info("HTTP server stopped")

		return nil
	}
}
