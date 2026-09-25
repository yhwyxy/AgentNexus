# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

AgentNexus is a self-hosted "Agent Capability Control Plane" written in Go. v0.1 is an MCP gateway/aggregator: register backend MCP servers (remote streamable HTTP, stdio process, Docker), health-check them, aggregate their `tools/list`, and route `tools/call` through a single `/mcp` endpoint using `serverName.toolName` public names. Implemented so far: config/bootstrap, SQLite storage and migrations, the MCP Server domain model/service/repository, Tool snapshots + catalog + sync (`internal/tool`), the runtime `ProviderManager` with the remote and process providers (`internal/runtime`), the MCP client session manager (`internal/mcpclient`), the MCP SDK adapter (`internal/mcpadapter`), the virtual MCP server + router (`internal/gateway`), and the app wiring/lifecycle/reconciler (`internal/app`: registration-triggered sync, startup replay, periodic sweep, `POST ...:refresh-tools`). Not written yet: the Docker runtime provider, `internal/auth`, `internal/audit`, metrics, and health probing (convergence is driven by registration, refresh, and the sweep - there is no active backend health check yet).

Module: `github.com/yhwyxy/AgentNexus`, Go 1.27. Dependencies are deliberately few: `modernc.org/sqlite` (pure Go, no cgo), `go.yaml.in/yaml/v3`, the official `github.com/modelcontextprotocol/go-sdk`, plus `github.com/google/uuid` (IDs) and `golang.org/x/sync` (singleflight in the catalog cache). HTTP routing uses `net/http` method patterns (`"GET /health/live"`); tests use only the standard `testing` package.

## Commands

There is no Makefile, linter config, or CI config in the repo. Use the Go toolchain directly.

```bash
go build ./...                                          # compile everything
go build -o bin/agentnexus ./cmd/agentnexus             # binary (bin/ is gitignored)
go test ./...                                           # all tests
go test ./internal/server -v                            # one package
go test ./internal/server -run TestServerValidate -v    # one test
go vet ./...
gofmt -l .                                              # must print nothing before a PR
```

Run the service:

```bash
go run ./cmd/agentnexus                     # reads configs/agentnexus.yaml if present
go run ./cmd/agentnexus -config other.yaml
AGENTNEXUS_HTTP_ADDRESS=:9000 AGENTNEXUS_DATABASE_PATH=/tmp/an.db go run ./cmd/agentnexus
curl localhost:8080/health/live             # {"status":"ok"}; /health/ready is identical for now
curl -s -X POST localhost:8080/api/v1/mcp-servers -d '{"name":"weather","transport":"streamable_http","runtime":{"type":"remote","remote":{"endpoint":"http://weather-mcp:8080/mcp"}}}'   # 202 + Location
curl -s -X POST localhost:8080/api/v1/mcp-servers -d '{"name":"local-add","transport":"stdio","runtime":{"type":"process","process":{"command":"/abs/path/to/mcp-server","args":[],"env":{"K":"V"},"workingDir":"/abs/dir"}}}'   # stdio child; command must be an absolute path
curl -s localhost:8080/api/v1/mcp-servers/<id>   # 200; 404 for unknown id
curl -s -X POST localhost:8080/api/v1/mcp-servers/<id>:refresh-tools   # 202; 409 if not enabled/running
# status.phase converges asynchronously: pending -> starting (runtime up) -> ready (tool snapshot published)
# the startup replay and the periodic sweep (runtime.reconcileInterval) drive the same convergence loop
```

Config precedence: built-in defaults → YAML → `AGENTNEXUS_HTTP_ADDRESS` / `AGENTNEXUS_SHUTDOWN_TIMEOUT` / `AGENTNEXUS_DATABASE_PATH` / `AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL` → validate. A missing YAML file is silently ignored; a present-but-invalid one is fatal. `runtime.reconcileInterval` (YAML: duration string, default 30s) sets the periodic sweep cadence; `0s` disables the sweep but keeps the startup replay. Negative values are rejected. `configs/dev.yaml` currently fails validation (`shutdownTimeout: 0s`), so do not use it as-is.

Manual/phase-0 programs live in `examples/` (gitignored, so absent on a fresh clone):

```bash
go run ./examples/phase0-add-server -http :8080   # streamable HTTP; omit -http for stdio
go run ./examples/phase0-add-client               # connects to localhost:8080/mcp
```

