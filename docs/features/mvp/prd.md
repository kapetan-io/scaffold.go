# Scaffold Framework — Design Checkpoint V14

## Overview

Scaffold is a Go framework for building HTTP API services. It provides service lifecycle management, configuration, health/readiness, structured logging, metrics, TLS/mTLS, and a clean testing story with manual dependency injection. It is designed for DUH-RPC services (POST-only, `/v1/{subject}.{method}`) and follows the principle that scaffold owns the port and middleware chain — routing and dispatch belong to the handlers.

The name "scaffold" reflects its role: it provides the structural shell around your service so you focus on business logic.

## Mental Model

Scaffold is organized around five concepts:

**Daemon** — the unit you write and run. A daemon implements `OnStart`/`OnStop`, creates one or more bindings, and wires up middleware and handlers. In production it runs as a long-lived process; in tests, `Start()` returns an `Instance` you hold onto to control it.

**Binding** — a named port and middleware chain. A binding listens on a port and passes incoming requests through its middleware stack to registered RPC handlers in order, falling back to a mux handler if none match. Scaffold does not perform any routing — dispatch belongs entirely to the handlers.

**Server** — a generated (or hand-written) struct that owns a `Service` and bridges it to the HTTP transport. A server implements `RPCHandler`, returning `false` when the request does not match any of its endpoints, allowing the next registered handler or the fallback mux to try.

**Service** — a transport-agnostic API implementation. A service knows nothing about HTTP, gRPC, or serialization — it receives typed request structs and returns typed responses.

**Transport** — informal concept: the protocol a binding uses (HTTP, gRPC, etc.). HTTP is the default. For other protocols, a binding's `ServeFunc` hands off the listener to the protocol's own serve loop.

## Core Design Principles

- **Manual dependency injection by construction** — no DI containers, no reflection, no magic. Dependencies are fields on a struct. If set, use them; if nil, create the real one.
- **DUH-RPC spec-first preferred** — write the spec, lint it, generate the server interface and types. Code conforms to the contract, not the reverse.
- **Scaffold owns the port and middleware, not the routing** — scaffold opens the listener and runs the middleware chain. All routing and dispatch live in the handlers. Generated servers use a switch statement for O(1) dispatch with no mux overhead.
- **The service owns its defaults** — production wiring (middleware, dependencies) is defined by the service author, not the framework. Tests override only what they need.
- **Zero-value does the right thing** — nil means "use the default." This applies to dependencies, middleware, and configuration.
- **Generated code has no scaffold dependency** — the `RPCHandler` interface is satisfied implicitly via Go's structural typing. Generated packages import nothing from scaffold.

## Daemon Interface

The `Daemon` interface is what every service implements:

```go
type Daemon interface {
    OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error
    OnStop(ctx context.Context) error
}
```

- `OnStart` — sets up dependencies, creates bindings, registers handlers, wires middleware. Receives a plain `context.Context` and a `*scaffold.DaemonConfig` which provides all framework services.
- `OnStop` — performs any teardown that cannot be expressed as a `Cleaner` function — for example, signalling background goroutines or flushing stateful buffers. Most daemons return nil here and rely entirely on the `Cleaner` stack for teardown. Scaffold calls `OnStop` and `Cleaner.Clean` as separate sequential steps; `OnStop` must not call `cleaner.Clean` itself.

Health and readiness endpoints are not part of the `Daemon` interface. Scaffold provides `HealthHandler` and `ReadyHandler` as plain `http.Handler` helpers — mount them in a `SetMux` handler alongside any other non-DUH routes.

## scaffold.DaemonConfig

`scaffold.DaemonConfig` is a plain exported struct. All framework services are fields — discoverable via autocomplete, passable to sub-packages. It is passed as a pointer so additions made during `OnStart` (such as bindings created by a company wrapper) are visible to inner services receiving the same pointer.

```go
type DaemonConfig struct {
    Config   ConfigProvider
    Secrets  ConfigProvider
    Log      *slog.Logger
    Cleaner  *Cleaner
    Bindings Bindings
}
```

Sub-packages that need specific framework services can receive them as explicit parameters, or extract them from the `context.Context` passed to `OnStart` via convenience shims:

```go
func GetConfig(ctx context.Context) ConfigProvider
func GetSecrets(ctx context.Context) ConfigProvider
func GetLogger(ctx context.Context) *slog.Logger
```

These shims are populated by scaffold on the context passed to `OnStart` — they are for sub-packages initialized during startup that receive a `context.Context` but not `sc` directly. They are **not** available on per-request contexts.

**Scaffold does not set any values on per-request contexts.** The `context.Context` on an incoming `*http.Request` is the standard `net/http` base context — scaffold adds nothing to it. If a service needs request-scoped values (such as a logger with trace IDs, authenticated user claims, or correlation metadata), the service author must add them via their own middleware using `r.WithContext()`. This is a deliberate design choice: scaffold owns the port and middleware chain, not the request context.

## Lifecycle: Serve vs Start

