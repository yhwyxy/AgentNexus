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

	"github.com/yhwyxy/AgentNexus/internal/audit"
	"github.com/yhwyxy/AgentNexus/internal/auth"
	"github.com/yhwyxy/AgentNexus/internal/config"
	"github.com/yhwyxy/AgentNexus/internal/edge/httpapi"
	"github.com/yhwyxy/AgentNexus/internal/gateway"
	"github.com/yhwyxy/AgentNexus/internal/mcpadapter"
	mcpserver "github.com/yhwyxy/AgentNexus/internal/mcpadapter/server"
	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/observability"
	"github.com/yhwyxy/AgentNexus/internal/runtime"
	"github.com/yhwyxy/AgentNexus/internal/runtime/process"
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

	// 认证在最早的失败点完成：密钥表非法（角色拼错、密钥重复等）直接拒绝启动。
	// 空密钥表是合法配置——服务照常启动，但除探针外全部请求都会被拒绝。
	authorizer, err := auth.NewAuthorizer(toKeyConfigs(cfg.Security.APIKeys), auth.DefaultPolicy())
	if err != nil {
		return fmt.Errorf("build API key authorizer: %w", err)
	}
	if len(cfg.Security.APIKeys) == 0 {
		logger.Warn("no API keys configured; all requests will be rejected")
	}

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
	// 审计只有一条写入路径:所有发射点(HTTP 管理动作、生命周期、Runtime、工具调用)
	// 共用同一个 Recorder,写入失败只记日志,绝不改变业务结果。
	recorder := audit.NewRecorder(sqlite.NewAuditRepository(db))
	observer := observability.NewAuditObserver(recorder, logger)
	toolRepo := sqlite.NewToolRepository(db)
	catalog := tool.NewCatalog(toolRepo)
	clients := mcpclient.NewManager(mcpadapter.NewConnector())
	defer clients.Close(context.Background())
	runtimes, err := runtime.NewManager(servers, remote.NewProvider(), process.NewProvider(ctx))
	if err != nil {
		return fmt.Errorf("create runtime manager: %w", err)
	}
	runtimes.WithObserver(observer)
	// 子进程由 ProviderManager 持有:关闭顺序是先停生命周期队列,再逐个终止子进程,
	// 最后关闭 MCP session。defer 是后进先出,因此这里声明在 lifecycle 之前。
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		if err := runtimes.Close(closeCtx); err != nil {
			logger.Warn("runtime providers did not stop cleanly", "error", err)
		}
	}()
	syncer := tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog)
	// 运行态协调在后台完成：注册触发，加上启动重放与按 reconcileInterval 的周期巡检
	// （重启后实例缓存为空，DB 里的 ready 不代表运行态存在，必须重放）。
	// 关闭时先停止 HTTP，再由本 defer 取消在飞同步并等待 worker 与 Reconciler 退出。
	lifecycle, err := NewLifecycle(LifecycleOptions{
		Registry:          registry,
		Syncer:            syncer,
		Auditor:           recorder,
		Lister:            servers,
		ReconcileInterval: cfg.Runtime.ReconcileInterval,
		Logger:            logger,
	})
	if err != nil {
		return fmt.Errorf("create lifecycle: %w", err)
	}
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
	caller.WithObserver(observer)
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
		httpapi.NewHandler(httpapi.Options{
			Registry:      lifecycle,
			Refresher:     lifecycle,
			MCP:           mcpHandler,
			Authenticator: authorizer,
			Auditor:       recorder,
			Logger:        logger,
		}),
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

// toKeyConfigs 只做形态转换：角色字符串的合法性由 auth.NewAuthorizer 判定，
// 校验语义只存在一处。
func toKeyConfigs(configured []config.APIKeyConfig) []auth.KeyConfig {
	keys := make([]auth.KeyConfig, 0, len(configured))
	for _, entry := range configured {
		keys = append(keys, auth.KeyConfig{
			Name:   entry.Name,
			Role:   auth.Role(entry.Role),
			Secret: entry.Secret,
		})
	}

	return keys
}
