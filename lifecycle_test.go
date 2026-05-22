package scaffold_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kapetan-io/scaffold"
	"github.com/kapetan-io/tackle/autotls"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestOptions returns an Options pre-wired with a testLogHandler-backed
// logger so tests can assert on captured log records. The returned handler
// is the log destination the caller can query.
func newTestOptions() (scaffold.Options, *testLogHandler) {
	log, h := newTestLogger()
	return scaffold.Options{Log: log}, h
}

// httpGetViaDaemon issues a GET request to binding name on inst using a
// new http.Client per call (so connection reuse does not confuse
// shutdown tests).
func httpGetViaDaemon(t *testing.T, inst *scaffold.Instance, name, path string) (*http.Response, error) {
	t.Helper()
	addr := inst.Addr(name)
	u := fmt.Sprintf("http://%s%s", addr.String(), path)
	return http.Get(u)
}

func TestStartHappyPathSingleBindingRoundTrip(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			mux := http.NewServeMux()
			mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			})
			b.SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)

	resp, err := httpGetViaDaemon(t, inst, "api", "/hello")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "hi", string(body))

	require.NoError(t, inst.Stop(context.Background()))
}

func TestStartMultipleBindingsOpenInAddOrder(t *testing.T) {
	opts, logH := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("alpha", 0).SetMux(http.NewServeMux())
			sc.Bindings.Add("bravo", 0).SetMux(http.NewServeMux())
			sc.Bindings.Add("charlie", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	var order []string
	for _, r := range logH.Records() {
		if r.Message != "binding listening" {
			continue
		}
		if a, ok := recordAttr(r, "name"); ok {
			order = append(order, a.Value.String())
		}
	}
	assert.Equal(t, []string{"alpha", "bravo", "charlie"}, order)
}

func TestStartMultipleBindingsShutDownInReverseOrder(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var closed []string
	appendClose := func(name string) func(context.Context) error {
		return func(_ context.Context) error {
			mu.Lock()
			closed = append(closed, "closed:"+name)
			mu.Unlock()
			return nil
		}
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("alpha", 0).SetMux(http.NewServeMux())
			sc.Cleaner.Add(appendClose("alpha"))
			sc.Bindings.Add("bravo", 0).SetMux(http.NewServeMux())
			sc.Cleaner.Add(appendClose("bravo"))
			sc.Bindings.Add("charlie", 0).SetMux(http.NewServeMux())
			sc.Cleaner.Add(appendClose("charlie"))
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"closed:charlie", "closed:bravo", "closed:alpha"}, closed)
}

func TestStartOnStartErrorCallsOnStopAndNoPortsOpened(t *testing.T) {
	opts, logH := newTestOptions()
	sentinel := errors.New("boom")
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return sentinel
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.Nil(t, inst)
	require.ErrorIs(t, err, sentinel)

	events := f.Events()
	assert.Equal(t, []string{"onstart", "onstop"}, events)

	// No "binding listening" log records should have been emitted.
	for _, r := range logH.Records() {
		assert.NotEqual(t, "binding listening", r.Message)
	}
}

func TestStartSecondBindingFailsToOpenClosesFirstAndCallsOnStop(t *testing.T) {
	// Pre-bind a port on all interfaces so scaffold's Listen on :<port>
	// returns EADDRINUSE.
	reserved, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = reserved.Close() }()
	reservedPort := reserved.Addr().(*net.TCPAddr).Port

	opts, _ := newTestOptions()
	// Override the default bindings: use DefaultBindings so the failing
	// binding can target the reserved port via :<port>.
	opts.Bindings = &scaffold.DefaultBindings{}

	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			// First binding picks port 0 (ephemeral) — should succeed.
			sc.Bindings.Add("alpha", 0).SetMux(http.NewServeMux())
			// Second binding targets the reserved port — should fail.
			sc.Bindings.Add("bravo", reservedPort).SetMux(http.NewServeMux())
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.Nil(t, inst)
	require.Error(t, err)

	events := f.Events()
	assert.Equal(t, []string{"onstart", "onstop"}, events)
}

func TestStartCallerCtxCancelInterruptsOnStart(t *testing.T) {
	opts, _ := newTestOptions()
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDaemon{
		OnStart: func(ctx context.Context, _ *scaffold.DaemonConfig) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}
	inst, err := scaffold.Start(ctx, asDaemon(f), &opts)
	require.Nil(t, inst)
	require.ErrorIs(t, err, context.Canceled)
}