```go
// For main — blocks until SIGTERM/SIGINT or ctx cancel, then drains and exits.
func Serve(ctx context.Context, args []string, daemon Daemon, opts Options) int

// For tests — starts serving, returns an Instance to control the running daemon.
func Start(ctx context.Context, daemon Daemon, opts *Options) (*Instance, error)

const (
    ExitSuccess = 0
    ExitFailure = 1
)
```

- `Serve()` is called from `main()`. It blocks until it receives SIGTERM/SIGINT **or** the provided `ctx` is cancelled, then performs graceful shutdown and returns an exit code. Context cancellation is equivalent to receiving a signal — both trigger the same orderly shutdown path. `Serve()` owns all logging internally via the configured logger — the caller maps the exit code to `os.Exit` only. Scaffold derives two contexts from the caller's ctx passed to `Serve()`:
    - The **Start context** — used for `OnStart` — is derived from the caller's ctx with `OnStartTimeout` applied (if non-zero). Caller cancellation propagates into `OnStart`, so a caller who cancels the ctx can interrupt a stalled `OnStart`.
    - The **Shutdown context** — used for `http.Server.Shutdown`, `OnStop`, and `Cleaner.Clean` — is derived from `context.Background()` with `OnStopTimeout` applied (if non-zero). Caller cancellation does **not** shorten the shutdown budget.
- `Start()` is called from tests. It uses `TestBindings` by default, binding each listener to a random port on `127.0.0.1`. It returns immediately with a `*Instance` once the service is fully initialized and serving. The context passed to `Start()` is passed directly to `OnStart` — `OnStartTimeout` and `OnStopTimeout` are ignored, giving the test full control over timeout behavior.
- Both call `OnStart`/`OnStop` on the daemon. The only difference is signal handling, the default `Bindings` implementation, and the context model.
- Both `Start()` and `Serve()` do not return until they verify all bound ports are open and accepting TCP connections. This avoids any race conditions which might occur between start and sending requests to the server.
- `Serve()` exit code mapping: all failures (`OnStart` error, bind failure, `OnStop` error, `Cleaner.Clean` error) return `ExitFailure`. Clean shutdowns — signal-triggered or ctx-cancel triggered — return `ExitSuccess`. There is no third exit code in v1.
- `args []string` is reserved on the `Serve` signature; it is not interpreted by scaffold in v1.

```go
func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

    os.Exit(scaffold.Serve(context.Background(), os.Args, &MyDaemon{}, scaffold.Options{
        Log: logger,
    }))
}
```

```go
type Instance struct { ... }

func (i *Instance) Stop(ctx context.Context) error   // shuts down all listeners, calls OnStop, then Cleaner.Clean
func (i *Instance) Addr(name string) net.Addr        // returns address of named binding; panics if name was never added
```

`Instance.Stop()` accepts a `context.Context` so tests control shutdown timeout budget precisely. The context is passed directly to `http.Server.Shutdown` for draining, then to `OnStop()`, and then to `Cleaner.Clean()` as separate sequential calls — `OnStopTimeout` is ignored.

## Bindings and Port Lifecycle

Bindings are created inside `OnStart` by calling `sc.Bindings.Add()`. Port numbers are read from config at that point. **Scaffold does not open any port until `OnStart` returns nil.** This eliminates the race between port availability and service readiness: a reachable port means a fully initialized service.

```go
type Options struct {
    Bindings        Bindings       // nil → DefaultBindings in Serve(), TestBindings in Start()
    ConfigProvider  ConfigProvider // nil → EnvConfigProvider
    SecretsProvider ConfigProvider // nil → EnvConfigProvider
    Log             *slog.Logger   // nil → built-in color logger writing to os.Stdout;
                                   //        always colorized, no timestamps.
    OnStartTimeout  time.Duration  // zero → no deadline on the Start context (derived from the caller's ctx in Serve)
    OnStopTimeout   time.Duration  // zero → no deadline on the Shutdown context (derived from context.Background() in Serve)
}
```

### Bindings Interface

```go
type Bindings interface {
    Add(name string, port int) *Binding  // creates a named binding on the given port
    Get(name string) *Binding            // returns nil if name was never added
}
```

`Add` registers a named binding on the given port. Calling `Add` twice with the same name panics — duplicate binding names are a programming error. `Get` retrieves a binding previously created by `Add` — its primary use is in the company wrapper pattern where the wrapper calls `Add` and the inner service calls `Get`. A nil return from `Get` means the binding was never added; an unchecked nil dereference will panic naturally, which is the correct outcome for a programmer error.

Two concrete implementations are provided:

- `DefaultBindings` — used by `Serve()`. Binds to the port provided in `Add()`.
- `TestBindings` — used by `Start()`. Ignores the port provided in `Add()` and binds to a random port on `127.0.0.1` instead.

For e2e tests that need specific ports, supply `DefaultBindings` and a `MapConfigProvider` with exact values:

```go
inst, err := scaffold.Start(ctx, daemon, &scaffold.Options{
    Bindings: &scaffold.DefaultBindings{},
    ConfigProvider: scaffold.MapConfigProvider{
        Values: map[string]string{
            "API_PORT":   "8443",
            "ADMIN_PORT": "9090",
        },
    },
})
```

### Binding as Middleware Chain