For end-to-end work prefer `internal/testsupport/fakemcp`: a real in-process MCP server exposing `demo.echo`/`demo.fail` over streamable HTTP. Integration tests register it as a backend (`fakemcp.New()`, `fake.HTTP.URL`) and then drive the real HTTP/MCP surface.

## Architecture

### Layering rule (frozen by the design docs)

```
edge (HTTP / MCP handlers) → application service → domain interfaces → adapters (sqlite, runtime, mcpadapter)
```

- `internal/server` is the domain layer. It must not import `database/sql`, the MCP SDK, `net/http`, or Docker types. Its `Repository` interface is shaped by domain needs; `internal/storage/sqlite` is an adapter implementing it.
- The MCP Go SDK is only imported by `internal/mcpadapter` (plus tests). Domain and service code never see SDK types.
- Gateway/router code (`internal/gateway`, `internal/tool`) must not execute SQL. Runtime providers (`internal/runtime/{remote,process,docker}`) must not decide public tool names.
- The database stores configuration and observed status only. Never persist live connections, MCP sessions, or process handles.

### Startup path

`cmd/agentnexus/main.go` -> `app.Run`:

```
config.Load -> JSON slog logger -> sqlite.Open -> sqlite.Migrate(ctx, db, migrations.FS)
  -> sqlite.NewServerRepository / sqlite.NewToolRepository
  -> server.NewService (registry)
  -> runtime.NewManager(servers, remote.NewProvider(), process.NewProvider(ctx))
     # ctx 是 NotifyContext。正常退出走分阶段关闭(defer runtimes.Close -> Provider.Close,
     # 按进程组 SIGTERM,宽限后 SIGKILL);ctx 取消只是兜底(exec 默认 Cancel 只 SIGKILL 直接子进程)。
     # 因为 defer 后进先出,runtimes.Close 先于 clients.Close,stop() 最后。
  -> mcpclient.NewManager(mcpadapter.NewConnector())
  -> tool.NewCatalog(toolRepo) + tool.NewSyncService(servers, runtimes, clients, toolRepo, catalog)
  -> gateway.NewToolCaller + gateway.NewVirtualServer -> mcpserver.NewHandler (SDK adapter)
  -> app.NewLifecycle(LifecycleOptions{Registry, Syncer, Lister, ReconcileInterval, Logger})
     # decorates the registry: sync after Register; Lister != nil also starts
     # the startup replay + periodic sweep (reconciler.go)
  -> httpapi.NewHandler(httpapi.Options{Registry: lifecycle, Refresher: lifecycle, MCP: mcpHandler, Logger: logger})
     wrapped by httpapi.NewServer
  -> block until SIGINT/SIGTERM -> httpServer.Shutdown -> deferred runtimes.Close,
     lifecycle.Close, clients.Close, db.Close
```

Shutdown order matters: stop accepting registrations first, then cancel/wait the sync worker and the reconciler (bounded by `shutdownTimeout`), then stop runtime instances (process children: SIGTERM to the process group, then SIGKILL after the grace period), then close MCP sessions, then the database. A migration failure prevents the HTTP server from starting.

Registry, startup replay, periodic sweep, and `:refresh-tools` all feed the same FIFO queue consumed by one worker, so a single server is never synced concurrently and a duplicate trigger while a sync is in flight is dropped (the next sweep retries). The replay is unconditional, the sweep only retries servers that are not converged (`phase != ready`, `observedRevision != revision`, or `consecutiveFailures > 0`), so a healthy server is not re-listed every tick. `SyncService.Sync` itself short-circuits when `revision` + `catalogDigest` are unchanged, so a replay of an untouched server costs one `tools/list` and no snapshot write.

### Storage (`internal/storage/sqlite`, `migrations/`)

