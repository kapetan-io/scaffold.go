// Package scaffold is a Go framework for building HTTP API services.
//
// Scaffold owns the daemon lifecycle — signal handling, context enrichment,
// graceful shutdown, binding orchestration, config and secrets providers,
// health and readiness handlers, TLS integration, and panic recovery — so
// that service authors can focus on the business logic they implement
// inside OnStart and OnStop hooks.
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Exit codes returned by Serve.
const (
	ExitSuccess = 0
	ExitFailure = 1
)

// Pinned values for the "reason" field of the "shutdown initiated" log
// record. The field values are an operator-facing contract (alert rules,
// log queries); keep them stable.
const (
	reasonInstanceStop = "instance_stop"
	reasonSignal       = "signal"
	reasonContext      = "context"
)

// Daemon is the user-implemented contract scaffold drives. OnStart runs
// once before any binding listener is opened; OnStop runs once during the
// shutdown sequence after all bindings have been stopped and before the
// Cleaner executes its stack. AddBeforeShutdown callbacks registered during
// OnStart run after shutdown is triggered but before any binding listener
// begins closing.
type Daemon interface {
	OnStart(ctx context.Context, sc *DaemonConfig) error
	OnStop(ctx context.Context) error
}

// DaemonConfig collects the framework services scaffold hands to the user
// daemon during OnStart. These are the same values scaffold enriches onto
// the lifecycle context via GetConfig / GetSecrets / GetLogger.
type DaemonConfig struct {
	Config   ConfigProvider
	Secrets  ConfigProvider
	Log      *slog.Logger
	Cleaner  *Cleaner
	Bindings Bindings

	onListenReady  []func(context.Context)
	beforeShutdown []func(context.Context)
}

// AddOnListenReady registers fn to be called after all bindings are open and
// dialable, before the "daemon ready" log. Callbacks are invoked in
// registration order. Calling AddOnListenReady from within an onListenReady
// callback is safe — the new callback will be called in the same pass.
func (sc *DaemonConfig) AddOnListenReady(fn func(context.Context)) {
	sc.onListenReady = append(sc.onListenReady, fn)
}

// AddBeforeShutdown registers fn to be called after shutdown is triggered but
// before any binding listener begins closing. Callbacks are invoked in
// registration order and only on normal shutdown paths (not OnStart errors or
// bind failures).
func (sc *DaemonConfig) AddBeforeShutdown(fn func(context.Context)) {
	sc.beforeShutdown = append(sc.beforeShutdown, fn)
}

// Options configures a call to Start or Serve. All fields are optional;
// zero values resolve to the documented defaults (see Start / Serve).
type Options struct {
	Bindings        Bindings
	ConfigProvider  ConfigProvider
	SecretsProvider ConfigProvider
	Log             *slog.Logger
	OnStartTimeout  time.Duration
	OnStopTimeout   time.Duration
}

// Instance is the handle returned by Start. It exposes the listener
// address for each named binding and a Stop method that triggers an
// idempotent shutdown.
type Instance struct {
	daemon        Daemon
	sc            *DaemonConfig
	bindingsOrder []*Binding
	shutdownFlag  *atomic.Bool
	once          sync.Once
}

// Addr returns the net.Addr the named binding is listening on. Panics if
// no binding with that name exists.
func (i *Instance) Addr(name string) net.Addr {
	b := i.sc.Bindings.Get(name)
	if b == nil {
		panic(fmt.Sprintf("scaffold: Instance.Addr: unknown binding %q", name))
	}
	return b.addr
}

// Stop runs the shutdown sequence exactly once. The first call returns a
// non-nil error joining any shutdown-path failures (OnStop errors,
// http.Server.Shutdown errors, Cleaner.Clean errors). Subsequent calls
// return nil without re-running any step. Bindings are torn down in
// reverse Add order, then daemon.OnStop and Cleaner.Clean run. Errors in
// any step are logged and execution continues; panics in OnStop and
// srv.Shutdown are recovered so Cleaner.Clean always runs.
func (i *Instance) Stop(ctx context.Context) error {
	var err error
	i.once.Do(func() {
		i.shutdownFlag.Store(true)
		enriched := enrichCtx(ctx, i.sc.Config, i.sc.Secrets, i.sc.Log)
		i.sc.Log.Info("shutdown initiated", "reason", reasonInstanceStop)
		err = runShutdown(enriched, i.sc.Log, &i.sc.beforeShutdown, i.bindingsOrder, i.daemon, i.sc.Cleaner)
		i.sc.Log.Info("daemon stopped")
	})
	return err
}