A `Binding` is a port declaration and a middleware chain. During `OnStart`, the service registers middleware and handlers directly on the binding. Scaffold does not perform routing — it passes the request through the middleware stack and then to the registered handlers in order.

The request pipeline for a binding is:

```
request → middleware chain → AddRPC() handlers (in order, fallthrough on false) → SetMux() fallback → 404
```

- If an `RPCHandler` returns `true`, the request is handled and the pipeline stops.
- If an `RPCHandler` returns `false`, the next registered handler is tried.
- If all `RPCHandler`s return `false` and a `SetMux` handler is set, it receives the request.
- If no `SetMux` is set, scaffold returns a plain HTTP 404 with no body.

```go
type Binding struct{}

// Middleware — registers binding-wide middleware. Applies to all handlers including SetMux.
// Multiple calls append in registration order.
func (b *Binding) UseMiddleware(mw ...MiddlewareFunc)

// AddRPC — registers one or more RPC handlers. Tried in registration order; fallthrough on false.
func (b *Binding) AddRPC(handlers ...RPCHandler)

// SetMux — registers a fallback handler for requests not matched by any RPCHandler.
// Single assignment — calling SetMux twice on the same binding panics.
func (b *Binding) SetMux(h http.Handler)

// TLS — nil means plain HTTP
func (b *Binding) SetTLS(cfg *tls.Config)

// HTTP server timeouts — zero means no timeout (Go default).
func (b *Binding) SetTimeouts(read, write, idle time.Duration)

// Escape hatch for non-HTTP transports
func (b *Binding) ServeFunc(fn func(net.Listener) error)
```

### RPCHandler Interface

`RPCHandler` is the interface DUH-RPC generated servers satisfy. It extends `ServeHTTP` with a boolean return: `true` means the request was handled, `false` means it was not matched and the next handler in the chain should be tried.

```go
// In scaffold
type RPCHandler interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request) bool
}
```

Generated code satisfies this interface implicitly via Go's structural typing — no scaffold import is required in generated packages:

```go
// In generated v1 package — no scaffold import
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
    switch r.URL.Path {
    case TransferFunds:
        r.Pattern = TransferFunds
        s.handleTransferFunds(w, r)
        return true
    case GetBalance:
        r.Pattern = GetBalance
        s.handleGetBalance(w, r)
        return true
    }
    return false // no match — scaffold tries the next RPCHandler
}
```

The generated switch performs an O(1) map lookup with no mux overhead. Because the generated code controls dispatch, it is guaranteed never to write a response before returning `false` — making the fallthrough safe and unambiguous.

### SetMux

`SetMux` registers a fallback `http.Handler` that receives any request not matched by the registered `RPCHandler`s. Calling `SetMux` twice on the same binding panics — duplicate fallback handlers are a programming error. This is the correct integration point for non-DUH endpoints such as Prometheus metrics, GraphQL, or OpenAPI REST routes.

```go
// Prometheus, health, ready — all mounted in a single mux
mux := http.NewServeMux()
mux.Handle("/metrics", promhttp.Handler())
mux.Handle("/healthz", scaffold.HealthHandler(s.CheckHealth))
mux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
api.SetMux(mux)
```

`SetMux` and `ServeFunc` are mutually exclusive. If both are called on the same binding, scaffold panics at startup with a clear message identifying the binding.

### HTTP Server Timeouts

Go's `http.Server` has no timeouts by default. For internet-facing bindings, this means a slow or malicious client can hold a connection open indefinitely. `SetTimeouts` configures the read, write, and idle timeouts for the binding's underlying `http.Server`:

```go
api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
```

- `read` — maximum time to read the full request, including body. Zero means no timeout.
- `write` — maximum time to write the response. Zero means no timeout.
- `idle` — maximum time an idle keep-alive connection is held open. Zero means no timeout.

Reasonable production defaults for a public API binding are `read=10s`, `write=30s`, `idle=120s`. Admin and internal bindings on trusted networks may use larger or zero values.

`SetTimeouts` has no effect on `ServeFunc` bindings.

### ServeFunc

`ServeFunc` hands a `net.Listener` to a foreign protocol's serve loop. Scaffold owns the listener; the foreign server registers its graceful drain with `Cleaner`.

`ServeFunc` and the HTTP pipeline are mutually exclusive on a binding. Scaffold panics at startup if `ServeFunc` is called on a binding that already has `UseMiddleware` or `AddRPC` calls, or if `UseMiddleware`/`AddRPC` are called on a binding that already has `ServeFunc` set — these are programmer errors. Scaffold also panics if both `ServeFunc` and `SetMux` are called on the same binding.

```go
grpcBinding := sc.Bindings.Add("grpc", sc.Config.IntOr("GRPC_PORT", 9000))
grpcServer := grpc.NewServer()
pb.RegisterMyServiceServer(grpcServer, s.grpcImpl)
grpcBinding.ServeFunc(grpcServer.Serve)
sc.Cleaner.Add(func(ctx context.Context) error {
    grpcServer.GracefulStop()
    return nil
})
```

## Middleware

One concept, one type. Standard Go HTTP middleware.

```go
type MiddlewareFunc func(next http.Handler) http.Handler
```