- `Open` resolves the path to absolute, builds a `file://` DSN, and applies the required per-connection baseline through DSN params: `_foreign_keys=on`, `_busy_timeout=5000`, `_journal_mode=WAL`, `_synchronous=NORMAL`. Verify PRAGMAs in tests through a real `Open` connection; the `sqlite3` CLI opens its own connection and will not show connection-level settings.
- `Migrate` accepts any `fs.FS`, runs `NNNNNN_name.sql` files in version order, one transaction each, and records them in `schema_migrations`. Production migrations are embedded via `migrations.FS` (`//go:embed *.sql`); tests use `testing/fstest.MapFS` for synthetic ones.
- Schema (`000001_mcp_core.sql`): `assets` is a generic Kubernetes-style envelope (`kind`, `namespace`, `name`, `labels_json`, `enabled`, `revision`; UNIQUE on namespace+kind+name). `mcp_servers` is the 1:1 kind-specific extension keyed by `asset_id`, holding `runtime_spec_json` and timeouts in milliseconds. `server_status` holds observed runtime state. `credentials` is referenced by `mcp_servers.credential_id`. Future kinds (Skill, Agent, Workflow) are meant to reuse `assets`.
- Schema (`000002_tool_catalog.sql`): `tool_snapshots` holds one row per refresh (`state` in building/active/superseded/failed, `generation`, `catalog_digest`, `server_revision`; a partial unique index enforces at most one `active` snapshot per server) and `tools` holds the normalized definitions keyed by `snapshot_id` (UNIQUE on `snapshot_id` + `backend_name`/`public_name`). `ListAggregated` returns only tools whose snapshot is `active`, whose `server_revision` equals `assets.revision`, and whose asset is enabled with `desired_state='running'` - a superseded or stale snapshot drops out of the catalog automatically.
- Conventions: timestamps are UTC RFC3339Nano strings, and `Service.Register` normalizes its clock to UTC so the value it returns renders identically to what a later read returns; booleans are 0/1 integers; IDs are strings assigned by the caller (the repository rejects an empty ID). Range limits on timeouts and `max_in_flight` exist both as SQL CHECK constraints and in `server.Validate`; keep them in sync.
- `ServerRepository.Create` writes `assets` + `mcp_servers` + `server_status` in one transaction. SQLite UNIQUE violations (extended code 2067) map to `server.ErrAlreadyExists`; `sql.ErrNoRows` maps to `server.ErrNotFound`. `WithClock` injects time for tests.

### Domain model (`internal/server`)

- `Server` = envelope fields + `Spec` (desired config) + `Status` (observed state). `Server.Revision` is the config version; `Status.ObservedRevision` is what the runtime has applied, and a mismatch means a reconcile is needed. `Spec.DesiredState` (running/stopped) and `Status.Phase` (pending/starting/ready/degraded/stopped/failed) are different things; do not conflate them.
- `RuntimeSpec` is a tagged union: `Type` plus exactly one non-nil payload (`Remote`, `Process`, `Docker`). Transport/runtime matrix: `streamable_http` → remote or docker; `stdio` → process or docker.
- `Validate()` only checks and never mutates. Defaulting (namespace `default`, timeouts 5s/10s/60s, maxInFlight 16, enabled true, desired state running) happens once, in `Service.Register` in the same package, which then assigns the ID and timestamps, validates, and persists through the `Repository` interface. Validation failures wrap `ErrInvalid`; repository errors pass through unchanged. Rules: namespace/name match `^[a-z][a-z0-9-]{0,62}$` and are immutable after registration (rename = new server); remote endpoints are absolute http/https URLs with a host and no userinfo; process commands are absolute paths with args passed as `[]string` to `exec.CommandContext`, never through a shell; secrets go through `CredentialID`, never into headers, env, or URLs.
- `Repository` sentinel errors: `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict` (optimistic-lock failure when `UpdateSpec` is called with a stale `expectedRevision`), and `ErrNotRunnable` (the server is disabled or `desiredState=stopped`, so runtime work like a tool refresh is meaningless). `Server.Runnable()` is the single definition of that predicate; the SQLite adapter's `ListEnabled` is the repository-side listing the reconciler uses.
- `Status.Phase` has two writers, per the detailed design §3.4 state machine: `ProviderManager` records `ObservedRevision`/`LastSuccessAt` and moves the phase to `starting` (or `degraded` on runtime failure) but never claims `ready`; `tool.SyncService` writes `ready` only after the snapshot is published (or confirmed unchanged), and writes `degraded` + `consecutive_failures+1` when a refresh fails, leaving the previous `active` snapshot readable. Never write `ready` before the catalog is reloaded - `ready` means "runtime applied the current revision AND the tool catalog is published". `Status.CreateInput()` is the shared status -> `CreateStatusInput` conversion both writers use.

### Auth (`internal/auth`, `internal/edge/httpapi/authmw.go`)