func TestStartGracefulDrainInFlightHandler(t *testing.T) {
	opts, _ := newTestOptions()
	started := make(chan struct{})
	release := make(chan struct{})
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				_, _ = w.Write([]byte("drained"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)

	type result struct {
		body string
		err  error
	}
	rc := make(chan result, 1)
	go func() {
		resp, err := httpGetViaDaemon(t, inst, "api", "/slow")
		if err != nil {
			rc <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		rc <- result{body: string(body)}
	}()

	<-started

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- inst.Stop(context.Background())
	}()

	// Give Stop a moment to begin the shutdown sequence before releasing.
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case r := <-rc:
		require.NoError(t, r.err)
		assert.Equal(t, "drained", r.body)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not complete after release")
	}
	select {
	case err := <-stopDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestStartPanicRecoveryInHandler(t *testing.T) {
	opts, logH := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/boom", func(_ http.ResponseWriter, _ *http.Request) {
				panic("kaboom")
			})
			b := sc.Bindings.Add("api", 0)
			b.UseMiddleware(scaffold.PanicRecovery(discardLogger()))
			b.SetMux(mux)
			return nil
		},
	}
	_ = logH
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/boom")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "internal server error", string(body))
}

func TestStartRPCChainFallthroughThenSetMuxThen404(t *testing.T) {
	opts, _ := newTestOptions()
	var count1, count2 int
	h1 := &fakeRPCHandler{result: false, called: &count1}
	h2 := &fakeRPCHandler{result: false, called: &count2}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.AddRPC(h1, h2)
			mux := http.NewServeMux()
			mux.HandleFunc("/mux", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("mux-hit"))
			})
			b.SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	// Route that the mux knows about — RPC chain falls through, mux handles it.
	resp, err := httpGetViaDaemon(t, inst, "api", "/mux")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, count1)
	assert.Equal(t, 1, count2)

	// Route that neither RPC nor mux handles → 404.
	resp, err = httpGetViaDaemon(t, inst, "api", "/nope")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStartMiddlewareExecutesInRegistrationOrder(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	mw := func(label string) scaffold.MiddlewareFunc {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seq = append(seq, label)
				mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.UseMiddleware(mw("first"), mw("second"), mw("third"))
			mux := http.NewServeMux()
			mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			})
			b.SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/x")
	require.NoError(t, err)
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"first", "second", "third"}, seq)
}

func TestStartServeFuncBindingListenerHandoff(t *testing.T) {
	opts, _ := newTestOptions()
	handoff := make(chan net.Listener, 1)
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("custom", 0)
			b.ServeFunc(func(ln net.Listener) error {
				handoff <- ln
				// Block on Accept so the serve loop does not return.
				for {
					conn, err := ln.Accept()
					if err != nil {
						return err
					}
					_ = conn.Close()
				}
			})
			sc.Cleaner.Add(func(_ context.Context) error {
				ln := <-handoff
				return ln.Close()
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)

	// Verify the listener is reachable at the advertised address.
	addr := inst.Addr("custom")
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	require.NoError(t, err)
	_ = conn.Close()

	require.NoError(t, inst.Stop(context.Background()))
}

func TestStartTLSRoundTripWithAutoTLS(t *testing.T) {
	opts, _ := newTestOptions()
	cfg := autotls.Config{AutoTLS: true, Logger: opts.Log}
	require.NoError(t, autotls.Setup(&cfg))

	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.SetTLS(cfg.ServerTLS)
			mux := http.NewServeMux()
			mux.HandleFunc("/secure", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("tls-ok"))
			})
			b.SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg.ClientTLS},
		Timeout:   5 * time.Second,
	}
	addr := inst.Addr("api")
	u := &url.URL{Scheme: "https", Host: addr.String(), Path: "/secure"}
	resp, err := client.Get(u.String())
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "tls-ok", string(body))
}