// enrichCtx attaches the config, secrets, and logger to ctx for retrieval
// via the GetConfig / GetSecrets / GetLogger accessors.
func enrichCtx(ctx context.Context, cfg, sec ConfigProvider, log *slog.Logger) context.Context {
	ctx = withConfig(ctx, cfg)
	ctx = withSecrets(ctx, sec)
	ctx = withLogger(ctx, log)
	return ctx
}

// runShutdown executes the beforeShutdown callbacks (when non-nil), then the
// reverse-order binding teardown, OnStop, and Cleaner.Clean steps using the
// enriched ctx. Each step's failures are logged and do not abort subsequent
// steps. Returns the joined error of every shutdown-path failure so callers
// that need to surface them (such as Instance.Stop) can do so; recovered
// panics are logged but not returned. beforeShutdown is nil on error paths
// where bindings were never fully opened.
func runShutdown(ctx context.Context, log *slog.Logger, beforeShutdown *[]func(context.Context), order []*Binding, d Daemon, c *Cleaner) error {
	runCallbacks(ctx, log, "before_shutdown", beforeShutdown)
	var errs []error
	for i := len(order) - 1; i >= 0; i-- {
		if err := shutdownBinding(ctx, log, order[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := callOnStop(ctx, log, d); err != nil {
		errs = append(errs, err)
	}
	if err := c.Clean(ctx); err != nil {
		log.Error("cleaner returned error", "error", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// shutdownBinding calls srv.Shutdown for HTTP bindings; ServeFunc
// bindings are untouched (the user's Cleaner is responsible for stopping
// the foreign serve loop). Panics and errors are recovered and logged;
// the srv.Shutdown error (if any) is returned for collection by
// runShutdown. Recovered panics do not produce a returned error.
func shutdownBinding(ctx context.Context, log *slog.Logger, b *Binding) (err error) {
	if b.srv == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("binding shutdown panicked",
				"binding", b.name,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err = b.srv.Shutdown(ctx); err != nil {
		log.Error("binding shutdown returned error",
			"binding", b.name,
			"error", err)
	}
	return err
}

// runCallbacks invokes each callback in *fns in index order with per-callback
// panic recovery. Using an index-based loop (rather than range) ensures that
// callbacks registered mid-iteration via AddOnListenReady are visible and
// fired in the same pass. On panic, logs at error level with the phase name,
// "panic", and "stack" fields, then continues. fns may be nil or point to an
// empty slice (no-op).
func runCallbacks(ctx context.Context, log *slog.Logger, phase string, fns *[]func(context.Context)) {
	if fns == nil {
		return
	}
	for i := 0; i < len(*fns); i++ {
		fn := (*fns)[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error(phase+" callback panicked",
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()))
				}
			}()
			fn(ctx)
		}()
	}
}

// callOnStop invokes d.OnStop with panic recovery. The shutdown sequence
// continues regardless of panic or error. Returns the OnStop error (if
// any) for collection by runShutdown. Recovered panics do not produce a
// returned error.
func callOnStop(ctx context.Context, log *slog.Logger, d Daemon) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("OnStop panicked",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err = d.OnStop(ctx); err != nil {
		log.Error("OnStop returned error", "error", err)
	}
	return err
}

// Start runs a daemon in test mode. It resolves Options defaults, invokes
// OnStart, opens every registered binding, and returns an *Instance whose
// Stop method drives the shutdown sequence.
//
// Unlike Serve, Start does not register signal handlers and does not use
// OnStartTimeout / OnStopTimeout. The caller's ctx is passed directly to
// OnStart; the ctx passed to Instance.Stop is used directly for the
// shutdown sequence. A nil *Options is treated as &Options{}.
func Start(ctx context.Context, d Daemon, opts *Options) (*Instance, error) {
	if opts == nil {
		opts = &Options{}
	}
	log := resolveLog(opts.Log)
	bindings := resolveBindings(opts.Bindings, false)
	cfgProv := resolveConfig(opts.ConfigProvider, log)
	secProv := resolveConfig(opts.SecretsProvider, log)
	cleaner := NewCleaner(log)
	flag := &atomic.Bool{}

	sc := &DaemonConfig{
		Config:   cfgProv,
		Secrets:  secProv,
		Log:      log,
		Cleaner:  cleaner,
		Bindings: bindings,
	}

	log.Info("daemon starting")
	startCtx := enrichCtx(ctx, cfgProv, secProv, log)
	if err := d.OnStart(startCtx, sc); err != nil {
		log.Error("OnStart returned error", "error", err)
		flag.Store(true)
		_ = runShutdown(startCtx, log, nil, nil, d, cleaner)
		return nil, err
	}

	order, err := orderedFromBindings(bindings)
	if err != nil {
		flag.Store(true)
		_ = runShutdown(startCtx, log, nil, nil, d, cleaner)
		return nil, err
	}

	opened, openErr := openBindings(startCtx, log, order, flag)
	if openErr != nil {
		flag.Store(true)
		_ = runShutdown(startCtx, log, nil, opened, d, cleaner)
		return nil, openErr
	}

	runCallbacks(startCtx, log, "on_listen_ready", &sc.onListenReady)

	log.Info("daemon ready")
	return &Instance{
		daemon:        d,
		sc:            sc,
		bindingsOrder: order,
		shutdownFlag:  flag,
	}, nil
}

// Serve runs a daemon in production mode. It resolves Options defaults,
// invokes OnStart within an optional OnStartTimeout, opens every
// registered binding, registers SIGTERM/SIGINT handlers, and blocks until
// either a signal fires or the caller's ctx is cancelled. On either
// event, Serve runs the shutdown sequence using a shutdown context
// derived from Background with the optional OnStopTimeout.
//
// Returns ExitSuccess on clean exits, ExitFailure on OnStart or bind
// failures. args is reserved for future sub-command expansion and is
// ignored in v1.
func Serve(ctx context.Context, args []string, d Daemon, opts Options) int {
	_ = args
	log := resolveLog(opts.Log)
	bindings := resolveBindings(opts.Bindings, true)
	cfgProv := resolveConfig(opts.ConfigProvider, log)
	secProv := resolveConfig(opts.SecretsProvider, log)
	cleaner := NewCleaner(log)
	flag := &atomic.Bool{}

	sc := &DaemonConfig{
		Config:   cfgProv,
		Secrets:  secProv,
		Log:      log,
		Cleaner:  cleaner,
		Bindings: bindings,
	}

	log.Info("daemon starting")

	startCtx := ctx
	var cancelStart context.CancelFunc
	if opts.OnStartTimeout > 0 {
		startCtx, cancelStart = context.WithTimeout(ctx, opts.OnStartTimeout)
	}
	enrichedStart := enrichCtx(startCtx, cfgProv, secProv, log)
	if err := d.OnStart(enrichedStart, sc); err != nil {
		if cancelStart != nil {
			cancelStart()
		}
		log.Error("OnStart returned error", "error", err)
		flag.Store(true)
		shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
		defer cancel()
		enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
		_ = runShutdown(enriched, log, nil, nil, d, cleaner)
		return ExitFailure
	}
	order, err := orderedFromBindings(bindings)
	if err != nil {
		if cancelStart != nil {
			cancelStart()
		}
		log.Error("Bindings iteration failed", "error", err)
		flag.Store(true)
		shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
		defer cancel()
		enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
		_ = runShutdown(enriched, log, nil, nil, d, cleaner)
		return ExitFailure
	}

	opened, openErr := openBindings(enrichedStart, log, order, flag)
	if openErr != nil {
		if cancelStart != nil {
			cancelStart()
		}
		flag.Store(true)
		shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
		defer cancel()
		enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
		_ = runShutdown(enriched, log, nil, opened, d, cleaner)
		return ExitFailure
	}

	runCallbacks(enrichedStart, log, "on_listen_ready", &sc.onListenReady)
	if cancelStart != nil {
		cancelStart()
	}

	log.Info("daemon ready")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		log.Info("shutdown initiated",
			"reason", reasonSignal,
			"signal", sig.String())
	case <-ctx.Done():
		log.Info("shutdown initiated", "reason", reasonContext)
	}

	flag.Store(true)
	shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
	defer cancel()
	enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
	_ = runShutdown(enriched, log, &sc.beforeShutdown, opened, d, cleaner)
	log.Info("daemon stopped")
	return ExitSuccess
}

// buildShutdownCtx returns a context derived from Background with the
// given timeout, or Background itself when timeout is zero. Derived from
// Background so caller-cancelled contexts do not shorten the shutdown
// budget.
func buildShutdownCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.Background(), func() {}
}

// resolveLog returns log or the default color logger when log is nil.
func resolveLog(log *slog.Logger) *slog.Logger {
	if log != nil {
		return log
	}
	return defaultLogger()
}

// resolveBindings returns b or a default Bindings implementation. Serve
// defaults to DefaultBindings; Start defaults to TestBindings.
func resolveBindings(b Bindings, production bool) Bindings {
	if b != nil {
		return b
	}
	if production {
		return &DefaultBindings{}
	}
	return &TestBindings{}
}

// resolveConfig returns p or an EnvConfigProvider bound to log.
func resolveConfig(p ConfigProvider, log *slog.Logger) ConfigProvider {
	if p != nil {
		return p
	}
	return &EnvConfigProvider{Logger: log}
}

// orderedFromBindings extracts the binding order from b. The lifecycle
// needs Add-order iteration to open bindings deterministically and tear
// them down in reverse; only DefaultBindings and TestBindings (which
// embed bindingStore) provide the unexported orderedBindings() method.
// Third-party Bindings implementations are not a supported v1 extension.
func orderedFromBindings(b Bindings) ([]*Binding, error) {
	src, ok := b.(interface{ orderedBindings() []*Binding })
	if !ok {
		return nil, errors.New("scaffold: Bindings implementation does not support ordered iteration; only DefaultBindings and TestBindings are supported in v1")
	}
	return src.orderedBindings(), nil
}

// openBindings opens a listener for each binding in order, spawns the
// serve-loop goroutine, and waits for each listener to become dialable.
// On any failure, returns the set of already-opened bindings (for
// reverse-order teardown) and the failure error.
func openBindings(
	ctx context.Context,
	log *slog.Logger,
	order []*Binding,
	flag *atomic.Bool,
) ([]*Binding, error) {
	opened := make([]*Binding, 0, len(order))
	for _, b := range order {
		if b.serveFunc == nil {
			b.finalHandler = b.buildHandler()
		}
		ln, err := net.Listen("tcp", b.listenAddr)
		if err != nil {
			log.Error("binding listen failed",
				"name", b.name,
				"error", err)
			return opened, err
		}
		b.addr = ln.Addr()
		if b.serveFunc == nil {
			b.srv = newHTTPServer(b)
		}
		go runBindingServeLoop(log, b, ln, flag)
		log.Info("binding listening",
			"name", b.name,
			"addr", b.addr.String())
		if err := waitForListener(ctx, b.addr, 5*time.Second); err != nil {
			log.Error("binding readiness probe failed",
				"name", b.name,
				"error", err)
			return opened, err
		}
		opened = append(opened, b)
	}
	return opened, nil
}

// runBindingServeLoop invokes the binding's serve function (HTTP or
// user-supplied ServeFunc) and classifies the return value using the
// shutdown-requested flag. Returns that arrive after shutdown is
// requested are expected; other returns are logged at error level. A
// single binding's goroutine exiting never triggers daemon teardown.
func runBindingServeLoop(log *slog.Logger, b *Binding, ln net.Listener, flag *atomic.Bool) {
	var err error
	switch {
	case b.serveFunc != nil:
		err = b.serveFunc(ln)
	case b.tls != nil:
		err = b.srv.ServeTLS(ln, "", "")
	default:
		err = b.srv.Serve(ln)
	}
	if flag.Load() {
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Debug("binding serve returned",
				"binding", b.name,
				"error", err)
		}
		return
	}
	if err != nil {
		log.Error("binding serve returned unexpectedly",
			"binding", b.name,
			"error", err)
		return
	}
	log.Error("binding serve returned nil unexpectedly",
		"binding", b.name)
}
