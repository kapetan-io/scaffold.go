# Scaffold Tech Spec

_Design document: [`docs/scaffold-design.md`](../../scaffold-design.md)_

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
- Health / Readiness handlers
- Thread-safety contracts
- Test surfaces

## Packaging & Dependencies

### Module

- Module path: `github.com/kapetan-io/scaffold`
- Go version: `go 1.26`. (`r.Pattern` requires ≥ 1.23; 1.26 is pinned as the current stable floor.)

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

`Serve` registers a buffered signal channel for `SIGTERM` and `SIGINT` via `signal.Notify`, then blocks on a `select` between that channel and the caller's `ctx.Done()`. Whichever fires first triggers shutdown, and the log line records which — `reason=signal` plus the signal name, or `reason=context`. The `reason=signal|context` field values are an operator-facing log contract and are pinned.

`signal.NotifyContext` is not used because it collapses both paths into one `ctx.Done()`, losing which event triggered shutdown.

### Context construction

`Serve` derives two contexts:

- **Start context** — `context.WithTimeout(callerCtx, OnStartTimeout)` if `OnStartTimeout` is non-zero, else `callerCtx` unmodified. Passed to `OnStart`. Caller cancellation propagates into `OnStart`, so a caller who cancels the ctx passed to `Serve` can interrupt `OnStart` mid-flight.
- **Shutdown context** — `context.WithTimeout(Background, OnStopTimeout)` if non-zero, else `Background`. Used for `http.Server.Shutdown`, `OnStop`, and `Cleaner.Clean` in sequence. Derived from `Background` so that a caller-cancelled ctx does not shorten the shutdown budget.

`Start` (test entry point) uses the caller-supplied context for `OnStart`, and the context passed to `Instance.Stop(ctx)` for shutdown steps. `OnStartTimeout` / `OnStopTimeout` are ignored in test mode.

`Start` accepts a nil `*Options`, which is treated as `&Options{}`; each zero-value field then resolves to its documented default (`TestBindings`, `EnvConfigProvider`, built-in color logger, no timeouts).

Both the Start and Shutdown contexts are enriched with framework services (see below) before being handed to `OnStart` / `OnStop`. Enrichment always uses `context.WithValue` on top of whatever parent context is in play. The caller's cancellation and any deadline they attached to the Start context remain in effect.

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
func waitForListener(ctx context.Context, addr net.Addr, timeout time.Duration) error
```

Behavior:

- Computes a dial address: if `addr.(*net.TCPAddr).IP.IsUnspecified()` (e.g. `0.0.0.0:<port>` or `[::]:<port>` returned by a listener on `:<port>`), the probe dials `127.0.0.1:<port>` for an IPv4 unspecified address and `[::1]:<port>` for an IPv6 unspecified address. Otherwise the probe dials `addr.String()` verbatim. Dialing the unspecified address is non-portable (works on Linux, fails on macOS/BSD), so the translation is required.
- Calls `net.DialTimeout("tcp", dialAddr, 100*time.Millisecond)` in a loop.
- Sleeps 10 ms between attempts, aborting the sleep on `ctx.Done()`.
- Returns nil on first successful dial.
- Returns `ctx.Err()` if `ctx` is cancelled during the probe (caller cancellation or shutdown signal propagates through).
- On `timeout` expiry: error whose message names the listener address.

Default timeout is 5 seconds. Not configurable. The `ctx` passed in is the Start context (so caller cancellation is observed); `timeout` bounds only the probe itself, independent of `OnStartTimeout`.

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
4. Surface the OnStart error (see "Error surfacing" below). No ports were opened.
```

**Binding-open failure** (net.Listen failed on the Nth binding):

```
1. Log the bind error with the binding name.
2. For each already-opened binding (1..N-1) in reverse order:
     - If HTTP:      http.Server.Shutdown(shutdownCtx).
     - If ServeFunc: no action (user Cleaner handles it).
3. OnStop(shutdownCtx).
4. Cleaner.Clean(shutdownCtx).
5. Surface the bind error (see "Error surfacing" below).
```

All paths share the single `OnStopTimeout`-derived context (Serve) or the context passed to `Instance.Stop` (Start).