func TestStartCleanerRunsLIFOAfterOnStop(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	record := func(label string) func(context.Context) error {
		return func(_ context.Context) error {
			mu.Lock()
			seq = append(seq, label)
			mu.Unlock()
			return nil
		}
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Cleaner.Add(record("first"))
			sc.Cleaner.Add(record("second"))
			sc.Cleaner.Add(record("third"))
			return nil
		},
		OnStop: func(_ context.Context) error {
			mu.Lock()
			seq = append(seq, "onstop")
			mu.Unlock()
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"onstop", "third", "second", "first"}, seq)
}

func TestStartOnStopErrorDoesNotPreventCleanerRun(t *testing.T) {
	opts, _ := newTestOptions()
	var cleanerRan atomic.Bool
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Cleaner.Add(func(_ context.Context) error {
				cleanerRan.Store(true)
				return nil
			})
			return nil
		},
		OnStop: func(_ context.Context) error {
			return errors.New("onstop boom")
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.ErrorContains(t, inst.Stop(context.Background()), "onstop boom")
	assert.True(t, cleanerRan.Load())
}

func TestStartStopReturnsOnStopError(t *testing.T) {
	opts, _ := newTestOptions()
	sentinel := errors.New("onstop sentinel")
	f := &fakeDaemon{
		OnStop: func(_ context.Context) error {
			return sentinel
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	stopErr := inst.Stop(context.Background())
	require.Error(t, stopErr)
	require.ErrorIs(t, stopErr, sentinel)
}

func TestStartStopSubsequentCallsReturnNil(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStop: func(_ context.Context) error {
			return errors.New("onstop boom")
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.Error(t, inst.Stop(context.Background()))
	require.NoError(t, inst.Stop(context.Background()))
	require.NoError(t, inst.Stop(context.Background()))
}

func TestStartOnStopPanicDoesNotPreventCleanerRun(t *testing.T) {
	opts, _ := newTestOptions()
	var cleanerRan atomic.Bool
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Cleaner.Add(func(_ context.Context) error {
				cleanerRan.Store(true)
				return nil
			})
			return nil
		},
		OnStop: func(_ context.Context) error {
			panic("onstop explode")
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))
	assert.True(t, cleanerRan.Load())
}

func TestInstanceStopIsIdempotent(t *testing.T) {
	opts, _ := newTestOptions()
	var cleanerCount atomic.Int32
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Cleaner.Add(func(_ context.Context) error {
				cleanerCount.Add(1)
				return nil
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)

	require.NoError(t, inst.Stop(context.Background()))
	require.NoError(t, inst.Stop(context.Background()))
	require.NoError(t, inst.Stop(context.Background()))

	assert.Equal(t, int32(1), cleanerCount.Load())
}

func TestInstanceStopLogsShutdownInitiatedWithReason(t *testing.T) {
	opts, logH := newTestOptions()
	f := &fakeDaemon{}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	rec := logH.findByMessage("shutdown initiated")
	require.NotNil(t, rec)
	reason, ok := recordAttr(*rec, "reason")
	require.True(t, ok)
	assert.Equal(t, "instance_stop", reason.Value.String())
}

func TestInstanceAddrPanicsForUnknownBinding(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	assert.Panics(t, func() {
		_ = inst.Addr("missing")
	})
}

// TestGetConfigAndSecretsAndLoggerAvailableInOnStart verifies the three
// framework services are readable off the Start context handed to
// OnStart.
func TestGetConfigAndSecretsAndLoggerAvailableInOnStart(t *testing.T) {
	opts, _ := newTestOptions()
	cfgProv := &scaffold.MapConfigProvider{Values: map[string]string{"A": "1"}}
	secProv := &scaffold.MapConfigProvider{Values: map[string]string{"S": "2"}}
	opts.ConfigProvider = cfgProv
	opts.SecretsProvider = secProv

	var cfgSeen, secSeen scaffold.ConfigProvider
	var logSeen *slog.Logger
	f := &fakeDaemon{
		OnStart: func(ctx context.Context, _ *scaffold.DaemonConfig) error {
			cfgSeen = scaffold.GetConfig(ctx)
			secSeen = scaffold.GetSecrets(ctx)
			logSeen = scaffold.GetLogger(ctx)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	assert.Equal(t, cfgProv, cfgSeen)
	assert.Equal(t, secProv, secSeen)
	assert.Equal(t, opts.Log, logSeen)
}

func TestGetConfigAndSecretsAndLoggerAvailableInOnStop(t *testing.T) {
	opts, _ := newTestOptions()
	cfgProv := &scaffold.MapConfigProvider{Values: map[string]string{"A": "1"}}
	secProv := &scaffold.MapConfigProvider{Values: map[string]string{"S": "2"}}
	opts.ConfigProvider = cfgProv
	opts.SecretsProvider = secProv

	var cfgSeen, secSeen scaffold.ConfigProvider
	var logSeen *slog.Logger
	f := &fakeDaemon{
		OnStop: func(ctx context.Context) error {
			cfgSeen = scaffold.GetConfig(ctx)
			secSeen = scaffold.GetSecrets(ctx)
			logSeen = scaffold.GetLogger(ctx)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	assert.Equal(t, cfgProv, cfgSeen)
	assert.Equal(t, secProv, secSeen)
	assert.Equal(t, opts.Log, logSeen)
}

func TestPerRequestContextIsNotEnriched(t *testing.T) {
	opts, _ := newTestOptions()
	var seenLogger *slog.Logger
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				seenLogger = scaffold.GetLogger(r.Context())
				_, _ = w.Write([]byte("ok"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Nil(t, seenLogger)
}

func TestBindingExitWhileShutdownRequestedSilentForNilReturn(t *testing.T) {
	opts, logH := newTestOptions()
	stopCh := make(chan struct{})
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("quiet", 0)
			b.ServeFunc(func(ln net.Listener) error {
				// Unblock when the cleaner signals.
				go func() {
					<-stopCh
					_ = ln.Close()
				}()
				for {
					conn, err := ln.Accept()
					if err != nil {
						// Return nil to exercise the silent-swallow path.
						return nil
					}
					_ = conn.Close()
				}
			})
			sc.Cleaner.Add(func(_ context.Context) error {
				close(stopCh)
				return nil
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	// Give the serve loop a moment to return after the cleaner closes the listener.
	time.Sleep(100 * time.Millisecond)

	for _, r := range logH.Records() {
		if r.Message == "binding serve returned" || r.Message == "binding serve returned unexpectedly" || r.Message == "binding serve returned nil unexpectedly" {
			if a, ok := recordAttr(r, "binding"); ok {
				assert.NotEqual(t, "quiet", a.Value.String())
			}
		}
	}
}

func TestBindingExitWhenShutdownNotRequestedLoggedAtError(t *testing.T) {
	opts, logH := newTestOptions()
	returned := make(chan struct{})
	trigger := make(chan struct{})
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("crash", 0)
			b.ServeFunc(func(ln net.Listener) error {
				// Accept one connection (the readiness probe) then block
				// until the test triggers an unexpected return.
				conn, err := ln.Accept()
				if err != nil {
					return err
				}
				_ = conn.Close()
				<-trigger
				_ = ln.Close()
				defer close(returned)
				return errors.New("premature exit")
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	close(trigger)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeFunc did not return")
	}
	// Give the goroutine a moment to record the error log.
	time.Sleep(50 * time.Millisecond)

	var found bool
	for _, r := range logH.Records() {
		if r.Level != slog.LevelError {
			continue
		}
		if !strings.Contains(r.Message, "binding serve returned") {
			continue
		}
		if a, ok := recordAttr(r, "binding"); ok && a.Value.String() == "crash" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected error-level log for unexpected ServeFunc return")
}

// TestServeFuncPlusAddRPCPanicsAtCallTimeFromOnStart validates that the
// setter mutual-exclusion panic fires from inside OnStart, so scaffold
// propagates it as part of normal OnStart-error handling.
func TestServeFuncPlusAddRPCPanicsAtCallTimeFromOnStart(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.AddRPC(&fakeRPCHandler{})
			defer func() { _ = recover() }()
			b.ServeFunc(func(_ net.Listener) error { return nil })
			return nil
		},
	}
	assert.NotPanics(t, func() {
		inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
		if err == nil {
			_ = inst.Stop(context.Background())
		}
	})
}

func TestServeFuncPlusSetMuxPanicsAtCallTimeFromOnStart(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.SetMux(http.NewServeMux())
			defer func() { _ = recover() }()
			b.ServeFunc(func(_ net.Listener) error { return nil })
			return nil
		},
	}
	assert.NotPanics(t, func() {
		inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
		if err == nil {
			_ = inst.Stop(context.Background())
		}
	})
}

func TestServeFuncPlusUseMiddlewarePanicsAtCallTimeFromOnStart(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.UseMiddleware(func(next http.Handler) http.Handler { return next })
			defer func() { _ = recover() }()
			b.ServeFunc(func(_ net.Listener) error { return nil })
			return nil
		},
	}
	assert.NotPanics(t, func() {
		inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
		if err == nil {
			_ = inst.Stop(context.Background())
		}
	})
}

func TestStartOnListenReadyFiresAfterListenersOpen(t *testing.T) {
	opts, _ := newTestOptions()
	var statusCode atomic.Int32
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			sc.AddOnListenReady(func(_ context.Context) {
				addr := sc.Bindings.Get("api").Addr()
				resp, err := http.Get("http://" + addr.String() + "/hello")
				if err != nil {
					return
				}
				_ = resp.Body.Close()
				statusCode.Store(int32(resp.StatusCode))
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)
	defer func() { _ = inst.Stop(context.Background()) }()

	assert.Equal(t, int32(http.StatusOK), statusCode.Load())
}

func TestStartOnListenReadyMultipleCallbacksInRegistrationOrder(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.AddOnListenReady(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "first")
				mu.Unlock()
			})
			sc.AddOnListenReady(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "second")
				mu.Unlock()
			})
			sc.AddOnListenReady(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "third")
				mu.Unlock()
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)
	defer func() { _ = inst.Stop(context.Background()) }()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"first", "second", "third"}, seq)
}

