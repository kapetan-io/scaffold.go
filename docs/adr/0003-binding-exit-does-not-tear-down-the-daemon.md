# 3. Binding exit does not tear down the daemon

Date: 2026-04-19

## Status

Accepted

## Context

A scaffold daemon runs one goroutine per binding. For HTTP bindings, the goroutine calls `srv.Serve(listener)` (or `srv.ServeTLS`), which always returns an error on exit — `http.ErrServerClosed` after a graceful `srv.Shutdown`, some other error otherwise. For `ServeFunc` bindings, the goroutine calls a foreign serve loop (gRPC, custom protocol, etc.) whose return value has no uniform meaning: one server returns `nil` on graceful stop, another returns a sentinel such as `grpc.ErrServerStopped`, a third may simply crash with an unrelated error.

Two decisions are entangled: **whether** a binding's exit should tear down the rest of the daemon, and **how** scaffold should distinguish an expected exit (part of a graceful stop) from an unexpected one (the binding failed on its own).

On the first question, the forces are:

- Many scaffold services run multiple bindings with distinct roles — a public API plus an admin / metrics port. Killing the whole daemon because the admin port unexpectedly stopped is strictly worse for users than logging the event and continuing to serve the API.
- Kubernetes, systemd, and similar supervisors already watch process liveness through orthogonal health probes. Scaffold tearing itself down when a binding dies duplicates that role inconsistently.
- A teardown policy forces scaffold to make a judgement call on every binding exit. The judgement is wrong whenever the "exit" was actually a successful graceful stop.

On the second question, two classification strategies were considered:

**Per-protocol sentinel matching.** Scaffold hard-codes recognition of `http.ErrServerClosed`, `grpc.ErrServerStopped`, and any other "expected shutdown" sentinel it knows about. The downside is that the set is open-ended: every new foreign integration brings its own sentinels, and scaffold either keeps growing a matching table or silently mis-classifies unknowns as "unexpected."

**Shutdown-requested flag.** Scaffold already knows whether it has initiated shutdown — signals, context cancellation, `Instance.Stop`, `OnStart` errors, and bind failures all route through scaffold's own control flow. An atomic flag set at the moment shutdown begins provides a uniform "was this exit expected?" answer for every binding type, without per-protocol knowledge. The cost is one atomic read on goroutine return, plus ensuring the flag is set *before* scaffold takes any action that would cause a goroutine to return (calling `srv.Shutdown`, invoking the user's Cleaner functions that stop foreign servers).

## Decision

A binding's serve-loop goroutine returning does not, by itself, initiate daemon shutdown. Shutdown is initiated only by the documented triggers: `SIGTERM`, `SIGINT`, cancellation of the context passed to `Serve`, an explicit call to `Instance.Stop`, an `OnStart` error, or a `net.Listen` failure.

Classification of a goroutine return uses a single mechanism: an atomic shutdown-requested flag owned by the daemon. The flag is set at the moment any of those triggers fires, and always before scaffold calls `srv.Shutdown` or runs the user's Cleaner functions. On goroutine return, scaffold consults the flag: if set, the return is expected (logged at debug level for diagnostics, not treated as a problem); if not set, the return is unexpected and logged at error level with the binding name and error. This applies uniformly to HTTP goroutines and `ServeFunc` goroutines — scaffold does not recognize per-protocol shutdown sentinels.

Service authors who want a specific binding's crash to tear down the whole daemon can cancel the daemon's outer context from inside their own serve loop. That is an explicit opt-in, not a scaffold default.

## Consequences

Multi-binding daemons degrade gracefully: one port going dark does not cost the other ports their traffic. This is the right default for services running a public API alongside an admin or metrics endpoint.

Scaffold stays protocol-agnostic for `ServeFunc` bindings. Adding support for a new foreign server (a new RPC framework, a custom wire protocol) does not require teaching scaffold that framework's graceful-stop sentinel. A correctly-written user Cleaner that stops the foreign server will produce "expected" exits automatically, because the Cleaner runs inside the shutdown path during which the flag is already set.

Failures become quieter. A binding that has silently stopped serving is visible only through the error log and through health / readiness checks that the service author is responsible for wiring up. Operators running scaffold services must treat binding-exit error logs as actionable alerts rather than informational noise. Diagnosing "why is traffic on one port stalled" requires looking at logs for the binding-exit line; it cannot be inferred from the daemon's process status alone, because the process is still running.

The decision also preserves symmetry with the shutdown sequencing captured in ADR-0002: scaffold does not route around or second-guess a misbehaving binding. A binding that exits unexpectedly is treated with the same deference as a binding that drains slowly — logged, left alone, handed to the operator to resolve.

Implementation-wise, this imposes one ordering constraint the code must respect: the flag must be set before any action that induces a binding goroutine to return. Setting the flag *after* calling `srv.Shutdown` would create a race where the goroutine returns `http.ErrServerClosed`, reads an unset flag, and logs an expected shutdown as an error. The flag flip happens at the top of every shutdown path.