If any step in any path returns an error, scaffold logs it and continues to the next step. A failed `OnStop` never prevents `Cleaner.Clean` from running. A failed `http.Server.Shutdown` never prevents later bindings from shutting down or subsequent steps from running.

Scaffold wraps each `OnStop` call and each `http.Server.Shutdown` call in its own `defer/recover`. A panic is logged at error level with the panic value and `debug.Stack()`, and the shutdown sequence continues to the next step. This preserves the core guarantee that `Cleaner.Clean` always runs — an unrecovered panic in `OnStop` would otherwise skip it. `Cleaner` already recovers per-function panics internally (see "Cleaner & Thread-Safety").

### Error surfacing

`Start` and `Serve` have different return signatures and therefore different error-surfacing rules:

- **`Start` returns `(*Instance, error)`.** Startup errors (`OnStart` or bind failure) are returned verbatim as the second return. Shutdown-path errors encountered while unwinding are logged but not returned (they are diagnostic only; the caller already knows startup failed). `Instance.Stop(ctx)` returns its own error for shutdown-path failures when called by a test.
- **`Serve` returns `int`.** All failures (`OnStart`, bind, `OnStop`, `Cleaner.Clean`) cause `Serve` to return `ExitFailure` (1). The error value is logged via `sc.Log` at error level before return; it is not otherwise surfaced (`Serve` owns all logging internally, per the design document). Clean exits — signal-triggered or ctx-cancel triggered — return `ExitSuccess` (0) regardless of whether individual shutdown steps logged errors. There is no third exit code for v1.

**ServeFunc listener ownership.** The listener is created by scaffold and handed to the foreign server via `ServeFunc`. Scaffold does **not** close it during shutdown — the foreign server usually owns the listener lifecycle (for example, `grpc.Server.GracefulStop` closes its listener internally). The user's Cleaner function must stop the foreign server in a way that unblocks its Serve loop; otherwise the goroutine running `ServeFunc` leaks until process exit. This trade is deliberate: scaffold does not know how a foreign server prefers to be shut down, so imposing `listener.Close` on it could cause unclean teardown.

### Serve-loop goroutine return handling

Scaffold spawns one goroutine per binding to run its serve loop. Classifying a goroutine's return as "expected" or "unexpected" uses a single mechanism across binding types: the daemon maintains an atomic **shutdown-requested** flag, set when any of these fires:

- `SIGTERM` / `SIGINT` received by `Serve`.
- The context passed to `Serve` is cancelled.
- `Instance.Stop(ctx)` is invoked (test entry point).
- `OnStart` returned a non-nil error (triggers the `OnStart error` teardown path).
- `net.Listen` failed for a binding (triggers the `Binding-open failure` teardown path).

The flag is set **before** scaffold begins any shutdown action that would cause a goroutine to return — specifically before calling `srv.Shutdown` on any HTTP binding and before the user's Cleaner functions run (which typically stop `ServeFunc`-registered foreign servers).

On goroutine return, the classification rule is uniform:

- If shutdown-requested is set, any return value is **expected**. Nil returns are swallowed silently. Non-nil returns are logged at debug level for diagnostics (`binding=<name>`, `error=<err>`) but do not indicate a problem. For HTTP bindings this means `http.ErrServerClosed` is expected; for `ServeFunc` bindings this means protocol-specific sentinels like `grpc.ErrServerStopped` are expected without scaffold needing to know about them.
- If shutdown-requested is **not** set, any non-nil return is **unexpected** — the binding stopped serving on its own. Log at error level with fields `binding=<name>` and `error=<err>`. A nil return when shutdown was not requested is also logged at error level (the binding "stopped cleanly" without being asked — still a surprise).

In both cases the daemon is **not** shut down as a side effect of a single binding's goroutine exiting. Scaffold prefers to keep remaining bindings serving and let an external supervisor (k8s liveness probe, systemd, or a `ReadyHandler` wired by the service author) decide whether a partially-dark process should be restarted. Service authors who want crash-triggered full-daemon shutdown can cancel the daemon's outer context from inside their own serve loop — that is an explicit opt-in, not a scaffold default.

See ADR-0003 for the full rationale.

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

