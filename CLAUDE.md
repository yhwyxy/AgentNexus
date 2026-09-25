# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

AgentNexus is a self-hosted "Agent Capability Control Plane" written in Go. v0.1 is an MCP gateway/aggregator: register backend MCP servers (remote streamable HTTP, stdio process, Docker), health-check them, aggregate their `tools/list`, and route `tools/call` through a single `/mcp` endpoint using `serverName.toolName` public names. Implemented so far: config/bootstrap, SQLite storage and migrations, the MCP Server domain model/service/repository, Tool snapshots + catalog + sync (`internal/tool`), the runtime `ProviderManager` with the remote, process, and Docker providers (`internal/runtime`), the host mount allow-list (`internal/runtime/hostaccess`), the MCP client session manager (`internal/mcpclient`), the MCP SDK adapter (`internal/mcpadapter`), the virtual MCP server + router (`internal/gateway`), auth/audit/observability/metrics (`internal/auth`, `internal/audit`, `internal/observability`, `internal/metrics`), and the app wiring/lifecycle/reconciler (`internal/app`: registration-triggered sync, startup replay, periodic sweep, `POST ...:refresh-tools`). Not written yet: health probing and ready-server drift polling (convergence is driven by registration, refresh, and the sweep - there is no active backend health check yet), the management API's update/start/stop/restart actions, and credential resolution/injection.

Module: `github.com/yhwyxy/AgentNexus`, Go 1.27. Dependencies are deliberately few: `modernc.org/sqlite` (pure Go, no cgo), `go.yaml.in/yaml/v3`, the official `github.com/modelcontextprotocol/go-sdk`, plus `github.com/google/uuid` (IDs), `golang.org/x/sync` (singleflight in the catalog cache), `github.com/prometheus/client_golang` (the `/metrics` registry and `promhttp`), and - imported only by `internal/runtime/docker` - `github.com/moby/moby/client` (Engine API client), `github.com/moby/moby/api` (container/mount types + `stdcopy`) and `github.com/containerd/errdefs` (`IsNotFound`). HTTP routing uses `net/http` method patterns (`"GET /health/live"`); tests use only the standard `testing` package.

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
# docker backend, stdio transport: mounts are checked against security.hostAccess.mounts at registration time
curl -s -X POST localhost:8080/api/v1/mcp-servers -d '{"name":"boxed-add","transport":"stdio","runtime":{"type":"docker","docker":{"image":"agentnexus-demo-mcp:local","command":["-transport","stdio"],"mounts":[{"source":"/abs/sandbox","target":"/workspace","readOnly":true}]}}}'
# docker backend, streamable_http transport: `port` is the in-container MCP port, `endpointPath` defaults to /mcp
curl -s -X POST localhost:8080/api/v1/mcp-servers -d '{"name":"boxed-http","transport":"streamable_http","runtime":{"type":"docker","docker":{"image":"agentnexus-demo-mcp:local","command":["-transport","http","-addr","0.0.0.0:8080","-path","/mcp"],"port":8080,"endpointPath":"/mcp"}}}'
curl -s localhost:8080/api/v1/mcp-servers/<id>   # 200; 404 for unknown id
curl -s -X POST localhost:8080/api/v1/mcp-servers/<id>:refresh-tools   # 202; 409 if not enabled/running
# status.phase converges asynchronously: pending -> starting (runtime up) -> ready (tool snapshot published)
# the startup replay and the periodic sweep (runtime.reconcileInterval) drive the same convergence loop
```

Authentication is required for everything except the two probes, so real requests need one of the configured keys:

```bash
export AGENTNEXUS_ADMIN_KEY=... AGENTNEXUS_AGENT_KEY=...     # keyEnv: unset or empty is fatal at startup
curl -si -X POST localhost:8080/api/v1/mcp-servers -H "Authorization: Bearer $AGENTNEXUS_ADMIN_KEY" \
  -H "X-Request-Id: smoke-1" -d '{...}'                      # 202; response echoes X-Request-Id: smoke-1