Middleware is binding-wide — it applies to every request on that binding, including those handled by `SetMux`. There is no per-group or per-route middleware scoping in scaffold. If finer-grained middleware is needed, it is the responsibility of the handler or mux.

Middleware can cancel requests (don't call `next`), enrich the request context (`r.WithContext()`), inspect/modify headers, set response codes, and wrap the handler for timing or metrics.

Request body size limits are a middleware concern. Scaffold does not provide a body-size-limit middleware. For DUH-RPC services, the DUH spec enforces its own body size limits at the generated handler level. For non-DUH endpoints behind `SetMux`, the service author is responsible for applying their own body-limit middleware (e.g. wrapping `http.MaxBytesReader`).

### Core middleware inventory

Only `scaffold.PanicRecovery(log)` ships as a built-in middleware in v1. Everything else — request IDs, access logs, CORS, rate limiting, compression — is either written by the service author, pulled from a third-party package, or lifted into a company wrapper. The `scaffold/prometheus` subpackage provides `HTTPMetrics` separately so the core module stays free of the Prometheus client dependency (see Metrics).

### Middleware execution order

```
Binding (UseMiddleware) → PanicRecovery → RequireAuth
                       → RPCHandler chain (AddRPC)
                       → SetMux fallback
                       → 404
```

### Auth middleware — cancellation and context enrichment

```go
func RequireAuth() scaffold.MiddlewareFunc {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            token := r.Header.Get("Authorization")
            if token == "" {
                w.WriteHeader(http.StatusUnauthorized)
                return
            }

            claims, err := validateToken(token)
            if err != nil {
                w.WriteHeader(http.StatusForbidden)
                return
            }

            ctx := auth.WithClaims(r.Context(), claims)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### Middleware vs service method responsibilities

Middleware confirms **identity** — who is making the request. Service methods check **permissions** — what the caller is allowed to do. All authorization decisions live in the handler, co-located with the business logic that determines what roles are needed.

The exception is transport-level access control on entire bindings — like admin endpoints behind mTLS. That's not role checking, that's access control at the transport layer.

## Health and Readiness

Scaffold provides `HealthHandler` and `ReadyHandler` as plain `http.Handler` helpers. They are mounted by the service author into whatever mux is passed to `SetMux`. No special framework integration is required.

```go
func HealthHandler(log *slog.Logger, fn func(ctx context.Context) any) http.Handler
func ReadyHandler(fn func(ctx context.Context) (bool, string)) http.Handler
```

`HealthHandler` returns `200 OK` with `Content-Type: application/json` and a JSON body produced by `fn`. If JSON marshaling fails, it logs the error via the provided `log` and returns `500 Internal Server Error` with body `{"error":"health marshal failed"}` and `Content-Type: application/json`. `log` must be non-nil — `HealthHandler(nil, ...)` panics at construction time; service authors pass `sc.Log`.

`ReadyHandler` returns `200 OK` with an empty body when `fn` returns `true`, or `503 Service Unavailable` with `Content-Type: text/plain; charset=utf-8` and the reason string verbatim as the body when it returns `false`.

```go
mux := http.NewServeMux()
mux.Handle("/healthz", scaffold.HealthHandler(sc.Log, func(ctx context.Context) any {
    return map[string]string{"db": db.Ping()}
}))
mux.Handle("/readyz", scaffold.ReadyHandler(func(ctx context.Context) (bool, string) {
    return db.IsReady(), "waiting for db"
}))
api.SetMux(mux)
```

A bug in a health or readiness handler cannot affect DUH-RPC endpoints — they are dispatched independently.

## Lifecycle Guarantee

```
1. Log "daemon starting"
2. Call OnStart(ctx, sc) — in Serve(): ctx derived from the caller's ctx with OnStartTimeout deadline (if non-zero),
                                       so caller cancellation propagates into OnStart;
                           in Start(): ctx is the caller's context passed directly
3a. OnStart returns error → call OnStop (to clean up partial init) → abort, no ports opened
3b. OnStart returns nil  → open bindings sequentially in Add() order
    → each binding open: log "binding listening" name=<n> addr=<addr>
    → first bind failure: call OnStop → close already-opened listeners → abort
    → all bindings succeed: log "daemon ready" → start serving
4. SIGTERM / SIGINT / ctx cancel received → log "shutdown initiated" reason=<signal|context>
5. Gracefully shut down all listeners — stop accepting new connections and drain in-flight requests
   via `http.Server.Shutdown`. In Serve(): uses the OnStopTimeout context. In Start(): uses the
   context passed to Instance.Stop().
6. Call OnStop(ctx) — same context as step 5
7. Cleaner.Clean(ctx) runs registered functions in LIFO order — same context as step 5
8. Log "daemon stopped"
```

Steps 5, 6, and 7 share a single timeout budget. In `Serve()`, scaffold creates a context from `context.Background()` with the `OnStopTimeout` deadline (if non-zero) and uses it for all three steps. In `Start()`, the context passed to `Instance.Stop()` is used directly — `OnStopTimeout` is ignored.

Steps 6 and 7 are distinct, sequential, scaffold-driven actions. `OnStop` runs first and is for teardown logic the service author expresses imperatively. `Cleaner.Clean` runs second and executes all functions registered via `sc.Cleaner.Add` in LIFO order. `OnStop` must not call `cleaner.Clean` — scaffold guarantees the cleaner runs regardless of what `OnStop` does. If `OnStop` returns an error, scaffold logs it and proceeds to `Cleaner.Clean` — a failed `OnStop` never prevents the cleaner stack from running.

Because steps 5, 6, and 7 share a single deadline, service authors should keep `OnStop` fast and prefer registering teardown via `sc.Cleaner.Add`. If `http.Server.Shutdown` or `OnStop` consumes most of the budget, `Cleaner.Clean` receives whatever time remains.

If `OnStart` returns an error, scaffold calls `OnStop` before returning, then runs `Cleaner.Clean` to unwind any partial initialization. `OnStart` should register cleanup with `sc.Cleaner` immediately alongside each initialization — not deferred to the end — so partial initialization is always cleaned up correctly.

Bindings open sequentially in `Add()` order. If any binding fails to open, scaffold calls `OnStop` first, then closes any already-opened listeners. A partially-bound service never serves traffic.

### Binding serve-loop exit

A binding's serve-loop goroutine returning does **not**, by itself, initiate daemon shutdown. Shutdown is initiated only by the documented triggers above: SIGTERM/SIGINT, cancellation of the ctx passed to `Serve`, `Instance.Stop`, an `OnStart` error, or a `net.Listen` failure. If a binding stops serving on its own — an HTTP server crashing, a `ServeFunc` foreign server exiting unexpectedly — scaffold logs the event but leaves the remaining bindings running. The classification of "expected" vs. "unexpected" uses a scaffold-owned shutdown-requested flag rather than per-protocol sentinels, so new foreign serve loops are supported without scaffold needing to know their graceful-stop errors.

The operational consequence: scaffold does not self-terminate on a single binding's failure. Service authors running multi-binding daemons should wire external liveness probes (Kubernetes liveness, systemd, or a `ReadyHandler` reflecting binding health) to decide whether a partially-dark process should be restarted. Authors who want a binding's crash to tear down the whole daemon can cancel the daemon's outer context from inside their own serve loop — that is an explicit opt-in, not a scaffold default. See ADR-0003 for the full rationale.

## Dependency Injection Pattern

Dependencies live in an `Inject` struct on the daemon. `OnStart` checks each field — if nil, create the real implementation; if set (by a test), use it as-is.

```go
type MyInjectables struct {
    Store          Store
    AccountsClient accounts.Client
    S3Client       s3.Client
}

type MyDaemon struct {
    Inject MyInjectables
}

func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    if s.Inject.Store == nil {
        db, err := mongo.Connect(ctx, sc.Config.StringOr("MONGO_URL", "mongodb://localhost:27017"))
        if err != nil {
            return err
        }
        s.Inject.Store = MyMongoStore{DB: db}
        sc.Cleaner.Add(func(ctx context.Context) error {
            return db.Shutdown(ctx)
        })
    }
    // ... same pattern for other deps ...
}
```

## Error Contract

Generated code owns its error writing — scaffold does not define or enforce an error format. The DUH-RPC error envelope format is the default and is provided as a helper in the generated package, but any format is valid. Services with a house envelope format can override the error writer on the generated server after construction.

```go
// In generated code — no scaffold dependency
func NewServer(svc Service) *Server
```

## Configuration

Environment variable based (12-factor) by default. CLI arguments take precedence over environment variables. Accessed via `sc.Config` during `OnStart`.

```go
type ConfigProvider interface {
    String(key string) (string, error)
    StringOr(key string, fallback string) string

    Int(key string) (int, error)
    IntOr(key string, fallback int) int

    Int64(key string) (int64, error)
    Int64Or(key string, fallback int64) int64

    Float64(key string) (float64, error)
    Float64Or(key string, fallback float64) float64

    Bool(key string) (bool, error)
    BoolOr(key string, fallback bool) bool

    Duration(key string) (time.Duration, error)
    DurationOr(key string, fallback time.Duration) time.Duration
}
```

**Plain variants** (`Int`, `Duration`, etc.) return an error when the key is not found or the value cannot be parsed. Returning it from `OnStart` causes scaffold to log it via the configured logger before shutdown.

**`Or` variants** (`IntOr`, `DurationOr`, etc.) never return an error. If the key is missing the fallback is used silently. If the key exists but cannot be parsed, scaffold logs a warning and uses the fallback.

The default `ConfigProvider` is `EnvConfigProvider` which reads environment variables.

```go
scaffold.Options{
    ConfigProvider: scaffold.MapConfigProvider{
        Values: map[string]string{
            "API_PORT":   "8443",
            "ADMIN_PORT": "9090",
        },
    },
}
```

`MapConfigProvider` is a struct with `Values map[string]string` and `Logger *slog.Logger` fields. A nil `Values` is equivalent to an empty map: every lookup returns not-found or the configured fallback, so `MapConfigProvider{}` is a valid zero-value provider.

### Secrets

`sc.Secrets` is a second `ConfigProvider` on `DaemonConfig` — same interface, different backing source. For production, `FileConfigProvider` covers the common pattern of secrets mounted as files:

```go
type FileConfigProvider struct {
    Dir string
}
```

| Environment | ConfigProvider        | SecretsProvider          |
|-------------|----------------------|--------------------------|
| Dev         | `EnvConfigProvider`  | `EnvConfigProvider`      |
| Production  | `EnvConfigProvider`  | `FileConfigProvider`     |
| Tests       | `MapConfigProvider`  | `MapConfigProvider`      |

## Logging

`sc.Log` is a `*slog.Logger` available throughout `OnStart`, middleware, and handlers. It is also accessible via `scaffold.GetLogger(ctx)` on the context passed to `OnStart` and any contexts derived from it during startup — it is **not** available on per-request contexts (see scaffold.DaemonConfig above).

When `Options.Log` is nil, scaffold uses a built-in color logger that writes human-readable output to `os.Stdout`. Output is always ANSI-colorized and timestamps are omitted — the logger is a developer convenience, not a production target. Users who pipe output to a file or need structured JSON logs should provide their own `*slog.Logger` via `Options.Log`. The built-in logger is configured with `slog.LevelInfo` (matching slog's own default).

## Metrics

Prometheus support lives in the `scaffold/prometheus` subpackage, keeping the main scaffold module free of the Prometheus client dependency. Mount the scrape endpoint via `SetMux`:

```go
import (
    "github.com/prometheus/client_golang/prometheus/promhttp"
    sprometheus "github.com/your-org/scaffold/prometheus"
)

