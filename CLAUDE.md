# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

AgentNexus is a self-hosted "Agent Capability Control Plane" written in Go. v0.1 is an MCP gateway/aggregator: register backend MCP servers (remote streamable HTTP, stdio process, Docker), health-check them, aggregate their `tools/list`, and route `tools/call` through a single `/mcp` endpoint using `serverName.toolName` public names. So far only the bootstrap, SQLite storage, and the MCP Server domain model/repository exist; the gateway, runtime providers, and MCP SDK adapters are not written yet.

Module: `github.com/yhwyxy/AgentNexus`, Go 1.27. Dependencies are deliberately few: `modernc.org/sqlite` (pure Go, no cgo), `go.yaml.in/yaml/v3`, and the official `github.com/modelcontextprotocol/go-sdk`. HTTP routing uses `net/http` method patterns (`"GET /health/live"`); tests use only the standard `testing` package.

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
curl -s localhost:8080/api/v1/mcp-servers/<id>   # 200; 404 for unknown id
```

Config precedence: built-in defaults → YAML → `AGENTNEXUS_HTTP_ADDRESS` / `AGENTNEXUS_SHUTDOWN_TIMEOUT` / `AGENTNEXUS_DATABASE_PATH` → validate. A missing YAML file is silently ignored; a present-but-invalid one is fatal. `configs/dev.yaml` currently fails validation (`shutdownTimeout: 0s`), so do not use it as-is.

Phase-0 MCP SDK experiments live in `examples/` (gitignored, so absent on a fresh clone):

```bash
go run ./examples/phase0-add-server -http :8080   # streamable HTTP; omit -http for stdio
go run ./examples/phase0-add-client               # connects to localhost:8080/mcp
```

## Architecture

### Layering rule (frozen by the design docs)

```
edge (HTTP / MCP handlers) → application service → domain interfaces → adapters (sqlite, runtime, mcpadapter)
```

- `internal/server` is the domain layer. It must not import `database/sql`, the MCP SDK, `net/http`, or Docker types. Its `Repository` interface is shaped by domain needs; `internal/storage/sqlite` is an adapter implementing it.
- The MCP Go SDK is only allowed inside the (future) `internal/mcpadapter` package. Domain and service code never see SDK types.
- Gateway/router code (future `internal/gateway`, `internal/tool`) must not execute SQL. Runtime providers (future `internal/runtime/{remote,process,docker}`) must not decide public tool names.
- The database stores configuration and observed status only. Never persist live connections, MCP sessions, or process handles.

### Startup path

`cmd/agentnexus/main.go` → `app.Run`: load config → JSON `slog` logger → `sqlite.Open` → `sqlite.Migrate(ctx, db, migrations.FS)` → `sqlite.NewServerRepository` → `server.NewService` → `httpapi.NewHandler` wrapped by `httpapi.NewServer` → block until SIGINT/SIGTERM, then `Shutdown` with the configured timeout. A migration failure prevents the HTTP server from starting.

### Storage (`internal/storage/sqlite`, `migrations/`)

- `Open` resolves the path to absolute, builds a `file://` DSN, and applies the required per-connection baseline through DSN params: `_foreign_keys=on`, `_busy_timeout=5000`, `_journal_mode=WAL`, `_synchronous=NORMAL`. Verify PRAGMAs in tests through a real `Open` connection; the `sqlite3` CLI opens its own connection and will not show connection-level settings.
- `Migrate` accepts any `fs.FS`, runs `NNNNNN_name.sql` files in version order, one transaction each, and records them in `schema_migrations`. Production migrations are embedded via `migrations.FS` (`//go:embed *.sql`); tests use `testing/fstest.MapFS` for synthetic ones.
- Schema (`000001_mcp_core.sql`): `assets` is a generic Kubernetes-style envelope (`kind`, `namespace`, `name`, `labels_json`, `enabled`, `revision`; UNIQUE on namespace+kind+name). `mcp_servers` is the 1:1 kind-specific extension keyed by `asset_id`, holding `runtime_spec_json` and timeouts in milliseconds. `server_status` holds observed runtime state. `credentials` is referenced by `mcp_servers.credential_id`. Future kinds (Skill, Agent, Workflow) are meant to reuse `assets`.
- Conventions: timestamps are UTC RFC3339Nano strings, and `Service.Register` normalizes its clock to UTC so the value it returns renders identically to what a later read returns; booleans are 0/1 integers; IDs are strings assigned by the caller (the repository rejects an empty ID). Range limits on timeouts and `max_in_flight` exist both as SQL CHECK constraints and in `server.Validate`; keep them in sync.
- `ServerRepository.Create` writes `assets` + `mcp_servers` + `server_status` in one transaction. SQLite UNIQUE violations (extended code 2067) map to `server.ErrAlreadyExists`; `sql.ErrNoRows` maps to `server.ErrNotFound`. `WithClock` injects time for tests.

