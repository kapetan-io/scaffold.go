# Scaffold

[![GitHub tag](https://img.shields.io/github/tag/kapetan-io/scaffold?include_prereleases=&sort=semver&color=blue)](https://github.com/kapetan-io/scaffold/releases/)
[![CI](https://github.com/kapetan-io/scaffold/workflows/CI/badge.svg)](https://github.com/kapetan-io/scaffold/actions?query=workflow:"CI")
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

`scaffold.DaemonConfig` is a plain exported struct passed as a pointer.
Platform teams can build a company-standard base layer on top of scaffold
without forking it — company middleware, binding conventions, and admin
ports are applied once in the wrapper; individual services implement a
thinner company interface and get the wiring for free.

```go
package mycompany

type CompanyConfig struct {
    *scaffold.DaemonConfig
    APIBinding   *scaffold.Binding // pre-configured with company middleware
    AdminBinding *scaffold.Binding // metrics, healthz, readyz mounted
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

`main.go` is the usual `scaffold.Serve` call, wrapping the service with the
company base layer:

```go
os.Exit(scaffold.Serve(ctx, os.Args, mycompany.New(&paymentsvc.Daemon{}), scaffold.Options{}))
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