func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), sprometheus.HTTPMetrics(nil))
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
    api.AddRPC(v1.NewServer(s.svc))

    admin := sc.Bindings.Add("admin", sc.Config.IntOr("ADMIN_PORT", 9090))
    adminMux := http.NewServeMux()
    adminMux.Handle("/healthz", scaffold.HealthHandler(sc.Log, s.CheckHealth))
    adminMux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
    adminMux.Handle("/metrics", promhttp.Handler())
    admin.SetMux(adminMux)

    return nil
}
```

`sprometheus.HTTPMetrics` records per-request metrics. It records:

- `http_requests_total` — counter, labels: `method`, `pattern`, `status`
- `http_request_duration_seconds` — histogram, labels: `method`, `pattern`, `status`

The `pattern` label uses Go 1.23's `r.Pattern` field rather than the raw request path, avoiding cardinality explosion. Generated RPCHandler code sets `r.Pattern` to the matched endpoint constant (e.g. `/v1/transfer.funds`) before calling the service method. For requests handled by `SetMux`, Go's `ServeMux` sets `r.Pattern` automatically. The metrics middleware reads `r.Pattern` after calling `next.ServeHTTP`, so the value is available regardless of which handler matched. Note that `r.Pattern` is not set until dispatch — middleware running before the handler must not rely on it.

## TLS

Scaffold uses `github.com/kapetan-io/tackle/autotls` directly rather than providing its own TLS helper. Tackle's `autotls.Config` accepts file paths or PEM buffer inputs, supports outbound mTLS, defaults to TLS 1.3 minimum, and integrates with `*slog.Logger`. Refer to the tackle package for the full type; scaffold consumes it unchanged.

Daemons call `autotls.Setup(&cfg)` to validate the config and populate `cfg.ServerTLS` / `cfg.ClientTLS`, then hand `ServerTLS` to a binding via `SetTLS`:

```go
import "github.com/kapetan-io/tackle/autotls"