`add` takes the mutex, lazily initializes the map, panics if the name is already present (duplicate registration is a programmer error — the panic message identifies the duplicated name), appends the new `Binding` to `order`, stores it in `byName`, and returns it. `Get` takes the mutex and returns the `*Binding` for the given name (or nil).

`DefaultBindings` and `TestBindings` wrap the shared store. The only difference is the port they forward through `add`:

```go
type DefaultBindings struct{ bindingStore }
func (d *DefaultBindings) Add(name string, port int) *Binding  // forwards port as-is

type TestBindings struct{ bindingStore }
func (t *TestBindings) Add(name string, port int) *Binding     // ignores port, forwards 0
```

Passing `0` to `net.Listen` yields a kernel-assigned ephemeral port.

The public `Bindings` interface (`Add(name, port) *Binding`, `Get(name) *Binding`) is an extension point: `Options.Bindings` accepts any implementation. The two built-ins cover production and standard test use; third-party implementations are supported for cases such as a fully in-memory pipe-based binding that lets surface tests run under `testing/synctest`. Implementations must guarantee duplicate-name rejection and `Get`-after-`Add` consistency, which the built-in `bindingStore` provides.

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

Mutually-exclusive setter conflicts are detected **at call time**, inside the offending setter, and panic immediately. The panic stack therefore names the line in `OnStart` that caused the conflict rather than surfacing out-of-band during composition. Rules:

- `ServeFunc` panics if `len(b.mws) > 0`, `len(b.rpcHandlers) > 0`, `b.mux != nil`, `b.tls != nil`, or any timeout is non-zero. (`ServeFunc` owns the listener; the HTTP pipeline would be dead code.)
- `UseMiddleware`, `AddRPC`, `SetMux`, `SetTLS`, `SetTimeouts` each panic if `b.serveFunc != nil`.
- `SetMux` panics if `b.mux != nil` (duplicate fallback is a programmer error).
- Duplicate `AddRPC` is allowed — multiple handlers form a fallthrough chain.

Panic messages identify the binding by name and the conflict; exact wording is not pinned.

Scaffold does **not** pre-validate TLS configs at startup. A `*tls.Config` with no `Certificates`, `GetCertificate`, or `GetConfigForClient` is accepted — misconfiguration surfaces at TLS handshake time via Go's standard error path. This keeps dynamic cert loading (ACME, SNI) working without a special case.

### `Instance.Addr`

After `net.Listen` returns, scaffold stores `listener.Addr()` on the `Binding.addr` field. The `Instance` method reads this field via `bindings.Get(name)`:

```go
func (i *Instance) Addr(name string) net.Addr
```

Panics when `name` was never added; the panic message names the missing binding. For `ServeFunc` bindings, `addr` is set from the listener before the user's serve function is invoked.

### `Instance.Stop`

`Instance.Stop(ctx)` is idempotent. A `sync.Once`-backed guard runs the shutdown sequence exactly once; subsequent calls return nil immediately without re-running any step. This matches the Go convention for `Close`-shaped methods and lets tests safely combine `defer inst.Stop(ctx)` with an explicit earlier `Stop` in specific code paths.

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
- TLS: `srv.ServeTLS(listener, "", "")` in a goroutine. Empty cert/key arguments rely on `TLSConfig.Certificates`, `GetCertificate`, or `GetConfigForClient` being set. `autotls.Setup` populates `Certificates`. Callers assigning `*tls.Config` directly are responsible for populating at least one of those fields; otherwise the handshake fails at first request.

Goroutine return values are handled per the **Serve-loop goroutine return handling** section above.

Graceful shutdown calls `srv.Shutdown(shutdownCtx)` per the sequence above.

## Configuration Providers

All three providers carry an optional `Logger *slog.Logger` field. When non-nil, `Or` variants log a warning on parse failure before falling back. When nil, parse-failure logging is a no-op. Scaffold constructs its default providers with `sc.Log` attached; user-constructed providers may leave `Logger` nil.

### `EnvConfigProvider`

```go
type EnvConfigProvider struct {
    Logger *slog.Logger
}
```