### Domain model (`internal/server`)

- `Server` = envelope fields + `Spec` (desired config) + `Status` (observed state). `Server.Revision` is the config version; `Status.ObservedRevision` is what the runtime has applied, and a mismatch means a reconcile is needed. `Spec.DesiredState` (running/stopped) and `Status.Phase` (pending/starting/ready/degraded/stopped/failed) are different things; do not conflate them.
- `RuntimeSpec` is a tagged union: `Type` plus exactly one non-nil payload (`Remote`, `Process`, `Docker`). Transport/runtime matrix: `streamable_http` → remote or docker; `stdio` → process or docker.
- `Validate()` only checks and never mutates. Defaulting (namespace `default`, timeouts 5s/10s/60s, maxInFlight 16, enabled true, desired state running) happens once, in `Service.Register` in the same package, which then assigns the ID and timestamps, validates, and persists through the `Repository` interface. Validation failures wrap `ErrInvalid`; repository errors pass through unchanged. Rules: namespace/name match `^[a-z][a-z0-9-]{0,62}$` and are immutable after registration (rename = new server); remote endpoints are absolute http/https URLs with a host and no userinfo; process commands are absolute paths with args passed as `[]string` to `exec.CommandContext`, never through a shell; secrets go through `CredentialID`, never into headers, env, or URLs.
- `Repository` sentinel errors: `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict` (optimistic-lock failure when `UpdateSpec` is called with a stale `expectedRevision`).

### HTTP API (`internal/edge/httpapi`)

- `NewHandler` builds the mux and depends on a consumer-defined `ServerRegistry` interface (Register + Get), not on the concrete service. Tests inject the real service over a temp SQLite database, or a stub for failure paths.
- Routes: `GET /health/live`, `GET /health/ready`, `POST /api/v1/mcp-servers` (202 Accepted plus `Location`, because runtime start is asynchronous by design), `GET /api/v1/mcp-servers/{id}`.
- Wire format is defined by the DTOs in `dto.go`; domain types are never serialized directly. Timeouts travel as whole seconds (`connectSeconds` etc.), the runtime is a tagged union that only emits the active variant, and `desiredState` is not accepted on registration.
- Errors use one envelope, `{"error":{"code":...,"message":...}}`, with codes `invalid_argument` (400), `not_found` (404), `conflict` (409), `internal` (500). Domain sentinels are mapped in exactly one place, `writeDomainError`. Unexpected errors are logged with detail and returned as an opaque "internal error".
- Request bodies are capped at 1 MiB and unknown JSON fields are rejected.

### Planned package layout (from the detailed design)

`internal/tool` (naming, schema digest, catalog, sync), `internal/gateway` (virtual MCP server + router), `internal/runtime/{remote,process,docker}`, `internal/mcpclient`, `internal/mcpadapter`, `internal/auth`, `internal/audit`. Place new code in these packages rather than inventing new top-level ones. Public tool names are `serverName.toolName`, resolved through a stored `publicName → Route` mapping, never by splitting the string.

## Conventions

- The design docs in `docs/` (gitignored, local only) are the architecture source of truth: "AgentNexus 整体架构设计 v1.0" and "AgentNexus MCP Core Detailed Design v0.1". When repo code and the docs disagree, point out the conflict instead of silently picking one.
- Code comments and docs are written in Chinese; identifiers, error strings, and log messages are English.
- Errors wrap as `fmt.Errorf("verb noun: %w", err)`. Externally visible errors must not leak credentials, endpoints, host paths, or env vars.
- Tests are table-driven where possible. SQLite tests open a real file under `t.TempDir()` and run the production migrations.
- GitHub Flow: short-lived `feat/*` branches off `main`, Conventional Commit messages (`feat: ...`), merged to `main` through pull requests. Keep each PR to one independently verifiable step and run `go test ./...`, `go vet ./...`, and `gofmt -l .` before opening it.
