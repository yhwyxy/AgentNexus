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
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	mcpserver "github.com/yhwyxy/AgentNexus/internal/mcpadapter/server"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/remote"
	"github.com/yhwyxy/AgentNexus/internal/server"
	"github.com/yhwyxy/AgentNexus/internal/storage/sqlite"
	"github.com/yhwyxy/AgentNexus/internal/tool"
	"github.com/yhwyxy/AgentNexus/migrations"
)

func Run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(context.Background(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	logger.Info("database opened", "database_path", cfg.Database.Path)

	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	logger.Info("database migrations applied", "database_path", cfg.Database.Path)

	servers := sqlite.NewServerRepository(db)
	registry := server.NewService(servers)
	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo)
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	defer clients.Close(context.Background())
	runtimes, err := runtime.NewManager(servers, remote.NewProvider())
	if err != nil {
		return fmt.Errorf("create runtime manager: %w", err)
	}
	syncer := tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog)
	// 注册后的运行态协调与 Tool 同步在后台完成；关闭时先停止 HTTP，
	// 再由本 defer 取消在飞同步并等待 worker 退出。
	lifecycle := NewLifecycle(registry, syncer, logger)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		if err := lifecycle.Close(closeCtx); err != nil {
			logger.Warn("tool sync did not stop before shutdown timeout", "error", err)
		}
	}()
	caller, err := gateway.NewToolCaller(catalog, servers, runtimes, clients)
	if err != nil {
		return fmt.Errorf("create tool caller: %w", err)
	}
	virtual, err := gateway.NewVirtualServer(catalog, caller)
	if err != nil {
		return fmt.Errorf("create virtual MCP server: %w", err)
	}
	mcpHandler, err := mcpserver.NewHandler(virtual)
	if err != nil {
		return fmt.Errorf("create MCP handler: %w", err)
	}

	httpServer := httpapi.NewServer(
		cfg.Server.HTTPAddress,
		httpapi.NewHandlerWithMCP(lifecycle, mcpHandler, logger),
	)
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "http_address", cfg.Server.HTTPAddress, "database_path", cfg.Database.Path)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		logger.Info("HTTP server stopped")
		return nil
	}
}