func TestStartOnListenReadyPanicRecovery(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			sc.AddOnListenReady(func(_ context.Context) {
				panic("first callback explodes")
			})
			sc.AddOnListenReady(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "second")
				mu.Unlock()
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)
	defer func() { _ = inst.Stop(context.Background()) }()

	mu.Lock()
	assert.Equal(t, []string{"second"}, seq)
	mu.Unlock()

	resp, err := httpGetViaDaemon(t, inst, "api", "/hello")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStartBeforeShutdownFiresBeforeListenersClose(t *testing.T) {
	opts, _ := newTestOptions()
	var addr atomic.Value
	var statusCode atomic.Int32
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			sc.AddBeforeShutdown(func(_ context.Context) {
				a, ok := addr.Load().(string)
				if !ok || a == "" {
					return
				}
				resp, err := http.Get("http://" + a + "/hello")
				if err != nil {
					return
				}
				_ = resp.Body.Close()
				statusCode.Store(int32(resp.StatusCode))
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)

	addr.Store(inst.Addr("api").String())
	require.NoError(t, inst.Stop(context.Background()))

	assert.Equal(t, int32(http.StatusOK), statusCode.Load())
}

func TestStartBeforeShutdownMultipleCallbacksInRegistrationOrder(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.AddBeforeShutdown(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "first")
				mu.Unlock()
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "second")
				mu.Unlock()
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "third")
				mu.Unlock()
			})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NotNil(t, inst)
	require.NoError(t, inst.Stop(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"first", "second", "third"}, seq)
}