sqlite3 data/agentnexus.db \
  "select occurred_at, event_type, outcome, actor_name, request_id, asset_name, target, error_code, duration_ms
   from audit_events order by occurred_at"                   # management actions, runtime/tool lifecycle, tool.called
```

Each request also emits one `request completed` log line with `request_id`, `status`, `duration_ms`, `bytes`, `principal`, and `error_code` (empty on success, `unauthenticated`/`permission_denied` on rejection).

Config precedence: built-in defaults → YAML → `AGENTNEXUS_HTTP_ADDRESS` / `AGENTNEXUS_SHUTDOWN_TIMEOUT` / `AGENTNEXUS_DATABASE_PATH` / `AGENTNEXUS_RUNTIME_RECONCILE_INTERVAL` → validate. A missing YAML file is silently ignored; a present-but-invalid one is fatal. `runtime.reconcileInterval` (YAML: duration string, default 30s) sets the periodic sweep cadence; `0s` disables the sweep but keeps the startup replay. Negative values are rejected. `configs/dev.yaml` currently fails validation (`shutdownTimeout: 0s`), so do not use it as-is.

Manual/phase-0 programs live in `examples/` (gitignored, so absent on a fresh clone):

```bash
go run ./examples/phase0-add-server -http :8080   # streamable HTTP; omit -http for stdio
go run ./examples/phase0-add-client               # connects to localhost:8080/mcp
```

Docker packaging and end-to-end smoke:

```bash
docker build -t agentnexus:local .                                  # control plane (Dockerfile)
docker build -f Dockerfile.demo-mcp -t agentnexus-demo-mcp:local .  # example backend (cmd/demo-mcp)
export AGENTNEXUS_SMOKE_KEY=$(openssl rand -hex 16)                 # compose refuses to start without it
docker compose up -d --build                                        # control plane :8080 + demo-mcp backend
docker compose down -v
scripts/compose-e2e.sh                                              # the same topology plus assertions
scripts/docker-provider-smoke.sh                                    # docker runtime end-to-end
```

- `Dockerfile` builds a static `CGO_ENABLED=0` binary on `golang:1.27-alpine` and runs it as uid 10001 on `alpine:3.21` with CA certificates and a writable `/data`. `configs/compose.yaml` is the Compose config (mounted at `/etc/agentnexus/config.yaml`, database on the `/data` volume).
- The Compose topology deliberately does **not** mount the Docker socket (socket uid/gid mapping differs between Docker Desktop, OrbStack, and Linux, which would make "starts in an empty environment" irreproducible), so `runtime.docker` stays at its defaults there and only the `remote` transport is used.
- `scripts/compose-e2e.sh` = `compose up --build` → wait for `/health/ready` → register `demo-mcp` (`streamable_http`, `http://demo-mcp:8080/mcp`) → wait for `ready` → `initialize` + `notifications/initialized` + `tools/list` + `tools/call demo-mcp.demo.echo` via `/mcp` → `compose down -v`; any failed assertion is a non-zero exit.
- `scripts/docker-provider-smoke.sh` runs the binary on the host against the local daemon and asserts the Docker provider contract: a `stdio` backend with a read-only bind mount returns the mounted file's content through `demo.readfile`, a `streamable_http` backend with `port: 8080` answers `demo.echo`, an out-of-allow-list mount and a mount target outside the allowed container root are rejected with 400 `invalid_argument`, `SIGKILL` + restart adopts the existing container (same container ID), and `SIGTERM` reclaims every `agentnexus-*` container. Both scripts source `scripts/smoke-lib.sh` (curl/awk only, no jq/python).
- `cmd/demo-mcp` is the deterministic example backend used by both scripts: `-transport http|stdio`, `-addr`, `-path`, logs to stderr (stdout belongs to the MCP protocol in stdio mode). It registers `demo.echo`, `demo.fail`, and `demo.readfile`, which reads files only below `DEMO_MCP_READ_ROOT` (default `/workspace`) and re-checks after `EvalSymlinks`, so "the bind mount really works" becomes an assertable fact. It carries its own copy of the fixture tool set instead of importing `internal/testsupport/fakemcp`, so test-support code never ships in a product binary (see the deviation note in the Docker design spec).

