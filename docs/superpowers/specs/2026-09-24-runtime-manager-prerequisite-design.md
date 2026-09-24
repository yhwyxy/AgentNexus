# RuntimeManager prerequisite design

Date: 2026-09-24

## Context

The detailed design lists the Runtime Provider/Manager interfaces in PR-2 and requires `RuntimeManager.EnsureReady` in the PR-4 tool-call path. The merged code currently has `runtime.Provider` and `RemoteProvider`, but no Manager. PR-4 is intentionally blocked on a separate prerequisite PR, as selected by the user. PR-5 remains responsible for Process Runtime, authorization, audit, metrics, and graceful shutdown.

The current `server.Repository` already persists observed status through `UpdateStatus`; there is no runtime-instance repository. Live connections and handles must not be persisted.

## Goal

Add a useful, SDK-independent Runtime Manager that selects Providers and coordinates readiness, stopping, and reconciliation for the current Remote runtime. Keep the change separate from the later PR-4 Virtual MCP Router.

## Design

### API and construction

Keep the detailed-design contract:

```go
type Manager interface {
    EnsureReady(context.Context, server.Server) (Instance, error)
    Stop(context.Context, server.ID) error
    Reconcile(context.Context, server.ID) error
}
```

Add a constructor that accepts `server.Repository` and a set of `Provider` implementations. It indexes providers by `Provider.Type()` and rejects nil dependencies, an empty provider set, or duplicate runtime types at construction. The returned implementation is internal to `internal/runtime`; no MCP SDK type is introduced.

### Instance ownership and concurrency

The manager owns an in-memory map from `server.ID` to the active `Instance` and its Provider. It never writes runtime handles, targets, headers, or live connections to SQLite. A keyed lock serializes Manager lifecycle operations for one Server while allowing different Servers to proceed concurrently. Lock entries are released when no operation uses them.

### EnsureReady

1. Acquire the per-Server lock and reload the current Server by ID from the repository. This prevents a stale caller snapshot from bypassing a concurrent disable or revision change.
2. Reject disabled Servers and any desired state other than `running`; these expected state errors do not mark the runtime degraded.
3. Select the Provider matching `Spec.Runtime.Type`; an unregistered type returns an explicit error.
4. Reuse a cached instance only when its revision matches. Call `Provider.Inspect`; reuse only a successful running instance for the same Server and revision. If inspection fails or reports a stale/non-running instance, stop it using the Provider stored with that cached instance before creating a replacement. If stopping fails, retain the old entry and return the error.
5. Call `Provider.Ensure`, validate the returned instance belongs to the Server/provider/current revision and has phase `running`; if validation fails, attempt `Provider.Stop` on that returned instance before reporting the failure. Cache only a valid instance.
6. Persist ready status using `UpdateStatus`: phase ready, observed revision from the instance, UTC last-success time, and zero consecutive failures. If status persistence fails after a successful Ensure, retain the instance and return the persistence error; a later call reuses it and retries status persistence rather than creating a duplicate.

Provider/inspection failures that cannot be recovered by a successful replacement update degraded status with a generic non-secret message and increment the current consecutive-failure count. A failed cached `Inspect` followed by successful stop/re-Ensure is a recovered path and persists ready status. Preserve existing observed revision, health time, and last-success time on unrecovered failure. Return wrapped errors to the caller; do not copy endpoint, credentials, or raw provider error text into persisted status.

### Stop and Reconcile

`Stop` serializes by Server ID, loads the current Server, and calls the Provider associated with the cached instance. Successful stop removes the cache entry and persists stopped status; stop failure retains the entry and persists degraded status. Stopping a Server with no cached instance is idempotent and persists stopped status without invoking a Provider.

`Reconcile` serializes by Server ID, loads the current Server, then runs the same internal ensure or stop path according to `Enabled` and `Spec.DesiredState`. Internal helpers avoid reacquiring the same lock. Reconcile is an on-demand operation only; this change adds no background loop or health checker.

### Status and errors

Use the existing `server.Status`/`server.CreateStatusInput` fields and phase constants. Status messages are fixed generic English text, not raw Provider errors. State-policy errors are distinguishable from Provider/runtime failures; no HTTP error mapping is added here.

If `UpdateStatus` fails, return the repository error wrapped with the operation. Do not discard a successful live instance or claim persisted readiness. Retain an ensured instance after a ready-status write failure so a later Ensure retries status persistence without another Ensure call. After Provider.Stop succeeds, remove the cache entry even if the stopped-status write fails; a later Stop retries the status write without invoking Provider.Stop again.

## Scope exclusions

- No Virtual MCP Server, `/mcp` route, gateway, or `tools/list`/`tools/call` behavior.
- No Process or Docker Provider, credential/Secret Provider, authentication or authorization, audit, metrics, or graceful-shutdown wiring.
- No migration or runtime-instance table; no persisted live instance/connection/handle.
- No background reconciler, periodic health check, or automatic retry loop.

## Verification

Use deterministic fake Providers in `internal/runtime` tests. Cover provider selection and constructor errors; enabled/desired-state guards; same-Server concurrent Ensure deduplication and cross-Server concurrency; valid cached-instance inspection; revision replacement and stale-instance stop failure; invalid Ensure result; Stop idempotence and failure retention; Reconcile state selection; status transitions; and retry after status persistence failure without duplicate provider creation.

Run focused runtime tests, `gofmt`, `go test ./...`, `go build ./...`, and `go vet ./...` before committing the prerequisite branch. PR-4 begins only after this prerequisite branch is merged into `main`.
