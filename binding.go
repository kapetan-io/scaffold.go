package scaffold

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// RPCHandler is the interface implemented by handlers that can short-circuit
// the binding's RPC chain. A handler that recognizes and fully serves the
// request returns true; otherwise it returns false and the binding's
// dispatcher consults the next handler (or falls through to the mux).
//
// RPCHandler is the integration point for DUH-RPC generated dispatchers and
// any custom RPC-shaped code that wants participation in the fallthrough
// chain established by AddRPC.
type RPCHandler interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request) bool
}

// Binding accumulates the configuration for a single listener (address,
// middleware, RPC handlers, fallback mux, TLS config, timeouts, or a raw
// ServeFunc). Setters record state and may panic on mutual-exclusion
// violations — see the per-setter doc comments for the full rule set.
//
// Binding values are created by DefaultBindings.Add / TestBindings.Add and
// configured during OnStart. After OnStart returns nil, scaffold composes a
// single http.Handler (middleware wrapped around an RPC-chain dispatcher
// with a mux fallback) and serves it on the listener opened at
// listenAddr.
type Binding struct {
	name       string
	listenAddr string

	mws         []MiddlewareFunc
	rpcHandlers []RPCHandler
	mux         http.Handler
	errHandlers map[ErrorStatus]http.Handler
	tls         *tls.Config
	readTO      time.Duration
	writeTO     time.Duration
	idleTO      time.Duration
	serveFunc   func(net.Listener) error

	finalHandler http.Handler
	addr         net.Addr
	srv          *http.Server
}

// UseMiddleware appends one or more MiddlewareFuncs to this binding's
// pipeline. The first-registered middleware becomes the outermost wrapper
// (runs first on the request ingress path).
//
// Panics if ServeFunc has already been set on this binding (the HTTP
// pipeline would be dead code when ServeFunc owns the listener).
func (b *Binding) UseMiddleware(mw ...MiddlewareFunc) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: UseMiddleware is mutually exclusive with ServeFunc", b.name))
	}
	b.mws = append(b.mws, mw...)
}

// AddRPC appends one or more RPCHandlers to this binding's RPC chain. The
// chain is consulted in registration order; the first handler whose
// ServeHTTP returns true short-circuits the request.
//
// Calling AddRPC multiple times is allowed — the chain grows in call order.
// Panics if ServeFunc has already been set on this binding.
func (b *Binding) AddRPC(handlers ...RPCHandler) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: AddRPC is mutually exclusive with ServeFunc", b.name))
	}
	b.rpcHandlers = append(b.rpcHandlers, handlers...)
}

// SetMux installs the fallback http.Handler the binding delegates to when
// every RPCHandler returns false. Only one mux per binding.
//
// Panics if a mux has already been set on this binding (duplicate fallback
// is a programmer error) or if ServeFunc has already been set.
func (b *Binding) SetMux(h http.Handler) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetMux is mutually exclusive with ServeFunc", b.name))
	}
	if b.mux != nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetMux already called (duplicate fallback mux)", b.name))
	}
	b.mux = h
}

// SetErrorHandler registers h as the response for a terminal error scaffold
// emits itself: ErrorStatus404 (no RPCHandler claimed the request and the mux
// had no matching route) or ErrorStatus500 (a panic recovered by
// PanicRecovery). h replaces scaffold's built-in JSON default for that status.
//
// Panics if status is not a known ErrorStatus, if h is nil, if a handler has
// already been registered for status (duplicate registration is a programmer
// error), or if ServeFunc has already been set.
func (b *Binding) SetErrorHandler(status ErrorStatus, h http.Handler) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetErrorHandler is mutually exclusive with ServeFunc", b.name))
	}
	if h == nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetErrorHandler called with nil handler", b.name))
	}
	switch status {
	case ErrorStatus404, ErrorStatus500:
	default:
		panic(fmt.Sprintf("scaffold: binding %q: SetErrorHandler unknown ErrorStatus %d", b.name, int(status)))
	}
	if b.errHandlers == nil {
		b.errHandlers = make(map[ErrorStatus]http.Handler)
	}
	if _, dup := b.errHandlers[status]; dup {
		panic(fmt.Sprintf("scaffold: binding %q: SetErrorHandler already called for ErrorStatus %d", b.name, int(status)))
	}
	b.errHandlers[status] = h
}