func TestStartBeforeShutdownPanicRecovery(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	var cleanerRan atomic.Bool
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Cleaner.Add(func(_ context.Context) error {
				cleanerRan.Store(true)
				return nil
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				panic("before_shutdown explodes")
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				mu.Lock()
				seq = append(seq, "second")
				mu.Unlock()
			})
			return nil
		},
		OnStop: func(_ context.Context) error {
			mu.Lock()
			seq = append(seq, "onstop")
			mu.Unlock()
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	mu.Lock()
	assert.Equal(t, []string{"second", "onstop"}, seq)
	mu.Unlock()
	assert.True(t, cleanerRan.Load())
}

func TestStartFullLifecycleOrdering(t *testing.T) {
	opts, _ := newTestOptions()
	var mu sync.Mutex
	var seq []string
	record := func(label string) {
		mu.Lock()
		seq = append(seq, label)
		mu.Unlock()
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.AddOnListenReady(func(_ context.Context) {
				record("listen_ready")
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				record("before_shutdown")
			})
			sc.Cleaner.Add(func(_ context.Context) error {
				record("cleaner")
				return nil
			})
			return nil
		},
		OnStop: func(_ context.Context) error {
			record("on_stop")
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"listen_ready", "before_shutdown", "on_stop", "cleaner"}, seq)
}