- Reads environment variables via `os.Getenv` on every call. No caching.
- No prefix support. Callers that want a prefix name their keys explicitly (`MYAPP_API_PORT`).
- `Plain` variants return an error when `os.LookupEnv` reports the key absent, or when parsing fails.
- `Or` variants return the fallback silently on missing. On parse failure with a present value, they log a warning (if `Logger` is set) and use the fallback.

### `MapConfigProvider`

```go
type MapConfigProvider struct {
    Values map[string]string
    Logger *slog.Logger
}
```

Implements every method of `ConfigProvider`. Plain variants return a not-found error whose message names the missing key when the key is absent, or `strconv` errors when a value exists but does not parse. `Or` variants return the fallback on missing keys; on parse failure they log a warning (if `Logger` is set) and return the fallback.

`Values` may be nil — a nil map is treated as empty, so every key lookup returns not-found / fallback. A zero-value `MapConfigProvider{}` is therefore a valid provider that always returns fallbacks. This matches Go's "zero-value does the right thing" principle from the design document.

Note: this struct shape breaks the design document's composite-literal usage `MapConfigProvider{"API_PORT": "8443"}`. Callers write `MapConfigProvider{Values: map[string]string{"API_PORT": "8443"}}` instead.

### `FileConfigProvider`

Designed for Kubernetes-style mounted ConfigMap / Secret volumes where each key is a file.

```go
type FileConfigProvider struct {
    Dir    string
    Logger *slog.Logger
}
```

Behavior:

- Key → file name verbatim. `String("DB_PASSWORD")` reads `{Dir}/DB_PASSWORD`.
- Keys containing `/`, `..`, or any path component that changes under `filepath.Clean` are rejected with an error (prevents path traversal).
- Read on every call. Kubernetes rotates mounted Secrets / ConfigMaps via atomic symlink swap; `os.ReadFile` follows symlinks transparently, so rotations are picked up without reload logic.
- Trims exactly one trailing newline (`\n` or `\r\n`). Preserves leading whitespace and internal newlines (critical for PEM data). Rationale: `kubectl create secret --from-file=...` preserves source-file trailing newlines; stripping one matches `$(<file)` shell convention without mangling multi-line values.
- Missing file → not-found error, same shape as `EnvConfigProvider`.

### CLI argument parsing

**Not implemented in v1.** `Serve`'s `args []string` parameter is retained for forward compatibility but ignored. Reserved for future sub-command expansion (e.g. `my-service serve`).

## TLS

Scaffold imports `github.com/kapetan-io/tackle/autotls` directly. The tackle package provides a richer `autotls.Config` than the design document shows: PEM buffer inputs, outbound mTLS cert fields, `InsecureSkipVerify`, `MinVersion` with TLS 1.3 default, `ServerOrgName`, and a `*slog.Logger` field.

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
- On panic, logs at error level with the pinned message `"panic recovered"` and fields `method`, `path`, `panic` (`fmt.Sprint(v)`), `stack` (`string(debug.Stack())`). Both the message and the field names are operator-facing log contracts and are pinned.
- Writes response: `500 Internal Server Error` with body `"internal server error"` and `Content-Type: text/plain; charset=utf-8`. The body is user-facing and pinned.
- Does not re-panic.
- If the response has already been written, the status-code write is skipped via standard `http.ResponseWriter` semantics; the log entry is still emitted.
- `log` must not be nil. `PanicRecovery(nil)` panics at construction time. Service authors pass `sc.Log` (scaffold guarantees `sc.Log` is non-nil by the time middleware is wired). A `slog.Default()` fallback is explicitly rejected: in a process that never calls `slog.SetDefault`, `slog.Default()` writes to `os.Stderr` via the standard `log` package at `INFO` level — a silent and potentially invisible sink in containerized deployments that discard stderr.

## Health and Readiness Handlers

Scaffold exposes two helpers that service authors mount into whatever `http.Handler` they pass to `SetMux` (typically `http.NewServeMux()`):

```go
func HealthHandler(log *slog.Logger, fn func(ctx context.Context) any) http.Handler
func ReadyHandler(fn func(ctx context.Context) (bool, string)) http.Handler
```

### `HealthHandler`

