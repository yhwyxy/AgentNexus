# PR-3: Tool Snapshot 与 Catalog 设计

日期：2026-09-24

## 目标

实现 MCP Server 下游工具的发现、校验、原子持久化和进程内只读聚合。PR-3 作为一个完整纵切，拆分为两个独立验证和提交的阶段：快照持久化；同步服务与 Catalog。

不包含 PR-4 的上游 Virtual MCP Server、`/mcp` Router，不在注册 HTTP 请求中同步启动后端，不增加 refresh 管理 API、后台轮询或通知订阅。

## 现状与边界

仓库已有 Server domain/repository、SQLite migration、PR-2 `runtime.Provider`、SDK 隔离的 `mcpclient.Manager`。当前没有 `RuntimeManager.EnsureReady`。因此本阶段的 `ToolSyncService.Refresh` 显式加载 Server，并通过构造时注入的 Runtime Provider 和 MCP Client Manager 获取工具；不假称已有自动 reconcile/runtime lifecycle。调用方显式触发 Refresh。

MCP SDK 仅允许存在于 `internal/mcpadapter`；Tool、ToolSync 和 Catalog 均使用 `internal/mcpclient`、`internal/runtime`、`internal/server` 的类型及领域接口。SQL 仅存在于 `internal/storage/sqlite`。

## 模型与命名

`internal/tool` 定义工具 Definition、Route、Tool Snapshot、Snapshot Replacement、Tool Repository 及状态/错误语义。公开名为 `server.name + "." + backendTool.name`，后端名称大小写和内容原样保留。后端名称非空，公开名最长 128 字节，只允许 ASCII 字母、数字、下划线、连字符和句点。非法名称、后端重复名、公开名冲突或超长均拒绝整次刷新；不截断、不重命名。路由使用 `publicName → Route` 映射，不拆分名称反推 Server。

输入 Schema 必须存在、是合法 JSON object；不生成默认 schema。递归规范化 object key 并压缩 JSON，按规范化字节计算 SHA-256 schema digest。Catalog digest 使用按 public name 排序的 Definition，按稳定字段顺序编码 `public name`、`backend name`、title、description、input/output schema、annotations 后计算 SHA-256；排除数据库 ID、snapshot ID 和排序偶然性。`RefreshResult.Changed` 比较新摘要与刷新的前一个 active snapshot 摘要。

## 持久化和事务

新增版本化 SQLite migration，建立 `tool_snapshots`、`tools`、外键、唯一约束和设计文档规定的索引。每 Server 至多一个 active snapshot；每 snapshot 的 backend name 与 public name 唯一。时间戳遵守仓库 UTC RFC3339Nano 约定，ID 由调用者生成。

`ReplaceSnapshot` 在单个事务内插入完整新 snapshot 和工具、将旧 active 标记为 superseded、将新 snapshot 标记为 active。generation 是该 Server 历史最大 generation 加一；事务失败回滚全部变化，旧 active 保持可见；不留下半成品 building snapshot。工具获取和 schema 验证在事务外完成。读取只选择 active 且 revision 匹配的快照；聚合只包含 enabled 且 desired state 为 running 的 Server。

Repository 提供替换快照、读取 active snapshot、聚合 definitions、按 public name 解析 Route 的接口。SQLite adapter 不持有 runtime/MCP session，也不依赖 SDK。

## 刷新服务

`ToolSyncService.Refresh(ctx, serverID)` 执行：读取 Server；要求 enabled 且 desired state running；调用注入的 Provider.Ensure；Acquire MCP session lease 并保证 Release；遍历全部 `tools/list` 分页；检测重复 cursor 防止后端分页循环；校验及规范化每个工具；计算 schema/catalog digest；调用 `ReplaceSnapshot`；成功后使 Catalog 缓存失效并返回 server、generation、tool count、digest、changed。

任一 Runtime、连接、工具列表、分页、验证或存储错误均返回错误，不覆盖旧 active snapshot。当前 Server 状态仓储不提供专属失败计数更新契约，因此本 PR 不在刷新失败时写 degraded 状态；后续 Runtime Manager/Reconciler 负责运行状态收敛。

## Catalog Cache

Catalog 提供不可变 `CatalogSnapshot`：digest、构建时间、按公开名升序的 definitions 和 public-name 到 Route 的索引。通过 `atomic.Pointer` 发布完整快照；请求只读取已发布快照，不在读取期间持有 SQLite 事务。失效后按需重建，使用 singleflight 合并并发冷构建。跨 Server 发现重复 public name 时重建失败，不任意选择路由；构建失败不能发布部分 Catalog。只有成功提交 snapshot 后才使缓存失效。

Catalog 仅聚合 enabled、desired state running、存在 active snapshot 且 snapshot revision 等于 Server revision 的记录。Catalog cursor 分页与上游 `tools/list` 属于 PR-4，不在本实现范围。

## 提交阶段与验收

### 阶段一：快照持久化

Migration、tool domain/repository、SQLite 原子替换及读取。验证 migration、约束、事务回滚保留旧快照、active 查询和聚合过滤。

### 阶段二：同步与 Catalog

名称/schema/digest 规范化、分页 ToolSyncService、失败保留旧 active、不可变 Catalog cache。使用 PR-2 Fake MCP Server 验证真实 SDK list 路径及新工具快照生成；验证 schema/name/cursor 错误不破坏旧快照、并发读取只观察完整 Catalog。

所有测试使用临时 SQLite 与本地 fake，不依赖公网或外部服务。最终执行 gofmt、`go test ./...`、`go vet ./...`、`go build ./...`。