func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    tlsCfg := autotls.Config{
        CaFile:   sc.Secrets.StringOr("TLS_CA_FILE", ""),
        CertFile: sc.Secrets.StringOr("TLS_CERT_FILE", ""),
        KeyFile:  sc.Secrets.StringOr("TLS_KEY_FILE", ""),
    }
    if err := autotls.Setup(&tlsCfg); err != nil {
        return fmt.Errorf("tls setup: %w", err)
    }

    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8443))
    api.SetTLS(tlsCfg.ServerTLS)
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log))
    api.AddRPC(v1.NewServer(s.svc))

    return nil
}
```

### TLS in Tests

`TestBindings` honors `SetTLS`. Tests use `AutoTLS: true` to generate ephemeral self-signed certs at startup:

```go
tlsCfg := autotls.Config{AutoTLS: true}
require.NoError(t, autotls.Setup(&tlsCfg))

daemon := &MyDaemon{Inject: MyInjectables{Store: fakeStore(), TLSCfg: tlsCfg}}
inst, err := scaffold.Start(ctx, daemon, nil)
require.NoError(t, err)

c := &http.Client{
    Transport: &http.Transport{TLSClientConfig: tlsCfg.ClientTLS},
}
```

## Graceful Shutdown

`Cleaner` is a LIFO stack. Functions registered last are called first, naturally producing correct teardown order.

```go
sc.Cleaner.Add(func(ctx context.Context) error {
    return db.Shutdown(ctx)
})
```

Cleaner functions return `error`. Scaffold logs any returned errors and continues — a single failure never prevents the rest of the stack from running. Scaffold also recovers panics within cleaner functions. The full LIFO stack always runs to completion.

## Code Generation

### Separate specs per API version

```
api/
  v1/openapi.yaml   → generates package api/v1
  v2/openapi.yaml   → generates package api/v2
