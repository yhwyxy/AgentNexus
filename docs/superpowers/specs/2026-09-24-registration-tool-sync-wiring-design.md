# 注册与启动接入 Tool 同步及 Runtime 生命周期协调设计

日期：2026-09-24

## 目标

把已合并的 `runtime.ProviderManager`（PR #6）与 `tool.SyncService`（PR #5）接进生产路径，使详细设计 §9.2 与 §10.1 的链路真正贯通：

```text
POST /api/v1/mcp-servers
  → server.Service.Register（事务：assets + mcp_servers + server_status）
  → 202 Accepted / Server(pending)
  → 异步：RuntimeManager.EnsureReady → ClientManager.Acquire → 分页 tools/list
          → ReplaceSnapshot → Catalog Invalidate → Server ready
```

PR-3 的 spec 明确记录了当时的妥协：「本阶段的 `ToolSyncService.Refresh` 显式加载 Server，并通过构造时注入的 Runtime Provider 和 MCP Client Manager 获取工具；不假称已有自动 reconcile/runtime lifecycle。调用方显式触发 Refresh。」Runtime Manager 已落地，本 PR 消除该妥协，并补上注册后的触发点与应用装配。

## 现状与边界

- `internal/app/app.go` 从未构造 `tool.SyncService`；`internal/tool/sync_integration_test.go` 与 `internal/gateway/router_integration_test.go` 是仅有的调用方。
- `tool.SyncService.Sync` 直接调用 `runtime.Provider.Ensure`，绕过 Manager：不写 `server_status`（phase 永远停在 `pending`）、不复用 Manager 的实例缓存，Process/Docker provider 落地后同一次注册会对后端 Ensure 两次。
- 注册成功后没有任何触发点，Gateway 的 `tools/list` 只能看到空 Catalog。

不在本 PR 范围：后台巡检 / 周期性健康检查、启动时对既有 Server 的全量重放、管理 API `:refresh-tools`、Process/Docker Provider、auth、audit、metrics。

## 设计

### 1. SyncService 依赖 RuntimeManager

`SyncService` 的 `runtime.Provider` 字段与构造参数替换为 `runtime.Manager`，`Sync` 内部从 `provider.Ensure` 改为 `runtimes.EnsureReady(ctx, srv)`。收益：

- per-Server 串行锁与实例缓存由 Manager 统一持有，Run 与 Call 路径共享同一实例；
- ready/degraded 与 `observedRevision` 由 Manager 写入 `server_status`，注册后状态收敛不再悬空；
- 运行时类型分发交给 Manager，`SyncService` 删除 "remote runtime provider required" 这类 provider 白名单判断（Manager 按 `Spec.Runtime.Type` 选择，未注册类型返回 `ErrProviderNotFound`）。

`SyncService` 仍自行加载 Server，以保留 `enabled` 且 `DesiredState == running` 的领域前置校验（失败返回包装 `ErrInvalidTool`），并保留 revision + catalog digest 相同的幂等短路。

同时给 Ensure/Acquire 套上 `Spec.Timeouts.Connect` 上限：异步 worker 单线程消费队列，若后端 TCP 可连但 MCP initialize 永不返回，未设限的 `Acquire` 会永久占住 worker，使后续注册再也无法同步。超时为 0 时保持无 deadline 的现状（测试可控）。分页 `tools/list` 的每页超时沿用现有 `Spec.Timeouts.List`。

### 2. 注册后的异步协调（`internal/app`）

新增 `internal/app/lifecycle.go`：

```go
type ServerRegistry interface {
	Register(context.Context, server.RegisterInput) (server.Server, error)
	Get(context.Context, server.ID) (server.Server, error)
}

type ToolSynchronizer interface {
	Sync(context.Context, server.ID) (tool.SyncResult, error)
}

type Lifecycle struct{ /* ... */ }
func NewLifecycle(registry ServerRegistry, syncer ToolSynchronizer, logger *slog.Logger) *Lifecycle
func (l *Lifecycle) Register(ctx context.Context, in server.RegisterInput) (server.Server, error)
func (l *Lifecycle) Get(ctx context.Context, id server.ID) (server.Server, error)
func (l *Lifecycle) Trigger(id server.ID)
func (l *Lifecycle) Close(ctx context.Context) error
```

- `Lifecycle` 满足 `httpapi.ServerRegistry`（结构一致），在 `app.Run` 中包装真实 `*server.Service` 后交给 HTTP 层；HTTP 层不知道协调逻辑的存在。
- `Register` 委托成功后调用 `Trigger(srv.ID)`，随后原样返回注册结果——响应仍是 `202 Accepted` / `pending`，运行态在后端异步收敛。
- 队列：`[]server.ID` FIFO + `map[server.ID]struct{}` 去重集，只由一个 worker goroutine 消费。去重覆盖「已入队未出队」的重复触发；`Trigger` 不做阻塞发送，避免拖慢注册请求。空队列时 worker 等待 `wake` 信号或应用 context 结束。
- 同步使用应用生命周期 context，不使用请求 context（请求在 202 响应时已结束）；失败与部分结果只记日志，不回写注册响应。
- `Close` 置 `closed`（后续 `Trigger` 变成 no-op）、取消 context 并等待 worker 退出；幂等，便于 `app.Run` 的 defer 与显式关闭复用。

### 3. 应用装配与关闭

`app.Run` 增加：

```go
syncer := tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog)
lifecycle := NewLifecycle(registry, syncer, logger)
defer func() { /* 以 shutdownTimeout 关闭 lifecycle，超时只告警 */ }()
httpServer := httpapi.NewServer(cfg.Server.HTTPAddress, httpapi.NewHandlerWithMCP(lifecycle, mcpHandler, logger))
```

关闭顺序：HTTP `Shutdown` 先停止接受新注册，函数返回时 deferred `lifecycle.Close` 取消并等待在飞同步，最后才是既有的 `db.Close` / `clients.Close`。`Close` 超时（后端函数无视 context）只记 warning，不阻断进程退出。

`Lifecycle` 虽然是 `app` 包内类型，但它是应用层编排：只依赖 `server` 领域类型与 `tool.SyncResult`，不触碰 SQL、MCP SDK、HTTP。

## 验证

1. `internal/app/lifecycle_test.go`（fake registry + fake syncer，确定性 channel 同步）：
   - 注册成功 → 同步收到新 ID，`Register` 返回值原样透传，`Get` 委托；
   - 注册失败 → 不触发同步（通过 `Close` 排空后断言调用记录为空）；
   - 同一 ID 在队列中重复 `Trigger` 只消费一次，不同 ID 互不影响；
   - `Close` 取消在飞同步并等待 worker 退出；`Close` 后 `Trigger` 为 no-op；重复 `Close` 返回 nil。
2. `internal/app/lifecycle_integration_test.go`（真实 SQLite + `fakemcp` + 真实 `SyncService`/`ProviderManager`/MCP handler）：
   - POST 注册 → 202 且 phase 为 `pending`；轮询 GET 直到 `phase=ready`、`observedRevision=1`；
   - 官方 SDK client 连 `/mcp` 执行 `tools/list`，看到 `backend.demo.echo`，证明注册 → 异步协调 → 快照落库 → Catalog 失效 → 聚合可见整条链路。
3. 更新 `internal/tool/sync_integration_test.go` 与 `internal/gateway/router_integration_test.go` 改用 `runtime.NewManager(servers, remote.NewProvider())`。
4. 提交前执行 `gofmt -l .`（无输出）、`go vet ./...`、`go build ./...`、`go test ./...`。