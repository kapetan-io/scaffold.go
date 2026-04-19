# Scaffold Tech Spec

_Design document: [`docs/scaffold-design.md`](scaffold-design.md)_

## Overview

Scaffold is a Go framework for building HTTP API services. This document specifies the implementation internals, cross-cutting technical decisions, and test surfaces that the design document leaves open. It does not re-state the public API contract — refer to the design document for mental model, lifecycle semantics, and public types.

This spec covers:

- Module layout and dependencies
- Internal data structures for `Bindings`, `Binding`, `Cleaner`
- Lifecycle implementation (signal handling, port-readiness, context enrichment, shutdown)
- Configuration provider implementations
- HTTP server wiring and timeout mapping
- TLS integration (via `tackle/autotls`)
- Built-in middleware and logger
- Metrics middleware behavior
- Thread-safety contracts
- Test surfaces

## Packaging & Dependencies

### Module

- Module path: `github.com/kapetan-io/scaffold`
- Go version: matches the latest stable release at implementation time (≥ Go 1.23 required for `r.Pattern`).

### Package layout

```
scaffold/             // core: Daemon, DaemonConfig, Binding, Bindings,
                      //   Cleaner, ConfigProvider implementations,
                      //   PanicRecovery, Health/Ready handlers,
                      //   Serve/Start/Instance, port-readiness helper
scaffold/prometheus   // sprometheus: HTTPMetrics middleware
```

The design document mentioned `scaffold/autotls` and `scaffold/internal/colorlog`; both are replaced by direct use of `github.com/kapetan-io/tackle/autotls` and `github.com/kapetan-io/tackle/color`. Port-readiness verification is a single unexported helper in core, not a subpackage.

### Direct dependencies

Core `scaffold` package:

- Standard library only, except:
- `github.com/kapetan-io/tackle` — provides `autotls`, `color`, and `set`. Lean module (transitively depends only on `testify` and `golang.org/x/net`).

`scaffold/prometheus` subpackage:

- `github.com/prometheus/client_golang` — quarantined here so users who don't need Prometheus metrics don't pull it in.

Test-only:

- `github.com/stretchr/testify`

### Rationale

The `tackle` dependency is accepted (vs. copying code into `scaffold/autotls`) because:

- Same-org maintenance (`kapetan-io`).
- Brings three useful utilities (TLS config, slog color handler, `set.Default`).
- Transitive dep surface is minimal.

## Lifecycle Internals

### Signal handling (in `Serve`)

`Serve` registers a buffered signal channel for `SIGTERM` and `SIGINT` via `signal.Notify`, then blocks on a `select` between that channel and the caller's `ctx.Done()`. Whichever fires first triggers shutdown, and the log line records which — `reason=signal` plus the signal name, or `reason=context`.

`signal.NotifyContext` is not used because it collapses both paths into one `ctx.Done()`, losing which event triggered shutdown.

### Context construction

`Serve` derives three contexts from `context.Background()`:

- **Start context** — `context.WithTimeout(Background, OnStartTimeout)` if non-zero, else `Background`. Passed to `OnStart`.
- **Shutdown context** — `context.WithTimeout(Background, OnStopTimeout)` if non-zero, else `Background`. Used for `http.Server.Shutdown`, `OnStop`, and `Cleaner.Clean` in sequence.

`Start` (test entry point) uses the caller-supplied context for `OnStart`, and the context passed to `Instance.Stop(ctx)` for shutdown steps. `OnStartTimeout` / `OnStopTimeout` are ignored in test mode.

Both the Start and Shutdown contexts are enriched with framework services (see below) before being handed to `OnStart` / `OnStop`. Enrichment always uses `context.WithValue` on top of whatever parent context is in play — in `Serve`, that parent is the `Background`-derived timeout context; in `Start`, it is the caller-supplied context. The caller's cancellation and any deadline they attached remain in effect.

### Context enrichment

Scaffold stores `sc.Config`, `sc.Secrets`, and `sc.Log` on the lifecycle contexts using a set of private, unexported context keys (one per field). The public accessors are:

```go
func GetConfig(ctx context.Context) ConfigProvider
func GetSecrets(ctx context.Context) ConfigProvider
func GetLogger(ctx context.Context) *slog.Logger
```

Each returns `nil` when the key is absent. Nil returns mean the accessor was called off a non-lifecycle context — a programmer error that surfaces as a nil dereference at the call site, which is the correct diagnostic.