For end-to-end work prefer `internal/testsupport/fakemcp`: a real in-process MCP server exposing `demo.echo`/`demo.fail` over streamable HTTP. Integration tests register it as a backend (`fakemcp.New()`, `fake.HTTP.URL`) and then drive the real HTTP/MCP surface.

## Architecture

### Layering rule (frozen by the design docs)

```
edge (HTTP / MCP handlers) → application service → domain interfaces → adapters (sqlite, runtime, mcpadapter)
```

- `internal/server` is the domain layer. It must not import `database/sql`, the MCP SDK, `net/http`, or Docker types. Its `Repository` interface is shaped by domain needs; `internal/storage/sqlite` is an adapter implementing it.
- MCP Go SDK only imported by `internal/mcpadapter` (plus tests). Domain and service code never see SDK types; the moby Engine client (`github.com/moby/moby/client`, `.../api`) only by `internal/runtime/docker`, and `internal/runtime/hostaccess` depends only on `internal/server` types. Nothing outside those two packages sees Docker types.
- Gateway/router code (`internal/gateway`, `internal/tool`) must not execute SQL. Runtime providers (`internal/runtime/{remote,process,docker}`) must not decide public tool names.
- The database stores configuration and observed status only. Never persist live connections, MCP sessions, or process handles.

### Startup path

`cmd/agentnexus/main.go` -> `app.Run`:

```
config.Load -> JSON slog logger -> auth.NewAuthorizer(keys, auth.DefaultPolicy())
  -> sqlite.Open -> sqlite.Migrate(ctx, db, migrations.FS)
  -> sqlite.NewServerRepository / sqlite.NewToolRepository / sqlite.NewAuditRepository
  -> hostaccess.NewPolicy(cfg.Security.HostAccess.Mounts, docker socket paths)
     # compiled at the earliest failure point: a bad allow-list (relative path, host root,
     # duplicate host) refuses to start; an empty list is legal but blocks every host mount
  -> server.NewService (registry).WithPolicy(policy)   # policy also runs at Ensure time
  -> audit.NewRecorder + observability.NewAuditObserver + metrics.New
  -> runtime.NewManager(servers, remote.NewProvider(), process.NewProvider(ctx),
                        docker.NewProvider(docker.Options{Host, Policy, ConnectHost, PublishHost, StopGrace}))
     # ctx 是 NotifyContext。正常退出走分阶段关闭(defer runtimes.Close -> Provider.Close,
     # 进程组 SIGTERM 宽限后 SIGKILL;docker 容器 Stop+Remove);
     # ctx 取消只是兜底(exec 默认 Cancel 只 SIGKILL 直接子进程)。
     # 因为 defer 后进先出,runtimes.Close 先于 clients.Close,stop() 最后。
     # docker.NewProvider only parses the host locally: an unreachable daemon never blocks startup.
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
- Schema (`000003_audit_events.sql`): `audit_events` is append-only (`event_type`, `outcome` CHECK in success/error/cancelled, `actor_name`/`actor_role`, `request_id`, `asset_id` nullable FK -> `assets` ON DELETE SET NULL, `asset_name` as a redundant copy so events stay readable after the asset is gone, `target`, `runtime_id`, `error_code`, `duration_ms` nullable, `detail_json` default `'{}'`). Three indexes: `occurred_at DESC`, `(asset_id, occurred_at DESC)`, `(actor_name, occurred_at DESC)`. There is no read path in v0.1 - query the table with the `sqlite3` CLI.
- Conventions: timestamps are UTC RFC3339Nano strings, and `Service.Register` normalizes its clock to UTC so the value it returns renders identically to what a later read returns; booleans are 0/1 integers; IDs are strings assigned by the caller (the repository rejects an empty ID). Range limits on timeouts and `max_in_flight` exist both as SQL CHECK constraints and in `server.Validate`; keep them in sync.
- `ServerRepository.Create` writes `assets` + `mcp_servers` + `server_status` in one transaction. SQLite UNIQUE violations (extended code 2067) map to `server.ErrAlreadyExists`; `sql.ErrNoRows` maps to `server.ErrNotFound`. `WithClock` injects time for tests.

### Domain model (`internal/server`)

- `Server` = envelope fields + `Spec` (desired config) + `Status` (observed state). `Server.Revision` is the config version; `Status.ObservedRevision` is what the runtime has applied, and a mismatch means a reconcile is needed. `Spec.DesiredState` (running/stopped) and `Status.Phase` (pending/starting/ready/degraded/stopped/failed) are different things; do not conflate them.
- `RuntimeSpec` is a tagged union: `Type` plus exactly one non-nil payload (`Remote`, `Process`, `Docker`). Transport/runtime matrix: `streamable_http` → remote or docker; `stdio` → process or docker.
- `DockerSpec` rules: `image` is a single non-empty reference (no whitespace); every `command` element must be non-empty; `env` keys must be valid env names; mount `source`/`target` must be absolute and neither may be the host/container root; `networkMode` is `""`/`none`/`bridge` only; `memoryBytes`/`cpus` are 0 (= default) or inside the frozen ranges, defaulting to 256 MiB / 1.0; `port` is required in 1-65535 for `streamable_http` (and forbidden for `stdio`, as is `endpointPath`), `endpointPath` defaults to `/mcp`, and publishing is rejected with `networkMode: none`. Host-mount authorization is not a domain rule: `Service.WithPolicy(SpecPolicy)` injects the check, `checkPolicy` runs it only for docker specs, and `nil` policy (wiring bug) must be treated as unchanged behaviour, so `internal/server` stays free of filesystem/Docker knowledge.
- `Validate()` only checks and never mutates. Defaulting (namespace `default`, timeouts 5s/10s/60s, maxInFlight 16, enabled true, desired state running) happens once, in `Service.Register` in the same package, which then assigns the ID and timestamps, validates, and persists through the `Repository` interface. Validation failures wrap `ErrInvalid`; repository errors pass through unchanged. Rules: namespace/name match `^[a-z][a-z0-9-]{0,62}$` and are immutable after registration (rename = new server); remote endpoints are absolute http/https URLs with a host and no userinfo; process commands are absolute paths with args passed as `[]string` to `exec.CommandContext`, never through a shell; secrets go through `CredentialID`, never into headers, env, or URLs.
- `Repository` sentinel errors: `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict` (optimistic-lock failure when `UpdateSpec` is called with a stale `expectedRevision`), and `ErrNotRunnable` (the server is disabled or `desiredState=stopped`, so runtime work like a tool refresh is meaningless). `Server.Runnable()` is the single definition of that predicate; the SQLite adapter's `ListEnabled` is the repository-side listing the reconciler uses.
- `Status.Phase` has two writers, per the detailed design §3.4 state machine: `ProviderManager` records `ObservedRevision`/`LastSuccessAt` and moves the phase to `starting` (or `degraded` on runtime failure) but never claims `ready`; `tool.SyncService` writes `ready` only after the snapshot is published (or confirmed unchanged), and writes `degraded` + `consecutive_failures+1` when a refresh fails, leaving the previous `active` snapshot readable. Never write `ready` before the catalog is reloaded - `ready` means "runtime applied the current revision AND the tool catalog is published". `Status.CreateInput()` is the shared status -> `CreateStatusInput` conversion both writers use.

### Auth (`internal/auth`, `internal/edge/httpapi/authmw.go`)

- Every request passes through `withAuth` before the mux; `Authorize(path, presented)` is the single decision point, so business handlers (and the MCP protocol layer) never see unauthenticated traffic.
- Roles: `admin` (management API + `/mcp`), `agent` (`/mcp` only). `auth.DefaultPolicy()` is the line policy: `/health/live` and `/health/ready` are public by exact match, `/metrics` and `/api/v1/` require admin, `/mcp` accepts both, and any unregistered path is denied (403) even for admin — forgetting to register a new route fails closed.
- Credentials come from config, not the database: `security.apiKeys[]` with `key` XOR `keyEnv` (`keyEnv` resolution happens in `config.loadYAML`; an unset or empty env var is fatal). Role/name/secret/duplicate validation lives only in `auth.NewAuthorizer`, and a bad key table prevents startup.
- Header rules (`presentedSecret`): `Authorization: Bearer <key>` (scheme case-insensitive) wins; a non-Bearer scheme does not fall back to `X-API-Key`; otherwise `X-API-Key`; nothing → 401.
- Failures reuse the error envelope with two codes: `unauthenticated` (401, adds `WWW-Authenticate: Bearer realm="agentnexus"`) and `permission_denied` (403, no challenge header). Messages are fixed and never echo the credential.
- An empty `apiKeys` list is valid: the service starts, logs `no API keys configured; all requests will be rejected`, and everything except the probes returns 401. `Options.Authenticator == nil` (wiring bug) fails closed for all paths, probes included.
- `auth.WithPrincipal`/`PrincipalFrom` is the only seam for reading the caller: `withAuth` writes it, `observability.EnrichAuditInput` and the request log read it.

### Audit and request logging (`internal/audit`, `internal/observability`, `migrations/000003_audit_events.sql`)

- Two different facts, two different destinations: the per-request structured log (`internal/edge/httpapi/requestlog.go`) is ephemeral and answers "what did this HTTP request do"; `audit_events` is durable and answers "who changed or invoked what". A request log line never substitutes for an audit row and vice versa.
- `internal/audit` is domain-only (no `net/http`, SQL, SDK, or `slog`): `EventType` (nine frozen values; unknown types are rejected), `Outcome` (success/error/cancelled), `Input` (what emitters build) vs `Event` (what `Recorder.Record` persists - it assigns the ID and a UTC `OccurredAt`), `Detail()` as the only detail builder, and `Repository.Append` as the single write path. `audit.ErrorCode(err)` maps domain sentinels to the detailed design §11.1 codes in exactly one place; `context.Canceled` deliberately maps to an empty code (cancellation is an `Outcome`, not an error code).
- `Recorder.Record` never changes a business result: invalid input returns `ErrInvalidEvent` (a programming error) and storage errors pass through; every caller logs and continues. `Detail` is redacted structurally - `redactDetail` rejects non-objects, drops deny-listed keys (case/`-`/`_` insensitive: `endpoint`, `url`, `command`, `args`, `env`, `headers`, `path`, `credential`, `token`, `secret`, ...) with their values, and caps depth at 16 and size at 4 KiB. To keep a new field out of the audit table, add its key to the deny-list.
- Emission points: HTTP handlers write `server.registered` / `server.refresh_requested` through `httpapi.Options.Auditor`; `app.Lifecycle` writes `tool.snapshot_published` / `tool.sync_failed`; and `observability.AuditObserver` implements both `gateway.CallObserver` (`tool.called`) and `runtime.Observer` (`runtime.started`/`restarted`/`stopped`/`failed`). `internal/observability` is the only package that knows audit + auth + gateway + runtime at once; `runtime`, `server`, and `tool` only declare consumer-side observer interfaces and never import `internal/audit` (only `gateway` does, for `Outcome`/`ErrorCode`).
- Actor and request id come from `observability.EnrichAuditInput`, which reads `auth.PrincipalFrom` and `RequestIDFrom` from the request context. Background work (startup replay, periodic sweep) has no request context, so those rows legitimately have empty `actor_*`/`request_id`; never invent values for them.
- Middleware order in `httpapi.NewHandler` is `withRequestLog(withAuth(mux))`: the request log must wrap auth so 401/403 responses still get a `request_id`, status, duration, and `error_code`. `withRequestLog` reuses an inbound `X-Request-Id` only when it matches `^[A-Za-z0-9._-]{1,64}$` (anything else is regenerated, preventing log injection) and always echoes the effective value in the response header.
- The log line is assembled from a `*requestRecord` stored in the context; `withAuth` writes the principal and `writeError` writes the error code. `statusWriter` records status and bytes and **must** keep its `Unwrap`, because the MCP streamable HTTP handler reaches the `Flusher` through `http.NewResponseController`.
- Authentication and authorization failures are logged but never written to `audit_events`; the audit table records business facts, not rejected traffic.

### Metrics (`internal/metrics`, `internal/observability/fanout.go`, `internal/edge/httpapi/metrics.go`)

- `internal/metrics` is an adapter: `Metrics` owns a private `prometheus.Registry` (never the default global one, so tests cannot pollute each other and no third-party collector leaks into the output) with the six instrument families — `mcp_requests_total{method,route,status}`, `mcp_request_duration_seconds{method,route}` (histogram, `prometheus.DefBuckets`), `mcp_request_errors_total{method,route,error_code}`, `mcp_tool_calls_total{server,tool}`, `mcp_tool_call_failures_total{server,tool,error_code}`, `mcp_runtime_restarts_total{server}` — plus the `mcp_server_health_status{server,phase}` gauge. It knows `prometheus` and `net/http` but no SQL, SDK, or domain-internal types: `ServerLister` (consumer-defined, `ListEnabled`) is its only storage dependency. Empty label values fall back to `unknown` in one place (`labelValue`/`errorCodeLabel`).
- Family semantics: labels are always bounded — `route` is the `net/http` pattern (`/api/v1/mcp-servers/{id}`, `unmatched` for unregistered paths), so asset ids and tool names never become a cardinality leak. `ObserveRequest` with an empty `errorCode` deliberately does **not** touch the errors family (empty label values are forbidden). `mcp_tool_calls_total` counts every reached `tools/call`; `IsError=true` results count as calls but never as failures, because the failure family is for gateway/runtime failures (`runtime.ErrProviderNotReady`, …), not for tool-declared errors. Only `runtime.RuntimeRestarted` writes `mcp_runtime_restarts_total`; a first start is not a restart.
- Vec families are lazily created: a family with no samples is absent from the scrape output. Never pre-seed fake series to make a family "always visible"; assert family definitions in unit tests with real samples instead.
- The health gauge is pull-based: `Metrics.ServeHTTP` first calls `refreshHealth` (2s timeout, `ListEnabled`) under `scrapeMu`, which resets the gauge and re-fills `mcp_server_health_status{server,phase}` with 1 only for `PhaseReady` — so it can never go stale and deleted/disabled servers disappear. A read failure answers 500 (never a silently empty scrape) and logs the cause.
- `observability.MultiRuntimeObserver` / `MultiCallObserver` fan out one observer call to several sinks (audit + metrics) in declaration order, skipping nil elements so wiring may omit a consumer. They deliberately do not add locking, error handling, or panic isolation: both sinks are counting/appending-only and must stay that way, which is why the call happens synchronously on the request and reconcile paths. The only panic guard is `observeRequest` in the HTTP layer, where metrics run after the response has been written.
- The endpoint is `internal/metrics.Metrics` itself (`http.Handler`); `httpapi` only registers it via `Options.Metrics` (`GET /metrics`, skipped when nil) and observes requests via `Options.RequestMetrics`. Because the route is registered before the auth wrapper, `auth.DefaultPolicy` must list it: admin 200, agent 403, no credential 401. `Content-Type` comes from `promhttp` and must not be overridden.
- Request observation happens in `withRequestLog`, after the response is written: the first scrape therefore cannot contain its own `/metrics` series. This is expected, not a bug.

### HTTP API (`internal/edge/httpapi`)

- `NewHandler` builds the mux, wraps it in `withRequestLog` (outermost) and `withAuth`, and depends on consumer-defined interfaces (`ServerRegistry` with Register + Get, `ServerRefresher`, `Authenticator`, plus `AuditRecorder`), not on the concrete service. Tests inject the real service over a temp SQLite database, or a stub for failure paths; `Options.Auditor == nil` silently skips management-action audit events.
- Routes: `GET /health/live`, `GET /health/ready`, `GET /metrics` (admin only, registered from `Options.Metrics`), `POST /api/v1/mcp-servers` (202 Accepted plus `Location`, because runtime start is asynchronous by design), `GET /api/v1/mcp-servers/{id}`, `POST /api/v1/mcp-servers/{id}:refresh-tools` (202, same async semantics). The action route is only registered when a `ServerRefresher` is supplied; `net/http` requires a wildcard segment to own a whole path segment, so the route is `POST .../{rest...}` and the handler splits `<id>:<action>` (`parseAction` in `mcp_server_handlers.go`). Unknown action or unknown id → 404; a non-runnable server (disabled or desired state stopped) → 409 `conflict` via `server.ErrNotRunnable`.
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
- `internal/runtime/remote` speaks streamable HTTP; `internal/runtime/process` runs local stdio children; `internal/runtime/docker` runs stdio children inside containers. `gateway` (call path) and `tool` (refresh path) share one `Manager`, so both reuse the same instance per server.
- Process semantics: the provider owns the child (`exec.CommandContext` with the application ctx, `Setpgid` so `Stop` signals the whole group, no shell), the pipes (stdin/stdout) and stderr as a bounded ring buffer surfaced by `Logs` (snapshot semantics; `Follow` is rejected until implemented). MCP owns stdin/stdout, so child logs must go to stderr. Sessions borrow `runtime.Streams{Stdin,Stdout}` wrappers whose `Close` only marks "this connection ended" - it never closes the child's fds. A closed stream (or an exited child) makes `Inspect` fail with `ErrInstanceUnavailable`, which is the manager's reap trigger: `Stop` (kill) then `Ensure` (new pid). Instance IDs embed the pid, so a restarted backend gets new session-cache keys.
- `Inspect` returning an error is the *only* way the manager learns an instance is gone; never close the fds from the adapter or the gateway.
- Docker semantics (`internal/runtime/docker`): containers are owned, not adopted blindly - every container carries `agentnexus.server-id` and `agentnexus.revision`, and the name is always `agentnexus-<server id>`. `Ensure` lists containers by the server-id label: a *running* container whose revision label matches is reused as-is; every other labelled container of that server is stopped and removed; a name conflict with an unlabelled leftover (previous crash, manual `docker run --name`) is removed too, so restarting the control plane after `SIGKILL` takes over its own container instead of failing. A missing image is pulled (bounded by the connect timeout). `Cmd`/`Env`/`Mounts`/`Resources`/`PortBindings` come from the spec, and the policy is re-checked at `Ensure` time (registration-time validation is not a substitute for the runtime check).
- stdio is `AttachStdin/Stdout/Stderr` + `OpenStdin` over the Engine API, demultiplexed with `stdcopy` into a stdout pipe (`internal/runtime/docker/streams.go`); stderr is dropped because the daemon's log driver still holds it for `Logs`. `streamable_http` publishes `port` to `publishHost` (default `127.0.0.1`) and `Ensure` polls `connectHost:port+endpointPath` until it answers, failing with the last probe error on timeout. `ConnectTarget` returns `URL` or `Streams` accordingly, so the SDK adapter and the session manager need no docker knowledge.
- Config keys under `runtime.docker`: `host` (defaults to `DOCKER_HOST`, then the platform socket), `connectHost`, `publishHost` (both default `127.0.0.1`), `stopGrace` (default 5s), plus `logBuffer` in code only (default 64 KiB). `Stop` = `ContainerStop(grace)` + forced `ContainerRemove`, `Close` = the same for every owned container then close the Engine client, and both are idempotent (an unknown or already-forgotten instance returns nil); an unreachable daemon is only discovered at `Ensure`/`Inspect`, never at startup. `Logs` demultiplexes stdout+stderr and supports `Follow` (unlike `process`, which rejects it): non-follow returns the last `logBuffer` bytes, follow hands back a pipe the caller closes.
- `internal/runtime/hostaccess` compiles `security.hostAccess.mounts` into a `hostaccess.Policy` satisfying `server.SpecPolicy`: entries are resolved (`filepath.Clean` + `EvalSymlinks`) against an absolute allow-list and a fixed deny-list (`/etc`, `/proc`, `/sys`, `/root`, `/var/run/docker.sock`, docker/ssh sockets, plus the configured `runtime.docker.host` paths), and `CheckDocker` also rejects a read-write mount for a read-only entry, a mount outside every container root, and a non-empty `env` in a docker spec whose key name matches the secret pattern (secrets belong in `CredentialID`).

### Gateway and MCP adapters (`internal/gateway`, `internal/mcpclient`, `internal/mcpadapter`)

- `gateway.NewVirtualServer(catalog, caller)` exposes the aggregated catalog as a single MCP server and routes `tools/call` by looking the public name up in the catalog, then calling the backend through `mcpclient`.
- `mcpclient` is the domain-facing session layer: `Manager.Acquire(server, instance)` returns a `SessionLease` keyed by server/revision/instance ID (bounded by `maxInFlight`), `Invalidate` drops sessions after connection failures, `Close` drains everything. `Acquire` also closes and drops the sessions of other instances of the same server, so a restarted backend cannot leave a stale session behind. It depends only on `runtime.ConnectTarget`.
- `mcpadapter` is the only place touching the official SDK: `mcpadapter.NewConnector()` (client side) and `mcpadapter/server` (server side, backing the `/mcp` endpoint). `Connector.Connect` accepts `streamable_http` (needs `URL`) and `stdio` (needs `Target.Streams`, wrapped as `mcp.IOTransport`); anything else is `unsupported MCP target`. The adapter never kills processes - closing the session only closes the stream wrappers, and the manager reaps the instance afterwards.

### App wiring (`internal/app`)

- `app.Run` is the composition root. It also runs `Lifecycle`, which decorates the `ServerRegistry` handed to the HTTP layer: a successful `Register` enqueues the new ID (FIFO queue + dedup set, one worker goroutine) and the sync runs asynchronously on the application context, so the response stays `202`/`pending` and sync failures are only logged. `Lifecycle.Close` stops accepting triggers, cancels in-flight syncs and waits for the worker.
- One `audit.Recorder` is built from `sqlite.NewAuditRepository` and handed to all three consumers: the HTTP layer (`Options.Auditor`), `Lifecycle` (`LifecycleOptions.Auditor`), and `observability.NewAuditObserver`, which is installed on both `runtime.Manager` (`WithObserver`) and `gateway.ToolCaller` (`WithObserver`). No other code path writes audit rows.
- A failed sync leaves the server `degraded` with the previous snapshot still readable; the periodic sweep retries it, and `POST ...:refresh-tools` triggers it immediately.
- Metrics wiring: one `metrics.New(serverRepo, logger)` instance is passed both as `httpapi.Options{Metrics: meter, RequestMetrics: meter}` and as `metrics.NewObserver(meter)` inside the two fan-out observers, so the HTTP surface and the domain observers fill the same registry.

### Package layout (from the detailed design)

Done: `internal/tool`, `internal/runtime/{remote,process,docker,hostaccess}`, `internal/mcpclient`, `internal/mcpadapter`, `internal/gateway`, `internal/auth`, `internal/audit`, `internal/observability`, `internal/metrics`. Runtime types stop at `remote`/`process`/`docker` (the detailed design's frozen `RuntimeType` set; Kubernetes and multi-node scheduling are post-v0.1 and have no package yet), and the design docs' out-of-scope lists cover the remaining v0.1 gaps (management actions PUT/start/stop/restart, credential resolution/injection, log query API, backend health probing, ready-server drift polling). Place new code in these packages rather than inventing new top-level ones.

## Conventions

- The design docs in `docs/` (gitignored, local only) are the architecture source of truth: "AgentNexus 整体架构设计 v1.0" and "AgentNexus MCP Core Detailed Design v0.1". When repo code and the docs disagree, point out the conflict instead of silently picking one.
- Code comments and docs are written in Chinese; identifiers, error strings, and log messages are English.
- Errors wrap as `fmt.Errorf("verb noun: %w", err)`. Externally visible errors must not leak credentials, endpoints, host paths, or env vars.
- Tests are table-driven where possible. SQLite tests open a real file under `t.TempDir()` and run the production migrations.
- GitHub Flow: short-lived `feat/*` branches off `main`, Conventional Commit messages (`feat: ...`), merged to `main` through pull requests. Keep each PR to one independently verifiable step and run `go test ./...`, `go vet ./...`, and `gofmt -l .` before opening it.