```

### Generated artifacts per version

- **Constants** for every endpoint path — compile-time safety.
- **Service interface** — one method per endpoint, fully typed.
- **Server struct** — implements `RPCHandler` implicitly. No scaffold import. Dispatches to the service interface via a switch statement. Owns its own error writing.
- **Encode/decode and error-writer helpers** — `decode`, `writeError`, `writeResponse` are generated into the package by DUH (or hand-written by the author for non-DUH handlers). Scaffold has no dependency on these helpers and no opinion about their format. See the Error Contract section above.
- **Client** — used in tests and for service-to-service calls.

```go
package v1

const (
    TransferFunds = "/v1/transfer.funds"
    GetBalance    = "/v1/get.balance"
)

type Service interface {
    TransferFunds(ctx context.Context, req *TransferFundsRequest) (*TransferFundsResponse, error)
    GetBalance(ctx context.Context, req *GetBalanceRequest) (*GetBalanceResponse, error)
}

// No scaffold import — RPCHandler interface satisfied implicitly
func NewServer(svc Service) *Server

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
    switch r.URL.Path {
    case TransferFunds:
        r.Pattern = TransferFunds
        s.handleTransferFunds(w, r)
        return true
    case GetBalance:
        r.Pattern = GetBalance
        s.handleGetBalance(w, r)
        return true
    }
    return false
}
```

### Generated dispatch

```go
// decode, writeError, and writeResponse are generated into this package by DUH
// (or hand-written by the author) — they are NOT provided by scaffold.
func (s *Server) handleTransferFunds(w http.ResponseWriter, r *http.Request) {
    var req TransferFundsRequest
    if err := decode(r, &req); err != nil {
        writeError(w, err)
        return
    }

    resp, err := s.svc.TransferFunds(r.Context(), &req)
    if err != nil {
        writeError(w, err)
        return
    }
    writeResponse(w, resp)
}
```

## Company Wrapper Pattern

Because `scaffold.DaemonConfig` is a plain exported struct passed as a pointer, organizations can build a company-standard base layer on top of scaffold without forking it.

The wrapper calls `sc.Bindings.Add()` to create bindings with company-standard ports and wires company-standard middleware. The inner service receives a `CompanyConfig` that exposes the pre-configured bindings directly for `AddRPC` calls.

```go
package mycompany

type CompanyConfig struct {
    *scaffold.DaemonConfig
    APIBinding   *scaffold.Binding // pre-configured with company middleware
    AdminBinding *scaffold.Binding // pre-configured for admin use
}

type Daemon interface {
    OnStart(ctx context.Context, cc *CompanyConfig) error
    OnStop(ctx context.Context) error
    CheckHealth(ctx context.Context) any
    IsReady(ctx context.Context) (bool, string)
}

type BaseDaemon struct {
    daemon Daemon
}

func New(d Daemon) *BaseDaemon {
    return &BaseDaemon{daemon: d}
}

func (b *BaseDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api   := sc.Bindings.Add("api",   sc.Config.IntOr("API_PORT",   8080))
    admin := sc.Bindings.Add("admin", sc.Config.IntOr("ADMIN_PORT", 9090))

    api.UseMiddleware(mycompany.RequestID(), mycompany.Metrics(), mycompany.PanicRecovery(sc.Log))
    api.UseMiddleware(mycompany.RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)

    adminMux := http.NewServeMux()
    adminMux.Handle("/healthz", scaffold.HealthHandler(sc.Log, b.daemon.CheckHealth))
    adminMux.Handle("/readyz", scaffold.ReadyHandler(b.daemon.IsReady))
    admin.SetMux(adminMux)

    return b.daemon.OnStart(ctx, &CompanyConfig{
        DaemonConfig: sc,
        APIBinding:   api,
        AdminBinding: admin,
    })
}

func (b *BaseDaemon) OnStop(ctx context.Context) error {
    return b.daemon.OnStop(ctx)
}
```

Individual services implement the thin company interface and get company-standard wiring for free:

```go
package paymentsvc

type Daemon struct {
    Inject Injectables
}

func (s *Daemon) OnStart(ctx context.Context, cc *mycompany.CompanyConfig) error {
    if s.Inject.Store == nil {
        db, err := mongo.Connect(ctx, cc.Config.StringOr("MONGO_URL", "mongodb://localhost:27017"))
        if err != nil {
            return err
        }
        s.Inject.Store = NewMongoStore(db)
        cc.Cleaner.Add(func(ctx context.Context) error {
            return db.Shutdown(ctx)
        })
    }

    cc.APIBinding.AddRPC(v1.NewServer(s.Inject.Store))

    return nil
}

func (s *Daemon) OnStop(ctx context.Context) error {
    return nil
}

func (s *Daemon) CheckHealth(ctx context.Context) any {
    return map[string]string{"db": s.Inject.Store.Ping()}
}