Enrichment happens once: immediately before `OnStart` is invoked (applied to the Start context) and again immediately before the shutdown sequence begins (applied to the Shutdown context). Per-request contexts are **not** enriched.

### Port-readiness verification

After `net.Listen` succeeds and the serve goroutine is spawned, scaffold probes the listener address before returning from `Start` / `Serve`. This is defensive — `net.Listen` already marks the socket as accepting at the kernel level, but the probe also catches sandbox or firewall misconfigurations where `Listen` succeeds but `Dial` fails.

The probe is an unexported helper inside the core `scaffold` package, with signature:

```go
func waitForListener(addr net.Addr, timeout time.Duration) error
```

Behavior:

- Calls `net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)` in a loop.
- Sleeps 10 ms between attempts.
- Returns nil on first successful dial.
- On timeout: `fmt.Errorf("timeout while waiting for server to accept connections on port %s", addr)`.

Default timeout is 5 seconds. Not configurable.

Port-wait runs **after** `OnStart` returns, as part of opening each binding. It is not subject to `OnStartTimeout` — that timeout bounds only `OnStart` itself. Total `Serve`/`Start` return latency from the caller's perspective is bounded by `OnStartTimeout + (5s × number of bindings)` in the worst case; in practice the probe succeeds on the first attempt after a few milliseconds.

### Shutdown sequencing

Shutdown runs strictly serially, in **reverse `Add` order** — the mirror of startup. This intentionally trades throughput for predictability: a slow binding cannot starve other bindings because there is no other binding to race against it.

Three distinct teardown paths exist, all using the same serial-reverse sequencing rule:

**Normal shutdown** (SIGTERM / SIGINT / ctx cancel after all bindings are open):

```
1. Log "shutdown initiated" with reason.
2. For each binding in reverse Add() order:
     - If HTTP:      http.Server.Shutdown(shutdownCtx).
     - If ServeFunc: scaffold takes no action. The user's Cleaner function
                     is responsible for stopping the foreign serve loop.
3. OnStop(shutdownCtx).
4. Cleaner.Clean(shutdownCtx) — LIFO.
5. Log "daemon stopped".
```

**OnStart error** (OnStart returned non-nil):

```
1. Log the OnStart error.
2. OnStop(shutdownCtx) — lets the daemon unwind partial in-memory state.
3. Cleaner.Clean(shutdownCtx) — unwinds any Cleaner.Add calls made before
   OnStart failed.
4. Return the OnStart error from Serve/Start. No ports were opened.
```

**Binding-open failure** (net.Listen failed on the Nth binding):

```
1. Log the bind error with the binding name.
2. For each already-opened binding (1..N-1) in reverse order:
     - If HTTP:      http.Server.Shutdown(shutdownCtx).
     - If ServeFunc: no action (user Cleaner handles it).
3. OnStop(shutdownCtx).
4. Cleaner.Clean(shutdownCtx).
5. Return the bind error from Serve/Start.
```

All paths share the single `OnStopTimeout`-derived context (Serve) or the context passed to `Instance.Stop` (Start).

If any step in any path returns an error, scaffold logs it and continues to the next step. A failed `OnStop` never prevents `Cleaner.Clean` from running. A failed `http.Server.Shutdown` never prevents later bindings from shutting down or subsequent steps from running.

**ServeFunc listener ownership.** The listener is created by scaffold and handed to the foreign server via `ServeFunc`. Scaffold does **not** close it during shutdown — the foreign server usually owns the listener lifecycle (for example, `grpc.Server.GracefulStop` closes its listener internally). The user's Cleaner function must stop the foreign server in a way that unblocks its Serve loop; otherwise the goroutine running `ServeFunc` leaks until process exit. This trade is deliberate: scaffold does not know how a foreign server prefers to be shut down, so imposing `listener.Close` on it could cause unclean teardown.

## Bindings Internals

### Shared store

`DefaultBindings` and `TestBindings` both embed a private `bindingStore`:

```go
type bindingStore struct {
    mu     sync.Mutex
    order  []string
    byName map[string]*Binding
}

func (s *bindingStore) add(name string, port int) *Binding
func (s *bindingStore) Get(name string) *Binding
```

`add` takes the mutex, lazily initializes the map, panics if the name is already present (duplicate registration is a programmer error), appends the new `Binding` to `order`, stores it in `byName`, and returns it. `Get` takes the mutex and returns the `*Binding` for the given name (or nil).