- Every request passes through `withAuth` before the mux; `Authorize(path, presented)` is the single decision point, so business handlers (and the MCP protocol layer) never see unauthenticated traffic.
- Roles: `admin` (management API + `/mcp`), `agent` (`/mcp` only). `auth.DefaultPolicy()` is the line policy: `/health/live` and `/health/ready` are public by exact match, `/api/v1/` requires admin, `/mcp` accepts both, and any unregistered path is denied (403) even for admin — forgetting to register a new route fails closed.
- Credentials come from config, not the database: `security.apiKeys[]` with `key` XOR `keyEnv` (`keyEnv` resolution happens in `config.loadYAML`; an unset or empty env var is fatal). Role/name/secret/duplicate validation lives only in `auth.NewAuthorizer`, and a bad key table prevents startup.
- Header rules (`presentedSecret`): `Authorization: Bearer <key>` (scheme case-insensitive) wins; a non-Bearer scheme does not fall back to `X-API-Key`; otherwise `X-API-Key`; nothing → 401.
- Failures reuse the error envelope with two codes: `unauthenticated` (401, adds `WWW-Authenticate: Bearer realm="agentnexus"`) and `permission_denied` (403, no challenge header). Messages are fixed and never echo the credential.
- An empty `apiKeys` list is valid: the service starts, logs `no API keys configured; all requests will be rejected`, and everything except the probes returns 401. `Options.Authenticator == nil` (wiring bug) fails closed for all paths, probes included.
- `auth.WithPrincipal`/`PrincipalFrom` is the only seam for reading the caller (currently unused by handlers; intended for audit/metrics).

### HTTP API (`internal/edge/httpapi`)

- `NewHandler` builds the mux, wraps it in `withAuth`, and depends on consumer-defined interfaces (`ServerRegistry` with Register + Get, plus `Authenticator`), not on the concrete service. Tests inject the real service over a temp SQLite database, or a stub for failure paths.
- Routes: `GET /health/live`, `GET /health/ready`, `POST /api/v1/mcp-servers` (202 Accepted plus `Location`, because runtime start is asynchronous by design), `GET /api/v1/mcp-servers/{id}`, `POST /api/v1/mcp-servers/{id}:refresh-tools` (202, same async semantics). The action route is only registered when a `ServerRefresher` is supplied; `net/http` requires a wildcard segment to own a whole path segment, so the route is `POST .../{rest...}` and the handler splits `<id>:<action>` (`parseAction` in `mcp_server_handlers.go`). Unknown action or unknown id → 404; a non-runnable server (disabled or desired state stopped) → 409 `conflict` via `server.ErrNotRunnable`.
- Wire format is defined by the DTOs in `dto.go`; domain types are never serialized directly. Timeouts travel as whole seconds (`connectSeconds` etc.), the runtime is a tagged union that only emits the active variant, and `desiredState` is not accepted on registration.
- Errors use one envelope, `{"error":{"code":...,"message":...}}`, with codes `invalid_argument` (400), `unauthenticated` (401), `permission_denied` (403), `not_found` (404), `conflict` (409), `internal` (500). Domain sentinels are mapped in exactly one place, `writeDomainError`. Unexpected errors are logged with detail and returned as an opaque "internal error".
- Request bodies are capped at 1 MiB and unknown JSON fields are rejected.

### Tool catalog and sync (`internal/tool`)

- Domain-only package: `model.go` (Snapshot/Definition/Route), `normalize.go` (backend tool -> `serverName.toolName` public name, schema digest, validation), `repository.go` (the `Repository` interface, implemented by `internal/storage/sqlite`), `catalog.go` (`CatalogCache`: in-memory snapshot + singleflight + epoch-based `Invalidate`), `sync.go` (`SyncService`).
- `SyncService.Sync(ctx, id)` is the whole refresh pipeline: load server -> enforce `enabled` + `DesiredState == running` -> `RuntimeManager.EnsureReady` -> `mcpclient.Manager.Acquire` -> paged `tools/list` (per-page `Spec.Timeouts.List`) -> `NormalizeTools` -> `ReplaceSnapshot` -> `catalog.Invalidate()` -> mark the server `ready`. `Spec.Timeouts.Connect` bounds `EnsureReady`/`Acquire`; without it an unresponsive backend would occupy the single sync worker forever.
- Idempotent short-circuit: when the active snapshot already matches both `revision` and `catalog_digest`, the snapshot and the catalog cache are left untouched and only the status is refreshed.
- Public names are always `serverName.toolName` resolved through the stored mapping (`ResolvePublicName` / `CatalogSnapshot.ByName`), never by splitting the string.

### Runtime (`internal/runtime`, `internal/runtime/remote`, `internal/runtime/process`)