func (s *Daemon) IsReady(ctx context.Context) (bool, string) {
    if !s.Inject.Store.IsReady() {
        return false, "waiting for db"
    }
    return true, ""
}
```

`main.go`:

```go
func main() {
    os.Exit(scaffold.Serve(
        context.Background(),
        os.Args,
        mycompany.New(&paymentsvc.Daemon{}),
        scaffold.Options{},
    ))
}
```

## Extended Examples

### Basic service wiring

```go
func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
    api.AddRPC(v1.NewServer(s.svc))

    mux := http.NewServeMux()
    mux.Handle("/healthz", scaffold.HealthHandler(sc.Log, s.CheckHealth))
    mux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
    api.SetMux(mux)

    return nil
}
```

### Multi-version API

```go
func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)

    // v1 consulted first, v2 on fallthrough
    api.AddRPC(
        v1.NewServer(s.v1svc),
        v2.NewServer(s.v2svc),
    )

    mux := http.NewServeMux()
    mux.Handle("/healthz", scaffold.HealthHandler(sc.Log, s.CheckHealth))
    mux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
    api.SetMux(mux)

    return nil
}
```

### Multiple bindings — public API + admin

```go
func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
    api.AddRPC(v1.NewServer(s.svc))

    admin := sc.Bindings.Add("admin", sc.Config.IntOr("ADMIN_PORT", 9090))
    adminMux := http.NewServeMux()
    adminMux.Handle("/healthz", scaffold.HealthHandler(sc.Log, s.CheckHealth))
    adminMux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
    adminMux.Handle("/metrics", promhttp.Handler())
    admin.SetMux(adminMux)

    return nil
}
```

### Escape hatch — gRPC

```go
func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
    api.AddRPC(v1.NewServer(s.svc))

    grpcBinding := sc.Bindings.Add("grpc", sc.Config.IntOr("GRPC_PORT", 9000))
    grpcServer := grpc.NewServer()
    pb.RegisterMyServiceServer(grpcServer, s.grpcImpl)
    grpcBinding.ServeFunc(grpcServer.Serve)
    sc.Cleaner.Add(func(ctx context.Context) error {
        grpcServer.GracefulStop()
        return nil
    })

    return nil
}
```

### HTTPS API with mTLS admin

```go
func (s *MyDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    apiTLS := autotls.Config{
        CaFile:   sc.Secrets.StringOr("TLS_CA_FILE", ""),
        CertFile: sc.Secrets.StringOr("TLS_CERT_FILE", ""),
        KeyFile:  sc.Secrets.StringOr("TLS_KEY_FILE", ""),
    }
    if err := autotls.Setup(&apiTLS); err != nil {
        return fmt.Errorf("api tls setup: %w", err)
    }

    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8443))
    api.SetTLS(apiTLS.ServerTLS)
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.AddRPC(v1.NewServer(s.svc))

    adminTLS := autotls.Config{
        CaFile:           sc.Secrets.StringOr("TLS_CA_FILE", ""),
        CertFile:         sc.Secrets.StringOr("TLS_CERT_FILE", ""),
        KeyFile:          sc.Secrets.StringOr("TLS_KEY_FILE", ""),
        ClientAuthCaFile: sc.Secrets.StringOr("ADMIN_CLIENT_CA_FILE", ""),
        ClientAuth:       tls.RequireAndVerifyClientCert,
    }
    if err := autotls.Setup(&adminTLS); err != nil {
        return fmt.Errorf("admin tls setup: %w", err)
    }

    admin := sc.Bindings.Add("admin", sc.Config.IntOr("ADMIN_PORT", 9090))
    admin.SetTLS(adminTLS.ServerTLS)
    adminMux := http.NewServeMux()
    adminMux.Handle("/healthz", scaffold.HealthHandler(sc.Log, s.CheckHealth))
    adminMux.Handle("/readyz", scaffold.ReadyHandler(s.IsReady))
    admin.SetMux(adminMux)

    return nil
}
```

### HTTP to HTTPS Redirect

```go
redirect := sc.Bindings.Add("redirect", sc.Config.IntOr("HTTP_PORT", 8080))
redirect.SetMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    target := "https://" + r.Host + r.URL.RequestURI()
    http.Redirect(w, r, target, http.StatusMovedPermanently)
}))
```

## Testing

Two natural levels:

1. **Full integration** — `scaffold.Start()` with `Inject` containing fakes. Full middleware stack applies. Real HTTP calls via generated client. Pass a valid test token for auth.
2. **Unit** — call the service method directly. No HTTP, no middleware, no framework.

```go
// Full integration — production middleware, fake dependencies, random ports
daemon := &my.Daemon{Inject: my.Injectables{Store: fakeStore()}}
inst, err := scaffold.Start(ctx, daemon, nil)
require.NoError(t, err)
// ... make HTTP calls ...
require.NoError(t, inst.Stop(ctx))

// Full integration — specific ports for e2e
inst, err := scaffold.Start(ctx, daemon, &scaffold.Options{
    Bindings: &scaffold.DefaultBindings{},
    ConfigProvider: scaffold.MapConfigProvider{Values: map[string]string{"API_PORT": "8443"}},
})

// Unit — direct method call, no framework
svc := &MyServiceImpl{Store: fakeStore()}
resp, err := svc.TransferFunds(auth.TestContext(claims), &v1.TransferFundsRequest{...})
```
