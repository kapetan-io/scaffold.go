# Scaffold

[![GitHub tag](https://img.shields.io/github/tag/kapetan-io/scaffold.go?include_prereleases=&sort=semver&color=blue)](https://github.com/kapetan-io/scaffold/releases/)
[![CI](https://github.com/kapetan-io/scaffold.go/workflows/CI/badge.svg)](https://github.com/kapetan-io/scaffold/actions?query=workflow:"CI")
[![License](https://img.shields.io/badge/License-Apache-blue)](#license)

Scaffold is a Go framework for building HTTP API services. It owns the
structural shell — process lifecycle, signal handling, port binding,
middleware chain, graceful shutdown ordering, config and secrets loading,
health and readiness endpoints, TLS, and panic recovery — so that service
authors can focus on business logic and platform teams have a stable seam
to enforce company standards.

## Why Scaffold

Every new Go service hand-rolls the same boilerplate: `main()` that parses
flags, wires a logger, loads config, opens ports, builds a middleware chain,
handles SIGTERM, drains connections, and runs cleanup in a sensible order.
The result is drift — two services in the same company end up with subtly
different shutdown behavior, different middleware layering, and different
test harnesses — and every service spends weeks reaching parity before it
writes a line of business logic.

Incidents repeatedly trace back to lifecycle bugs: connections not drained,
cleanup skipped, readiness reported before the service was actually ready.
Platform teams enforcing company-wide standards (request IDs, auth, metrics,
binding conventions) have no stable seam to hook into, so standards are
copy-pasted into each service and drift.

Scaffold moves this work out of service code and into a shared framework
with a small, explicit surface area.

## Quick Start

```go
package main

import (
    "context"
    "net/http"
    "os"

    "github.com/kapetan-io/scaffold"
)

type Daemon struct{}

func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log))

    mux := http.NewServeMux()
    mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte("hello, world"))
    })
    mux.Handle("/readyz", scaffold.ReadyHandler(func(_ context.Context) (bool, string) {
        return true, ""
    }))
    api.SetMux(mux)

    return nil
}

func (d *Daemon) OnStop(ctx context.Context) error { return nil }

func main() {
    os.Exit(scaffold.Serve(context.Background(), os.Args, &Daemon{}, scaffold.Options{}))
}
```

`Serve` opens the port only after `OnStart` returns nil, blocks until
SIGTERM / SIGINT / caller context cancellation, and then drains in-flight
requests, calls `OnStop`, and runs any teardown registered with
`sc.Cleaner.Add`. All three steps share a single `OnStopTimeout` budget.

## Lifecycle

Scaffold runs a daemon through a deterministic sequence. Every step happens
in order; failures at any point trigger a partial shutdown that tears down
only what was already set up.

```
Options resolved (address, logger, config, bindings)
    │
    ▼
OnStart(ctx, DaemonConfig)          ◄─ register bindings, middleware, handlers
    │
    ▼
Listeners opened (Add order)        ◄─ net.Listen on each binding
    │
    ▼
OnListenReady callbacks             ◄─ all ports are dialable
    │
    ▼
"daemon ready" log
    │
    ▼
Blocking (Serve: signals/ctx)       ◄─ Start: returns *Instance immediately
    │
    ▼
BeforeShutdown callbacks            ◄─ listeners still open
    │
    ▼
Listeners closed (reverse order)    ◄─ graceful drain via http.Server.Shutdown
    │
    ▼
OnStop(ctx)
    │
    ▼
Cleaner runs (LIFO)
    │
    ▼
"daemon stopped" log
```

### `Start` vs `Serve`

| | `Start` | `Serve` |
|---|---|---|
| Signal handling | No | SIGTERM, SIGINT |
| Timeouts | No | `OnStartTimeout`, `OnStopTimeout` |
| Default bindings | `TestBindings` (127.0.0.1:0) | `DefaultBindings` (BindAddress:port) |
| Returns | `(*Instance, error)` | `int` (exit code) |
| Blocking | Returns immediately | Blocks until signal or ctx cancel |

Use `Start` in tests and embedding scenarios. Use `Serve` in `main()`.

### Lifecycle Callbacks

Register callbacks during `OnStart` to hook into specific lifecycle moments:

```go
func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    sc.AddOnListenReady(func(ctx context.Context) {
        // All ports are open and dialable. Register with service discovery,
        // start background workers, or flip readiness to true.
    })

    sc.AddBeforeShutdown(func(ctx context.Context) {
        // Shutdown was triggered but listeners are still open.
        // Deregister from service discovery, drain queues, or
        // send "going away" to peers while they can still reach you.
    })

    // ...
    return nil
}
```

`OnListenReady` fires after all binding listeners are open and dialable,
before the "daemon ready" log. `BeforeShutdown` fires after shutdown is
triggered but before any listener begins closing — only on normal shutdown
paths (not OnStart errors or bind failures).

## Bindings

A binding is a named network listener with its own middleware stack and
handler. Register bindings during `OnStart`:

```go
func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api := sc.Bindings.Add("api", 8080)
    api.UseMiddleware(scaffold.PanicRecovery(sc.Log))
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)

    mux := http.NewServeMux()
    mux.HandleFunc("/hello", helloHandler)
    api.SetMux(mux)

    return nil
}
```

`Add` registers a binding with a port hint. In production (`Serve`), the
binding listens on `BindAddress:port`. In tests (`Start` with nil bindings),
`TestBindings` ignores the port and assigns a random port on `127.0.0.1`.
After startup, read the actual address via `inst.Addr("api")`.

Bindings open in Add order and close in reverse order during shutdown. This
guarantees that dependencies registered later shut down first.

### RPC Handlers

For services using RPC frameworks (gRPC, DUH-RPC), bindings support an RPC
handler chain that runs before the mux:

```go
api.AddRPC(v1.NewPaymentServer(svc), v1.NewAccountServer(svc))
api.SetMux(fallbackMux) // handles requests no RPC handler claimed
```

Each `RPCHandler` returns `true` to claim the request or `false` to pass it
down the chain. The mux is the final fallback.

### Terminal error replies

When no `RPCHandler` claims a request and the mux (if any) has no matching
route, scaffold answers with a DUH-shaped JSON Reply rather than an empty body
or `net/http.ServeMux`'s plain-text `404 page not found`:

```json
{"code":"404","message":"no route matches GET /unknown"}
```

The same JSON shape is written for the `500` that `PanicRecovery` produces when
a handler panics. Both are the terminal responses scaffold emits on its own
behalf, so RPC clients always get a machine-readable body to recover from.

Override either with `SetErrorHandler`, keyed by a fixed `ErrorStatus` (only
the codes scaffold itself emits can be registered):

```go
api.SetErrorHandler(scaffold.ErrorStatus404, didYouMeanHandler) // unmatched route
api.SetErrorHandler(scaffold.ErrorStatus500, internalErrHandler) // recovered panic
```

The `ErrorStatus500` handler is used only when `PanicRecovery` is in the
binding's middleware chain.

### ServeFunc

For non-HTTP protocols (gRPC with its own server, custom TCP), use
`ServeFunc` to take ownership of the raw listener:

```go
grpcBinding := sc.Bindings.Add("grpc", 9090)
grpcServer := grpc.NewServer()
grpcBinding.ServeFunc(func(ln net.Listener) error {
    return grpcServer.Serve(ln)
})
sc.Cleaner.Add(func(ctx context.Context) error {
    grpcServer.GracefulStop()
    return nil
})
```

When using `ServeFunc`, the caller is responsible for graceful shutdown via
the Cleaner.

## Network Identity

Scaffold gives every daemon a network identity: a `BindAddress` that all
bindings listen on and an `AdvertisedAddress` that services use for peer
communication and service registration.

```go
scaffold.Options{
    BindAddress:       "10.0.1.5",   // listen on this interface
    AdvertisedAddress: "203.0.113.1", // tell peers to reach me here
}
```

Both are resolved before `OnStart` and exposed on `DaemonConfig`:

```go
func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    // Register with Consul using the resolved advertised address
    consul.Register(sc.AdvertisedAddress, sc.Bindings.Get("api").Addr())
    return nil
}
```

### Defaults

| Scenario | `BindAddress` | `AdvertisedAddress` |
|---|---|---|
| Zero config (Serve) | `0.0.0.0` | Auto-detected private IP |
| Zero config (Start/tests) | `127.0.0.1` | `127.0.0.1` |
| Explicit bind, no advertised | Configured value | Same as bind address |
| Both explicit | Configured value | Configured value |

Auto-detection picks the first non-loopback, non-link-local, private IP
from the system's network interfaces — the same heuristic used by HashiCorp
Consul and Nomad.

### Kubernetes Example

```yaml
env:
  - name: BIND_ADDR
    valueFrom:
      fieldRef:
        fieldPath: status.podIP
```

```go
scaffold.Options{
    BindAddress: os.Getenv("BIND_ADDR"),
    // AdvertisedAddress derived from BindAddress automatically
}
```

## Config and Secrets

Scaffold provides two `ConfigProvider` slots — one for application config,
one for secrets — so sensitive values stay separate from general configuration.

```go
scaffold.Options{
    ConfigProvider:  &scaffold.EnvConfigProvider{Logger: log},
    SecretsProvider: &scaffold.FileConfigProvider{Dir: "/run/secrets", Logger: log},
}
```

Service authors consume them from `DaemonConfig`:

```go
func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    dbURL := sc.Config.StringOr("DATABASE_URL", "postgres://localhost:5432/mydb")
    apiKey, err := sc.Secrets.String("API_KEY")
    // ...
}
```

Three built-in providers:

- **`EnvConfigProvider`** — reads from environment variables on every call.
- **`FileConfigProvider`** — reads from `{Dir}/{key}` files; designed for
  Kubernetes ConfigMap and Secret volume mounts.
- **`MapConfigProvider`** — in-memory map; useful in tests.

Every provider supports typed accessors (`String`, `Int`, `Float64`, `Bool`,
`Duration`) and fallback variants (`StringOr`, `IntOr`, etc.) that return a
default on missing keys.

## Cleaner

`DaemonConfig.Cleaner` is a LIFO cleanup stack. Register teardown functions
during `OnStart`; they run automatically after `OnStop` in reverse order.

```go
func (d *Daemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    db, err := sql.Open("postgres", sc.Config.StringOr("DATABASE_URL", "..."))
    if err != nil {
        return err
    }
    sc.Cleaner.Add(func(ctx context.Context) error {
        return db.Close()
    })
    // db is guaranteed to be closed during shutdown, even if OnStop panics
    return nil
}
```

## Surface Testing

Scaffold ships a parallel `Start` entry point so tests exercise the same
code path as production, through the public HTTP surface, with no internal
reach-through. `Start` uses `TestBindings` by default (random ports on
`127.0.0.1`), returns an `*Instance`, and lets dependencies be substituted
with fakes.

```go
func TestHello(t *testing.T) {
    daemon := &my.Daemon{
        Inject: my.Injectables{
            Greeter: func() string { return "hello from test" },
        },
    }

    inst, err := scaffold.Start(context.Background(), daemon, nil)
    require.NoError(t, err)
    defer func() { _ = inst.Stop(context.Background()) }()

    resp, err := http.Get("http://" + inst.Addr("api").String() + "/hello")
    require.NoError(t, err)
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    assert.Equal(t, http.StatusOK, resp.StatusCode)
    assert.Equal(t, "hello from test", string(body))
}
```

The full middleware stack runs. The listener is real. The request is real.
The only thing that changes between test and production is the `Inject`
struct — a fake store replaces Mongo, a fake S3 client replaces S3. See
`example_test.go` for a runnable version of this pattern.

## Platform-Specific Scaffolds

`Options` is the platform team's contract for injecting platform concerns.
The recommended pattern is for the platform team to export a `Run` function
that constructs `Options` from platform configuration and calls `Serve`:

```go
package mycompany

func Run(ctx context.Context, args []string, d scaffold.Daemon) int {
    return scaffold.Serve(ctx, args, mycompany.New(d), scaffold.Options{
        BindAddress:       platform.BindAddress(),
        AdvertisedAddress: platform.AdvertisedAddress(),
        Log:               platform.Logger(),
        ConfigProvider:    platform.Config(),
        SecretsProvider:   platform.Secrets(),
    })
}
```

Service authors call the platform `Run` function and never touch `Options`
directly:

```go
func main() {
    os.Exit(mycompany.Run(context.Background(), os.Args, &paymentsvc.Daemon{}))
}
```

`DaemonConfig` is a plain exported struct passed as a pointer. Platform
teams can build a company-standard base layer on top of scaffold without
forking it — company middleware, binding conventions, and admin ports are
applied once in the wrapper; individual services implement a thinner company
interface and get the wiring for free.

```go
type CompanyConfig struct {
    *scaffold.DaemonConfig
    APIBinding   *scaffold.Binding
    AdminBinding *scaffold.Binding
}

type Daemon interface {
    OnStart(ctx context.Context, cc *CompanyConfig) error
    OnStop(ctx context.Context) error
    CheckHealth(ctx context.Context) any
    IsReady(ctx context.Context) (bool, string)
}

type BaseDaemon struct{ daemon Daemon }

func New(d Daemon) *BaseDaemon { return &BaseDaemon{daemon: d} }

func (b *BaseDaemon) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
    api   := sc.Bindings.Add("api",   sc.Config.IntOr("API_PORT",   8080))
    admin := sc.Bindings.Add("admin", sc.Config.IntOr("ADMIN_PORT", 9090))

    api.UseMiddleware(RequestID(), Metrics(), scaffold.PanicRecovery(sc.Log), RequireAuth())
    api.SetTimeouts(10*time.Second, 30*time.Second, 120*time.Second)

    mux := http.NewServeMux()
    mux.Handle("/healthz", scaffold.HealthHandler(sc.Log, b.daemon.CheckHealth))
    mux.Handle("/readyz",  scaffold.ReadyHandler(b.daemon.IsReady))
    mux.Handle("/metrics", promhttp.Handler())
    admin.SetMux(mux)

    return b.daemon.OnStart(ctx, &CompanyConfig{
        DaemonConfig: sc,
        APIBinding:   api,
        AdminBinding: admin,
    })
}

func (b *BaseDaemon) OnStop(ctx context.Context) error { return b.daemon.OnStop(ctx) }
```

A service written against the company interface is a handful of lines:

```go
func (s *Daemon) OnStart(ctx context.Context, cc *mycompany.CompanyConfig) error {
    cc.APIBinding.AddRPC(v1.NewServer(s.svc))
    return nil
}
```

Because `DaemonConfig` is a struct (not an interface) and `Bindings.Get`
retrieves previously-added bindings by name, the wrapper and the inner
service share state without either forking scaffold or inventing a parallel
configuration system.

## Metrics

Prometheus support lives in the `scaffold/prometheus` subpackage, so
services that don't need metrics don't link the Prometheus client. Mount
the scrape endpoint via `SetMux` and add the middleware on any binding
whose requests you want recorded:

```go
import sprometheus "github.com/kapetan-io/scaffold/prometheus"

api.UseMiddleware(scaffold.PanicRecovery(sc.Log), sprometheus.HTTPMetrics(nil))
adminMux.Handle("/metrics", promhttp.Handler())
```

## TLS

Scaffold consumes [`github.com/kapetan-io/tackle/autotls`](https://github.com/kapetan-io/tackle)
unchanged — pass `cfg.ServerTLS` to `binding.SetTLS`. For tests, set
`AutoTLS: true` and `autotls.Setup` generates an ephemeral self-signed pair
at startup.

## Design Reference

The full design is documented in
[docs/features/mvp/prd.md](docs/features/mvp/prd.md). Architecture
decisions are captured as ADRs under [docs/adr/](docs/adr/).