// SetTLS installs a *tls.Config on the binding. The binding's serve loop
// will call srv.ServeTLS once the listener is open. Scaffold does not
// pre-validate the config; misconfiguration surfaces at handshake time.
//
// Panics if ServeFunc has already been set on this binding.
func (b *Binding) SetTLS(cfg *tls.Config) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetTLS is mutually exclusive with ServeFunc", b.name))
	}
	b.tls = cfg
}

// SetTimeouts installs read, write, and idle timeouts on the underlying
// http.Server constructed for this binding. A zero value for a given
// duration leaves the corresponding http.Server field at its Go default.
//
// Panics if ServeFunc has already been set on this binding.
func (b *Binding) SetTimeouts(read, write, idle time.Duration) {
	if b.serveFunc != nil {
		panic(fmt.Sprintf("scaffold: binding %q: SetTimeouts is mutually exclusive with ServeFunc", b.name))
	}
	b.readTO = read
	b.writeTO = write
	b.idleTO = idle
}

// ServeFunc registers a caller-owned serve function that takes ownership of
// the listener scaffold opens for this binding. When set, scaffold
// bypasses http.Server construction entirely; the caller is responsible for
// closing the listener on shutdown (typically by registering a Cleaner).
//
// Panics if any HTTP-pipeline state is already configured on this binding
// (middleware, RPC handlers, a mux, a TLS config, or any non-zero timeout).
func (b *Binding) ServeFunc(fn func(net.Listener) error) {
	if len(b.mws) > 0 {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with UseMiddleware", b.name))
	}
	if len(b.rpcHandlers) > 0 {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with AddRPC", b.name))
	}
	if b.mux != nil {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with SetMux", b.name))
	}
	if len(b.errHandlers) > 0 {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with SetErrorHandler", b.name))
	}
	if b.tls != nil {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with SetTLS", b.name))
	}
	if b.readTO != 0 || b.writeTO != 0 || b.idleTO != 0 {
		panic(fmt.Sprintf("scaffold: binding %q: ServeFunc is mutually exclusive with SetTimeouts", b.name))
	}
	b.serveFunc = fn
}

// Addr returns the address the binding is listening on. Returns nil if the
// binding has not yet been opened (i.e., before scaffold calls openBindings
// after OnStart returns). This is safe to call from AddOnListenReady
// callbacks, where all bindings are guaranteed to be open.
func (b *Binding) Addr() net.Addr {
	return b.addr
}

// buildHandler composes the final http.Handler for this binding: the RPC
// chain runs in registration order, falls through to the mux (or the
// ErrorStatus404 handler) when no handler short-circuits, and is wrapped in
// middleware so the first-registered middleware is outermost.
//
// When a mux is set, an unmatched request must still yield the ErrorStatus404
// reply rather than the mux's own default (net/http.ServeMux answers with
// plain-text "404 page not found"). For a *http.ServeMux the binding detects a
// non-match via Handler, whose pattern is empty when no route applies; other
// http.Handler muxes cannot be introspected and own their fallback response.
func (b *Binding) buildHandler() http.Handler {
	rpcHandlers := b.rpcHandlers
	mux := b.mux
	notFound := b.resolveErrorHandler(ErrorStatus404)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range rpcHandlers {
			if h.ServeHTTP(w, r) {
				return
			}
		}
		if mux != nil {
			if sm, ok := mux.(*http.ServeMux); ok {
				if _, pattern := sm.Handler(r); pattern == "" {
					notFound.ServeHTTP(w, r)
					return
				}
			}
			mux.ServeHTTP(w, r)
			return
		}
		notFound.ServeHTTP(w, r)
	})

	var handler http.Handler = inner
	for i := len(b.mws) - 1; i >= 0; i-- {
		handler = b.mws[i](handler)
	}
	// Expose the registered error handlers to middleware (notably
	// PanicRecovery, which resolves ErrorStatus500 when a panic unwinds). Set
	// at the outermost layer so the map is visible no matter where the panic
	// originates in the chain.
	if len(b.errHandlers) > 0 {
		errHandlers := b.errHandlers
		next := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withErrorHandlers(r.Context(), errHandlers)))
		})
	}
	return handler
}

// resolveErrorHandler returns the handler registered for status, or scaffold's
// built-in default when none is registered.
func (b *Binding) resolveErrorHandler(status ErrorStatus) http.Handler {
	if h, ok := b.errHandlers[status]; ok {
		return h
	}
	return defaultErrorHandler(status)
}