`DefaultBindings` and `TestBindings` wrap the shared store. The only difference is the port they forward through `add`:

```go
type DefaultBindings struct{ bindingStore }
func (d *DefaultBindings) Add(name string, port int) *Binding  // forwards port as-is

type TestBindings struct{ bindingStore }
func (t *TestBindings) Add(name string, port int) *Binding     // ignores port, forwards 0
```

Passing `0` to `net.Listen` yields a kernel-assigned ephemeral port.

### Listen addresses

- `DefaultBindings` listens on `:<port>` (all interfaces, Go's standard convention).
- `TestBindings` listens on `127.0.0.1:0` (loopback, kernel-assigned port).

Host binding is not configurable in v1. Services that need specific bind hosts can use `ServeFunc` or control it at the process/container level.

### Binding struct

```go
type Binding struct {
    name string
    port int

    mws         []MiddlewareFunc
    rpcHandlers []RPCHandler
    mux         http.Handler
    tls         *tls.Config
    readTO, writeTO, idleTO time.Duration
    serveFunc   func(net.Listener) error

    finalHandler http.Handler // composed after OnStart returns
    addr         net.Addr     // set after net.Listen
    srv          *http.Server // nil for ServeFunc bindings
}
```

### Handler composition (lazy)

`UseMiddleware`, `AddRPC`, `SetMux`, and setters only record state on the `Binding`. Scaffold composes the final `http.Handler` **once**, after `OnStart` returns nil and before opening the listener.

Composition proceeds in two stages. The inner dispatcher is an `http.Handler` that iterates `b.rpcHandlers` in registration order, calling each handler's `ServeHTTP(w, r) bool` and stopping on the first `true`. If every `RPCHandler` returns `false`, it delegates to `b.mux` when non-nil, else writes `http.StatusNotFound` with no body. The outer wrapper applies each middleware in `b.mws` in reverse index order so that the first-registered middleware becomes the outermost wrapper (runs first). The result is assigned to `b.finalHandler`.

### Mutual-exclusion and validation checks

Before composition, scaffold validates each binding and panics on programmer errors:

- `ServeFunc` + any of `UseMiddleware` / `AddRPC` / `SetMux` → panic.
- Duplicate `SetMux` → panic at set time.
- Duplicate `AddRPC` is allowed (multiple handlers form a fallthrough chain).
- `b.tls != nil && len(b.tls.Certificates) == 0` → panic. `http.Server.ServeTLS("", "")` relies on `TLSConfig.Certificates` being populated, and empty certs would otherwise fail at the first incoming request rather than at startup. `autotls.Setup` normally populates this; the check catches the case where a caller assigns a `*tls.Config` directly without loading certs.

Panic messages identify the binding by name.

### `Instance.Addr`

After `net.Listen` returns, scaffold stores `listener.Addr()` on the `Binding.addr` field. The `Instance` method reads this field via `bindings.Get(name)`:

```go
func (i *Instance) Addr(name string) net.Addr
```

Panics with `scaffold: no binding named %q` if `name` was never added. For `ServeFunc` bindings, `addr` is set from the listener before the user's serve function is invoked.

## HTTP Server Wiring

One `http.Server` is constructed per HTTP binding (excluding `ServeFunc` bindings) after `OnStart` returns and before `net.Listen`. The stored `*http.Server` lives on `Binding.srv`.

### Field mapping from `Binding` to `http.Server`

| `Binding` source | `http.Server` field |
|---|---|
| `finalHandler` (composed earlier) | `Handler` |
| `readTO` (from `SetTimeouts`) | `ReadTimeout` |
| `writeTO` (from `SetTimeouts`) | `WriteTimeout` |
| `idleTO` (from `SetTimeouts`) | `IdleTimeout` |
| `tls` (from `SetTLS`; may be nil) | `TLSConfig` |

`ReadHeaderTimeout` is deliberately left zero — Go inherits `ReadTimeout` in that case, giving header-timeout coverage without exposing a fourth parameter. `MaxHeaderBytes` is not exposed; Go's 1 MB default applies.

### Serving

- Plain HTTP: `srv.Serve(listener)` in a goroutine.
- TLS: `srv.ServeTLS(listener, "", "")` in a goroutine. Empty cert/key arguments rely on `TLSConfig.Certificates` being populated (enforced by `autotls.Setup` and by the validation check in the Bindings Internals section).

Graceful shutdown calls `srv.Shutdown(shutdownCtx)` per the sequence above.

## Configuration Providers

### `EnvConfigProvider`

- Reads environment variables via `os.Getenv` on every call. No caching.
- No prefix support. Callers that want a prefix name their keys explicitly (`MYAPP_API_PORT`).
- `Plain` variants return an error when `os.LookupEnv` reports the key absent, or when parsing fails.
- `Or` variants return the fallback silently on missing. On parse failure with a present value, they log a warning via the configured logger and use the fallback.

### `MapConfigProvider`

Declared as a named type on `map[string]string` so the design document's composite literal usage works directly:

```go
type MapConfigProvider map[string]string
```

Implements every method of `ConfigProvider`. Plain variants return `scaffold: config key %q not found` when the key is missing or `strconv` errors when a value exists but does not parse. `Or` variants return the fallback on missing keys; on parse failure they log a warning via the configured logger and return the fallback.

### `FileConfigProvider`

Designed for Kubernetes-style mounted ConfigMap / Secret volumes where each key is a file.

```go
type FileConfigProvider struct {
    Dir string
}
```

Behavior:

- Key → file name verbatim. `String("DB_PASSWORD")` reads `{Dir}/DB_PASSWORD`.
- Keys containing `/`, `..`, or any path component that changes under `filepath.Clean` are rejected with an error (prevents path traversal).
- Read on every call. Kubernetes rotates mounted Secrets / ConfigMaps via atomic symlink swap; `os.ReadFile` follows symlinks transparently, so rotations are picked up without reload logic.
- Trims exactly one trailing newline (`\n` or `\r\n`). Preserves leading whitespace and internal newlines (critical for PEM data). Rationale: `kubectl create secret --from-file=...` preserves source-file trailing newlines; stripping one matches `$(<file)` shell convention without mangling multi-line values.
- Missing file → not-found error, same shape as `EnvConfigProvider`.

### CLI argument parsing

**Not implemented in v1.** `Serve`'s `args []string` parameter is retained for forward compatibility but is ignored. The design document's statement that "CLI arguments take precedence over environment variable configuration" is deferred to a future version.

See Open Questions for the design-document soft flag.

## TLS

Scaffold imports `github.com/kapetan-io/tackle/autotls` directly. The tackle package provides a richer `autotls.Config` than the design document shows — see Open Questions for the design-doc soft flag.

### Prerequisite

Before scaffold is implemented, `tackle/autotls` must be updated to replace its custom `StandardLogger` interface with `*slog.Logger`. The existing interface is already slog-shaped (`Info/Debug/Warn/Error(msg, args...)`), so the migration is mechanical: change the `Logger` field type and replace the `NoOpLogger` default with `slog.New(slog.DiscardHandler)`.

### Usage

Daemons set TLS per binding with `binding.SetTLS(cfg.ServerTLS)` after calling `autotls.Setup(&cfg)`. For tests, `autotls.Config{AutoTLS: true}` produces an in-memory CA + leaf cert; the generated `ClientTLS` trusts the generated server.

The tackle implementation defaults to TLS 1.3 minimum and ECDSA P-521 for generated keys.

## Middleware

### Built-in inventory (v1)

Only one middleware ships in core:

- `scaffold.PanicRecovery(log *slog.Logger) MiddlewareFunc`

`RequestID` is deliberately not built-in. Service authors who need request IDs can trivially write their own middleware or pull one from a third-party package.

Access logging, CORS, rate limiting, compression, and body-size limits are out of scope for core. The design document explicitly punts body size limits: DUH-RPC generated handlers enforce their own, and `SetMux` users provide their own.

### `PanicRecovery` behavior

```go
func PanicRecovery(log *slog.Logger) MiddlewareFunc
```

- `defer recover()` in a wrapper `http.Handler`.
- On panic, logs at error level with fields: `msg="panic recovered"`, `method`, `path`, `panic` (`fmt.Sprint(v)`), `stack` (`string(debug.Stack())`).
- Writes response: `500 Internal Server Error` with body `"internal server error"` and `Content-Type: text/plain; charset=utf-8`.
- Does not re-panic.
- If the response has already been written, the status-code write is skipped via standard `http.ResponseWriter` semantics; the log entry is still emitted.
- `log` must not be nil. `PanicRecovery(nil)` panics at construction time. Service authors pass `sc.Log` (scaffold guarantees `sc.Log` is non-nil by the time middleware is wired). A `slog.Default()` fallback is explicitly rejected: in a process that never calls `slog.SetDefault`, `slog.Default()` writes to `os.Stderr` via the standard `log` package at `INFO` level — a silent and potentially invisible sink in containerized deployments that discard stderr.

## Built-in Logger

When `Options.Log == nil`, scaffold constructs a `*slog.Logger` backed by `tackle/color`'s handler. The handler is configured with `slog.HandlerOptions.ReplaceAttr` set to `color.SuppressAttrs(slog.TimeKey)` so that timestamps are omitted.

Always colorized, no timestamp, no TTY detection — users who pipe output to a file should provide their own `*slog.Logger` via `Options.Log`.

This is a deliberate narrowing of the design document, which specifies TTY-detected color suppression. See Open Questions for the soft flag.

## Metrics

`scaffold/prometheus` exposes:

```go
func HTTPMetrics(registerer prometheus.Registerer) MiddlewareFunc
```

### Collectors

- `http_requests_total` — `CounterVec` with labels `method`, `pattern`, `status`.
- `http_request_duration_seconds` — `HistogramVec` with labels `method`, `pattern`, `status`. Buckets = `prometheus.DefBuckets`.

Custom buckets are not configurable in v1. Authors with different latency profiles can build their own middleware around the same collectors.

`HTTPMetrics` is an HTTP middleware and therefore applies only to HTTP bindings. It cannot be registered on `ServeFunc` bindings — the mutual-exclusion rule (`ServeFunc` + `UseMiddleware` panics) enforces this at startup.

### Registerer handling

- `nil` → `prometheus.DefaultRegisterer` (standard Go Prometheus convention).
- Collectors are cached per-registerer in a package-level `sync.Map`. A second call to `HTTPMetrics(nil)` in the same process (common in tests that re-create daemons) returns middleware bound to the first-registered collectors, avoiding duplicate-registration panics.

### Label handling

- `pattern` is read from `r.Pattern` after `next.ServeHTTP`. Generated DUH-RPC handlers set `r.Pattern` to their endpoint constant before dispatch; `http.ServeMux` sets it automatically. Empty post-handler (unmatched request) → label value `"unknown"`.
- `status` is captured by wrapping `http.ResponseWriter` with a small status-recording wrapper. Default `200` if `WriteHeader` was never called.
- `method` comes directly from `r.Method`.

## Cleaner & Thread-Safety

### `Cleaner`

LIFO stack, concurrent-safe registration. Construction takes a `*slog.Logger` so that panic recovery and error reporting inside `Clean` have a non-nil target. Scaffold constructs the `Cleaner` when building `DaemonConfig`, passing `sc.Log`.

```go
type Cleaner struct {
    mu      sync.Mutex
    log     *slog.Logger
    stack   []func(context.Context) error
    running bool
}

// Package-internal constructor; scaffold calls this when building DaemonConfig.
func newCleaner(log *slog.Logger) *Cleaner

func (c *Cleaner) Add(fn func(context.Context) error)
func (c *Cleaner) Clean(ctx context.Context) error
```

Behavior:

- `Add` appends `fn` to the stack under the mutex. If `Clean` has already begun or completed (`running == true`), `Add` panics with `scaffold: Cleaner.Add called during or after Clean`. This forces authors to register all cleanup up-front and keeps LIFO semantics simple.
- `Clean` atomically flips `running` to `true`, snapshots the stack, releases the mutex, then iterates the snapshot in reverse order. A second call to `Clean` returns `scaffold: Cleaner.Clean called twice` without rerunning the stack.
- Each function runs inside its own `defer/recover`. A panic is logged at error level with the panic value and `debug.Stack()`, and the next function still runs. A non-nil error return is logged at error level, and the next function still runs. The outer `Clean` always returns `nil`.

### Thread-safety contracts

- **`Binding` setup methods** (`UseMiddleware`, `AddRPC`, `SetMux`, `SetTLS`, `SetTimeouts`, `ServeFunc`) — safe only from the `OnStart` goroutine. Not concurrent-safe. Documented as a single-goroutine contract, not enforced by locks.
- **`Bindings.Add` / `Bindings.Get`** — mutex-protected. Supports the company-wrapper pattern trivially even if the wrapper and inner service happen to run on different goroutines.
- **`Cleaner.Add`** — mutex-protected, concurrent-safe. Handlers and background goroutines may register cleanup at any time before `Clean` begins.
- **After `OnStart` returns**, scaffold treats the `Binding` graph as frozen. The runtime never mutates it; handlers never should either.

## Testing

Testing follows the `surface-testing` skill.

### Key surfaces

**Integration** (scaffold's test suite uses `scaffold.Start()` + real HTTP):

- Lifecycle happy path (single binding, plain HTTP, round-trip, clean stop).
- Multiple bindings — open in `Add` order, shut down in reverse.
- `OnStart` returns error → `OnStop` called, no ports opened.
- Second binding fails to open → `OnStop` called, first binding closed.
- `OnStartTimeout` cancellation honored.
- Graceful drain — in-flight handler completes under the shutdown ctx deadline.
- `PanicRecovery` — handler panics return `500 "internal server error"`, subsequent requests still served.
- RPC handler chain fallthrough — handler returning `false` defers to next; `SetMux` fallback; 404 with no mux.
- `ServeFunc` binding — listener handed off; scaffold does not serve HTTP.
- TLS via `tackle/autotls` `AutoTLS: true` — HTTPS round-trip with generated client config.

**Programmer-error panics** (each in its own test):

- Duplicate `Bindings.Add(name, ...)`.
- Duplicate `SetMux` on a binding.
- `ServeFunc` + `AddRPC` on the same binding.
- `ServeFunc` + `SetMux` on the same binding.
- `Cleaner.Add` during or after `Cleaner.Clean`.

**Unit**:

- Each `ConfigProvider` implementation (`Env`, `Map`, `File`) tested through its interface methods. `FileConfigProvider` tests use `t.TempDir()`.
- `Cleaner` — LIFO order, panic-in-cleaner recovery, error-in-cleaner continuation.
- Port-readiness helper — happy path and timeout path, exercised indirectly via integration tests (bring up a daemon and assert the returned `Instance.Addr` responds) plus one targeted test on the unreachable-address timeout via an address that won't accept.

### Fakes needed

- Minimal fake `Daemon` with configurable `OnStart`/`OnStop` hooks for lifecycle tests.
- Minimal `RPCHandler` implementation for chain/fallthrough tests.

### Observability for non-HTTP behavior

Internal ordering (cleaner execution order, binding shutdown order) is verified by having test daemons append events into a slice that the test asserts against. No reflection into private fields; no direct unit tests of internal scaffold functions beyond the config providers and `Cleaner`.

## Prerequisites

Must land before scaffold implementation begins:

1. **`tackle/autotls` slog migration** — replace `StandardLogger` with `*slog.Logger`; replace `NoOpLogger` default with `slog.New(slog.DiscardHandler)`. Mechanical change — the existing interface is already slog-shaped.

## Open Questions & Soft Flags

These items warrant a design-document revision before the spec drives an implementation plan.

- **[NEEDS DESIGN CLARIFICATION]** The design document retains `args []string` on `Serve`'s signature with "CLI arguments take precedence over environment variable configuration." This spec drops CLI parsing from v1. The `args` parameter is kept as reserved/ignored. Design doc should either retain the parameter with an explicit "currently unused, reserved for future subcommands" note, or drop the parameter entirely.
- **[NEEDS DESIGN CLARIFICATION]** The design document's TLS section shows `import ".../scaffold/autotls"` and a minimal `autotls.Config`. Actual usage is `github.com/kapetan-io/tackle/autotls` with a significantly larger surface (PEM buffer inputs, outbound mTLS cert fields, `InsecureSkipVerify`, `MinVersion` with TLS 1.3 default, `ServerOrgName`, logger). Design doc should be updated to reference `tackle/autotls` and show its real fields.
- **[NEEDS DESIGN CLARIFICATION]** The design document references `scaffold.RequestID()` as a built-in middleware and shows it used in several examples. This spec removes `RequestID` from the v1 built-in middleware set. Design doc examples should be updated to remove `scaffold.RequestID()` or replace it with a user-supplied equivalent.
- **[NEEDS DESIGN CLARIFICATION]** The design document specifies TTY-detected color suppression for the built-in logger ("ANSI color is enabled in a TTY and suppressed otherwise"). This spec narrows that to "always colorized, no TTY detection"; users piping to files are expected to supply their own `*slog.Logger`. Design doc should be updated to reflect the simpler behavior, or the spec should re-introduce TTY detection.

Total soft flags: **4**. Recommended: a design-document revision pass before an implementation plan is built from this spec.