- Accepts any HTTP method. Kubernetes liveness probes default to GET; stricter gating belongs in the caller's mux if needed.
- Invokes `fn(r.Context())`, then marshals the result to JSON via `encoding/json`.
- On success, writes `200 OK` with `Content-Type: application/json` and the marshaled body.
- On marshal failure, logs at error level (using the provided `log`) with fields `msg="health marshal failed"`, `path`, `error`, and writes `500 Internal Server Error` with body `{"error":"health marshal failed"}` and `Content-Type: application/json`.
- `log` must not be nil — `HealthHandler(nil, ...)` panics at construction time. Rationale matches `PanicRecovery(nil)`: a silent marshal-failure log is worse than a loud construction-time panic.
- Panics raised inside `fn` are **not** caught by `HealthHandler`. The service author is expected to mount `PanicRecovery` in the middleware chain, or accept an unrecovered panic as the correct diagnostic.

### `ReadyHandler`

- Accepts any HTTP method.
- Invokes `fn(r.Context())`.
- On `(true, _)`, writes `200 OK` with an empty body and no explicit `Content-Type` (Go defaults apply if the body were non-empty).
- On `(false, reason)`, writes `503 Service Unavailable` with `Content-Type: text/plain; charset=utf-8` and the reason string verbatim as the body.
- No logger argument — `ReadyHandler` has no failure mode to log (marshaling is not involved).
- Panics raised inside `fn` are **not** caught by `ReadyHandler`. Same rationale as `HealthHandler`.

### Test surfaces

- `HealthHandler` — happy path (200, JSON body round-trips), marshal failure path (500 with pinned body, log emitted), nil-log panic at construction.
- `ReadyHandler` — ready returns 200/empty, not-ready returns 503 with reason body.
- Both verified as plain `http.Handler` via `httptest.NewRecorder`, no daemon wiring required.

## Built-in Logger

When `Options.Log == nil`, scaffold constructs a `*slog.Logger` backed by `tackle/color`'s handler. The handler is configured with:

- `slog.HandlerOptions.Level = slog.LevelInfo` (explicit, matches slog's own default).
- `slog.HandlerOptions.ReplaceAttr = color.SuppressAttrs(slog.TimeKey)` so timestamps are omitted.

Always colorized, no timestamp, no TTY detection — users who pipe output to a file should provide their own `*slog.Logger` via `Options.Log`. This is a deliberate narrowing of the design document, which specifies TTY-detected color suppression.

## Metrics

`scaffold/prometheus` exposes:

```go
func HTTPMetrics(registerer prometheus.Registerer) MiddlewareFunc
```

### Collectors

- `http_requests_total` — `CounterVec` with labels `method`, `pattern`, `status`.
- `http_request_duration_seconds` — `HistogramVec` with labels `method`, `pattern`, `status`. Buckets = `prometheus.DefBuckets`.

Custom buckets are not configurable in v1. Authors with different latency profiles can build their own middleware around the same collectors.

`HTTPMetrics` is an HTTP middleware and therefore applies only to HTTP bindings. It cannot be registered on `ServeFunc` bindings — the call-time mutual-exclusion rule enforces this at `UseMiddleware` time.

### Registerer handling

- `nil` → `prometheus.DefaultRegisterer` (standard Go Prometheus convention).
- Collectors are cached per-registerer in a package-level `sync.Map`. A second call to `HTTPMetrics(nil)` in the same process (common in tests that re-create daemons) returns middleware bound to the first-registered collectors, avoiding duplicate-registration panics.

### ResponseWriter wrapper

The middleware wraps the incoming `http.ResponseWriter` in a small recorder. To remain compatible with streaming handlers (DUH-RPC server-streaming responses, SSE endpoints mounted via `SetMux`) and WebSocket upgrades, the wrapper explicitly forwards the standard extension interfaces:

- **`Flush()`** — delegates via type assertion to the inner writer when it implements `http.Flusher`. Streaming handlers (DUH streaming, SSE) rely on `Flush` to push bytes without buffering.
- **`Hijack()`** — delegates via type assertion to the inner writer when it implements `http.Hijacker`. Returns an error if the inner writer does not. WebSocket upgrades use this to take over the underlying TCP connection.
- **`Unwrap() http.ResponseWriter`** — returns the inner writer so `http.NewResponseController(w)` (Go 1.20+) can reach it for `SetReadDeadline`, `SetWriteDeadline`, etc.

`http.Pusher` is deliberately not forwarded. HTTP/2 server push is deprecated in Go 1.26 and unused by modern browsers.

After a successful `Hijack()` call, the wrapper stops treating the response as HTTP: no further status/duration updates are recorded, and the metric for that request is emitted using the status captured before the hijack (typically `101 Switching Protocols` from the WebSocket upgrade handshake, which the handler writes via `WriteHeader(101)` before calling `Hijack`).

### Label handling

- `pattern` is read from `r.Pattern` after `next.ServeHTTP`. Generated DUH-RPC handlers set `r.Pattern` to their endpoint constant before dispatch; `http.ServeMux` sets it automatically. Empty post-handler (unmatched request) → label value `"unknown"`.
- `status` is captured by the wrapper on `WriteHeader`. Default `200` if `WriteHeader` was never called.
- `method` comes directly from `r.Method`.

## Cleaner & Thread-Safety

### `Cleaner`

LIFO stack, concurrent-safe registration. Construction takes a `*slog.Logger` so that panic recovery and error reporting inside `Clean` have a non-nil target. Scaffold constructs a `Cleaner` internally when building `DaemonConfig`, passing `sc.Log`. `NewCleaner` is exported so callers may construct additional instances outside the daemon lifecycle — a background subsystem that wants LIFO teardown, a CLI `Run()` function, or a test that exercises `Cleaner` in isolation.

```go
type Cleaner struct {
    mu      sync.Mutex
    log     *slog.Logger
    stack   []func(context.Context) error
    running bool
}

func NewCleaner(log *slog.Logger) *Cleaner

func (c *Cleaner) Add(fn func(context.Context) error)
func (c *Cleaner) Clean(ctx context.Context) error
```

Behavior:

- `Add` appends `fn` to the stack under the mutex. If `Clean` has already begun or completed (`running == true`), `Add` panics; the panic message identifies the programmer error (exact wording not pinned). This forces authors to register all cleanup up-front and keeps LIFO semantics simple.
- `Clean` atomically flips `running` to `true`, snapshots the stack, releases the mutex, then iterates the snapshot in reverse order. A second call to `Clean` returns an error whose message identifies the double-call (exact wording not pinned) without rerunning the stack.
- Each function runs inside its own `defer/recover`. A panic is logged at error level with the panic value and `debug.Stack()`, and the next function still runs. A non-nil error return is logged at error level, and the next function still runs. The outer `Clean` always returns `nil`.

### Thread-safety contracts

- **`Binding` setup methods** (`UseMiddleware`, `AddRPC`, `SetMux`, `SetTLS`, `SetTimeouts`, `ServeFunc`) — safe only from the `OnStart` goroutine. Not concurrent-safe. Documented as a single-goroutine contract, not enforced by locks.
- **`Bindings.Add` / `Bindings.Get`** — mutex-protected. Supports the company-wrapper pattern trivially even if the wrapper and inner service happen to run on different goroutines.
- **`Cleaner.Add`** — mutex-protected, concurrent-safe. Handlers and background goroutines may register cleanup at any time before `Clean` begins.
- **After `OnStart` returns**, scaffold treats the `Binding` graph as frozen. The runtime never mutates it; handlers never should either.

## Testing

Testing follows the `surface-testing` skill. All tests live in `package scaffold_test` (and `package sprometheus_test` for the prometheus subpackage) — the underscore-test convention is mandatory. A test that cannot compile outside the internal package is a signal that it is reaching into internals and must be restructured to enter through the public surface.

### Key surfaces

**Integration** (scaffold's test suite uses `scaffold.Start()` + real HTTP unless otherwise noted):

- Lifecycle happy path (single binding, plain HTTP, round-trip, clean stop).
- Multiple bindings — open in `Add` order, shut down in reverse.
- `OnStart` returns error → `OnStop` called, no ports opened.
- Second binding fails to open → `OnStop` called, first binding closed. Fault injected at the boundary: the test pre-binds the target port with its own `net.Listen` before starting the daemon, so scaffold's `net.Listen(":port")` returns `EADDRINUSE` through the real error path — no internal hooks. Uses `DefaultBindings` with a port reserved the same way (listen on `:0`, read the assigned port, keep the listener open).
- `OnStartTimeout` cancellation honored.
- Graceful drain — in-flight handler completes under the shutdown ctx deadline.
- `PanicRecovery` — handler panics return `500 "internal server error"`, subsequent requests still served.
- RPC handler chain fallthrough — handler returning `false` defers to next; `SetMux` fallback; 404 with no mux.
- `ServeFunc` binding — listener handed off; scaffold does not serve HTTP.
- TLS via `tackle/autotls` `AutoTLS: true` — HTTPS round-trip with generated client config.

**`Serve()` integration** (in-process, no subprocess — `Serve` run in a goroutine from the test):

- Caller ctx cancelled → `Serve` returns `ExitSuccess`; shutdown log records `reason=context`.
- `SIGTERM` delivered via `syscall.Kill(os.Getpid(), syscall.SIGTERM)` → `Serve` returns `ExitSuccess`; shutdown log records `reason=signal` with the SIGTERM name.
- `SIGINT` delivered likewise → `Serve` returns `ExitSuccess`; shutdown log records `reason=signal` with the SIGINT name.
- `OnStart` returns error → `Serve` returns `ExitFailure`; error is logged.

**Programmer-error panics** (each in its own test):

- Duplicate `Bindings.Add(name, ...)`.
- Duplicate `SetMux` on a binding.
- `ServeFunc` + `AddRPC` on the same binding (call-time panic on the setter that caused the conflict, either direction).
- `ServeFunc` + `SetMux` on the same binding.
- `ServeFunc` + `UseMiddleware` on the same binding.
- `Cleaner.Add` during or after `Cleaner.Clean`.

**Unit**:

- Each `ConfigProvider` implementation (`Env`, `Map`, `File`) tested through its interface methods. `FileConfigProvider` tests use `t.TempDir()`. `MapConfigProvider` with nil `Values` verified: every lookup returns fallback / not-found.
- `Cleaner` — LIFO order, panic-in-cleaner recovery, error-in-cleaner continuation, `Add`-after-`Clean` panic, double-`Clean` error. Constructed in tests via the exported `NewCleaner(log)`.
- `HealthHandler` and `ReadyHandler` — via `httptest.NewRecorder` as plain `http.Handler`. Covers happy paths, 503 with reason body, marshal-failure path, and nil-log construction panic.
- Port-readiness helper — happy path and the unspecified-address translation (`0.0.0.0` → `127.0.0.1`, `[::]` → `[::1]`) are covered implicitly by every integration test that uses `DefaultBindings` (which listens on `:<port>`) and then hits the returned `Instance.Addr`. The timeout branch is defensive code for sandbox/firewall misconfigurations that do not reproduce portably from a test; its error-formatting is verified by code review rather than a dedicated test. If a future bug surfaces in that branch, add a test-only dial-injection seam at that time — do not add one speculatively.

### Fakes needed

- Minimal fake `Daemon` with configurable `OnStart`/`OnStop` hooks for lifecycle tests.
- Minimal `RPCHandler` implementation for chain/fallthrough tests.

### Observability for non-HTTP behavior

Internal ordering (cleaner execution order, binding shutdown order) is verified by having test daemons append events into a slice that the test asserts against. No reflection into private fields; no direct unit tests of internal scaffold functions beyond the config providers and `Cleaner`.

## Prerequisites

Must land before scaffold implementation begins:

1. **`tackle/autotls` slog migration** — replace `StandardLogger` with `*slog.Logger`; replace `NoOpLogger` default with `slog.New(slog.DiscardHandler)`. Mechanical change — the existing interface is already slog-shaped.

## Config Provider Errors

V1 ships plain string-formatted errors from every `ConfigProvider` implementation. No exported `ErrKeyNotFound` or `ErrParseFailed` sentinels. Adding sentinels later is backward compatible (a new exported var does not break string-matching callers); do so reactively when a concrete caller surfaces a need for `errors.Is`.
