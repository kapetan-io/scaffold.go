// Package scaffold is a Go framework for building HTTP API services.
//
// Scaffold owns the daemon lifecycle — signal handling, context enrichment,
// graceful shutdown, binding orchestration, config and secrets providers,
// health and readiness handlers, TLS integration, and panic recovery — so
// that service authors can focus on the business logic they implement
// inside OnStart and OnStop hooks.
//
// See docs/features/mvp/prd.md for the product rationale and
// docs/features/mvp/tech-spec.md for the authoritative design decisions.
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

// Daemon is the user-implemented contract scaffold drives. OnStart runs
// once before any binding listener is opened; OnStop runs once during the
// shutdown sequence after all bindings have been stopped and before the
// Cleaner executes its stack.
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
	bindings      Bindings
	bindingsOrder []*Binding
	log           *slog.Logger
	config        ConfigProvider
	secrets       ConfigProvider
	cleaner       *Cleaner
	shutdownFlag  *atomic.Bool
	once          sync.Once
	onStopTimeout time.Duration
}

// Addr returns the net.Addr the named binding is listening on. Panics if
// no binding with that name exists.
func (i *Instance) Addr(name string) net.Addr {
	b := i.bindings.Get(name)
	if b == nil {
		panic(fmt.Sprintf("scaffold: Instance.Addr: unknown binding %q", name))
	}
	return b.addr
}

// Stop runs the shutdown sequence exactly once. Subsequent calls return
// nil without re-running any step. The shutdown sequence is:
//  1. Set the shutdown-requested flag.
//  2. Enrich ctx with config, secrets, and logger.
//  3. Log "shutdown initiated" (reason=instance_stop).
//  4. For each binding in reverse Add order, call srv.Shutdown for HTTP
//     bindings; ServeFunc bindings are left to the user's Cleaner.
//  5. Call daemon.OnStop.
//  6. Call Cleaner.Clean.
//  7. Log "daemon stopped".
//
// Errors in any step are logged and execution proceeds to the next step.
// Panics in OnStop and srv.Shutdown are recovered and logged; the
// sequence continues so Cleaner.Clean always runs.
func (i *Instance) Stop(ctx context.Context) error {
	i.once.Do(func() {
		i.shutdownFlag.Store(true)
		enriched := enrichCtx(ctx, i.config, i.secrets, i.log)
		i.log.Info("shutdown initiated", "reason", "instance_stop")
		runShutdown(enriched, i.log, i.bindingsOrder, i.daemon, i.cleaner)
		i.log.Info("daemon stopped")
	})
	return nil
}

// enrichCtx attaches the config, secrets, and logger to ctx for retrieval
// via the GetConfig / GetSecrets / GetLogger accessors.
func enrichCtx(ctx context.Context, cfg, sec ConfigProvider, log *slog.Logger) context.Context {
	ctx = withConfig(ctx, cfg)
	ctx = withSecrets(ctx, sec)
	ctx = withLogger(ctx, log)
	return ctx
}

// runShutdown executes the reverse-order binding teardown, OnStop, and
// Cleaner.Clean steps using the enriched ctx. Each step's failures are
// logged and do not abort subsequent steps.
func runShutdown(ctx context.Context, log *slog.Logger, order []*Binding, d Daemon, c *Cleaner) {
	for i := len(order) - 1; i >= 0; i-- {
		shutdownBinding(ctx, log, order[i])
	}
	callOnStop(ctx, log, d)
	if err := c.Clean(ctx); err != nil {
		log.Error("cleaner returned error", "error", err)
	}
}

// shutdownBinding calls srv.Shutdown for HTTP bindings; ServeFunc
// bindings are untouched (the user's Cleaner is responsible for stopping
// the foreign serve loop). Panics and errors are recovered and logged.
func shutdownBinding(ctx context.Context, log *slog.Logger, b *Binding) {
	if b.srv == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("binding shutdown panicked",
				"binding", b.name,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err := b.srv.Shutdown(ctx); err != nil {
		log.Error("binding shutdown returned error",
			"binding", b.name,
			"error", err)
	}
}

// callOnStop invokes d.OnStop with panic recovery. The shutdown sequence
// continues regardless of panic or error.
func callOnStop(ctx context.Context, log *slog.Logger, d Daemon) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("OnStop panicked",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err := d.OnStop(ctx); err != nil {
		log.Error("OnStop returned error", "error", err)
	}
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
		runShutdown(startCtx, log, nil, d, cleaner)
		return nil, err
	}

	order, err := orderedFromBindings(bindings)
	if err != nil {
		flag.Store(true)
		runShutdown(startCtx, log, nil, d, cleaner)
		return nil, err
	}

	opened, openErr := openBindings(startCtx, log, order, flag)
	if openErr != nil {
		flag.Store(true)
		runShutdown(startCtx, log, opened, d, cleaner)
		return nil, openErr
	}

	log.Info("daemon ready")
	return &Instance{
		daemon:        d,
		bindings:      bindings,
		bindingsOrder: order,
		log:           log,
		config:        cfgProv,
		secrets:       secProv,
		cleaner:       cleaner,
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
		runShutdown(enriched, log, nil, d, cleaner)
		return ExitFailure
	}
	if cancelStart != nil {
		cancelStart()
	}

	order, err := orderedFromBindings(bindings)
	if err != nil {
		log.Error("Bindings iteration failed", "error", err)
		flag.Store(true)
		shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
		defer cancel()
		enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
		runShutdown(enriched, log, nil, d, cleaner)
		return ExitFailure
	}

	opened, openErr := openBindings(enrichedStart, log, order, flag)
	if openErr != nil {
		flag.Store(true)
		shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
		defer cancel()
		enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
		runShutdown(enriched, log, opened, d, cleaner)
		return ExitFailure
	}

	log.Info("daemon ready")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		log.Info("shutdown initiated",
			"reason", "signal",
			"signal", sig.String())
	case <-ctx.Done():
		log.Info("shutdown initiated", "reason", "context")
	}

	flag.Store(true)
	shutdownCtx, cancel := buildShutdownCtx(opts.OnStopTimeout)
	defer cancel()
	enriched := enrichCtx(shutdownCtx, cfgProv, secProv, log)
	runShutdown(enriched, log, opened, d, cleaner)
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

// orderedFromBindings extracts the binding order from b's embedded
// bindingStore. Returns an error if b does not embed bindingStore — this
// is a known v1 limitation documented under the plan's "What We're NOT
// Doing" section; third-party Bindings implementations cannot satisfy
// the unexported orderedBindings() method.
func orderedFromBindings(b Bindings) ([]*Binding, error) {
	// See plans/scaffold-mvp-implementation-plan.md "What We're NOT Doing":
	// the lifecycle needs Add-order iteration which only the built-in
	// bindingStore provides via the unexported orderedBindings() method.
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
// requested are expected; other returns are logged at error level. The
// daemon is never torn down as a side effect of a single binding's
// goroutine exiting (ADR-0003).
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
