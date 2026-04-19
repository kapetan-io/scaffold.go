# 2. Serial shutdown in reverse Add order

Date: 2026-04-19

## Status

Accepted

## Context

A scaffold daemon may run multiple bindings simultaneously — for example, a public API on one port and an admin endpoint on another. On shutdown, every binding must stop accepting new connections and drain in-flight requests within a single shared timeout budget (`OnStopTimeout`, or the context passed to `Instance.Stop`).

Two approaches were considered for sequencing shutdown across bindings:

**Parallel.** Spawn a goroutine per binding, each calling `http.Server.Shutdown(ctx)` against the shared deadline context. All bindings race to complete within the budget. Maximizes wall-clock throughput during shutdown; all listeners stop accepting new traffic immediately regardless of how many there are.

**Serial in reverse `Add` order.** Shut down bindings one at a time, in the reverse of the order in which they were registered via `Bindings.Add`. Shutdown becomes the mirror image of startup. The last binding opened is the first closed.

The forces in tension are predictability versus throughput. Parallel shutdown finishes sooner when every binding drains promptly, but it introduces concurrent teardown: if two bindings share resources, the order in which their handlers release those resources during drain is unspecified. It also means a later binding's shutdown error can race with an earlier binding's, complicating diagnosis. Serial shutdown is slower in the worst case — a binding whose drain consumes most of the budget starves subsequent bindings — but the ordering is deterministic and mirrors the well-understood startup sequence.

A secondary concern is that services using the company-wrapper pattern register admin bindings and API bindings in a specific order; reverse-order shutdown ensures that wrapper-registered infrastructure (typically added first) outlives the inner service's bindings (typically added later), matching the usual dependency direction.

## Decision

We will shut down HTTP bindings serially, in the reverse of the order in which they were added via `Bindings.Add`. Each binding's `http.Server.Shutdown` is called with the shared shutdown context and completes before the next binding begins.

Bindings registered via `ServeFunc` are skipped during this phase. Their shutdown is the author's responsibility via `Cleaner.Add`, which also runs in LIFO order.

## Consequences

Shutdown becomes the mirror image of startup, which makes behavior easy to reason about and debug. Dependency ordering between bindings — when it exists — is respected by construction: infrastructure registered first outlives services registered later.

The tradeoff is that a single slow binding can exhaust the shared timeout budget and leave subsequent bindings with little or no time to drain. Service authors mitigate this by setting appropriate `SetTimeouts` values and keeping handlers responsive to context cancellation. The framework does not route around a misbehaving binding.

Implementation is simpler than the parallel alternative: no goroutine coordination, no error aggregation, no race between concurrent `Shutdown` calls. Logs and error messages reflect a linear sequence.

Throughput during shutdown is bounded by the sum of per-binding drain times rather than the maximum; in the common case of a handful of bindings with prompt drains, the difference is imperceptible.