- A `Provider` only starts/stops/inspects an instance. `Manager` (`EnsureReady`, `Stop`, `Reconcile`) owns per-server mutual exclusion (`keyedLocker`), the in-memory instance cache, and `server_status` convergence. Instances are never persisted (the database keeps configuration and observed status only). `(*ProviderManager).Close` stops every active instance and then calls the provider's `Releaser` (declared as `defer` in `app.Run` after `lifecycle` so it runs before `clients.Close`).
- Provider errors are sanitized into a fixed message (`runtime ensure failed`) before being stored, so endpoints/credentials never reach `server_status`; the original error is still returned to the caller.
- `internal/runtime/remote` speaks streamable HTTP; `internal/runtime/process` runs local stdio children. `gateway` (call path) and `tool` (refresh path) share one `Manager`, so both reuse the same instance per server.
- Process semantics: the provider owns the child (`exec.CommandContext` with the application ctx, `Setpgid` so `Stop` signals the whole group, no shell), the pipes (stdin/stdout) and stderr as a bounded ring buffer surfaced by `Logs` (snapshot semantics; `Follow` is rejected until implemented). MCP owns stdin/stdout, so child logs must go to stderr. Sessions borrow `runtime.Streams{Stdin,Stdout}` wrappers whose `Close` only marks "this connection ended" - it never closes the child's fds. A closed stream (or an exited child) makes `Inspect` fail with `ErrInstanceUnavailable`, which is the manager's reap trigger: `Stop` (kill) then `Ensure` (new pid). Instance IDs embed the pid, so a restarted backend gets new session-cache keys.
- `Inspect` returning an error is the *only* way the manager learns an instance is gone; never close the fds from the adapter or the gateway.

### Gateway and MCP adapters (`internal/gateway`, `internal/mcpclient`, `internal/mcpadapter`)

- `gateway.NewVirtualServer(catalog, caller)` exposes the aggregated catalog as a single MCP server and routes `tools/call` by looking the public name up in the catalog, then calling the backend through `mcpclient`.
- `mcpclient` is the domain-facing session layer: `Manager.Acquire(server, instance)` returns a `SessionLease` keyed by server/revision/instance ID (bounded by `maxInFlight`), `Invalidate` drops sessions after connection failures, `Close` drains everything. `Acquire` also closes and drops the sessions of other instances of the same server, so a restarted backend cannot leave a stale session behind. It depends only on `runtime.ConnectTarget`.
- `mcpadapter` is the only place touching the official SDK: `mcpadapter.NewConnector()` (client side) and `mcpadapter/server` (server side, backing the `/mcp` endpoint). `Connector.Connect` accepts `streamable_http` (needs `URL`) and `stdio` (needs `Target.Streams`, wrapped as `mcp.IOTransport`); anything else is `unsupported MCP target`. The adapter never kills processes - closing the session only closes the stream wrappers, and the manager reaps the instance afterwards.

### App wiring (`internal/app`)

- `app.Run` is the composition root. It also runs `Lifecycle`, which decorates the `ServerRegistry` handed to the HTTP layer: a successful `Register` enqueues the new ID (FIFO queue + dedup set, one worker goroutine) and the sync runs asynchronously on the application context, so the response stays `202`/`pending` and sync failures are only logged. `Lifecycle.Close` stops accepting triggers, cancels in-flight syncs and waits for the worker.
- A failed sync leaves the server `degraded` with the previous snapshot still readable; the periodic sweep retries it, and `POST ...:refresh-tools` triggers it immediately.

### Package layout (from the detailed design)

Done: `internal/tool`, `internal/runtime/{remote,process}`, `internal/mcpclient`, `internal/mcpadapter`, `internal/gateway`. Planned: `internal/runtime/docker`, `internal/auth`, `internal/audit`. Place new code in these packages rather than inventing new top-level ones.

## Conventions

- The design docs in `docs/` (gitignored, local only) are the architecture source of truth: "AgentNexus 整体架构设计 v1.0" and "AgentNexus MCP Core Detailed Design v0.1". When repo code and the docs disagree, point out the conflict instead of silently picking one.
- Code comments and docs are written in Chinese; identifiers, error strings, and log messages are English.
- Errors wrap as `fmt.Errorf("verb noun: %w", err)`. Externally visible errors must not leak credentials, endpoints, host paths, or env vars.
- Tests are table-driven where possible. SQLite tests open a real file under `t.TempDir()` and run the production migrations.
- GitHub Flow: short-lived `feat/*` branches off `main`, Conventional Commit messages (`feat: ...`), merged to `main` through pull requests. Keep each PR to one independently verifiable step and run `go test ./...`, `go vet ./...`, and `gofmt -l .` before opening it.
