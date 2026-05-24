package scaffold_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"syscall"
	"testing"
	"time"

	"github.com/kapetan-io/scaffold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveResult captures the outcome of a Serve invocation for cross-goroutine
// assertion.
type serveResult struct {
	exit int
}

func runServeInBackground(t *testing.T, ctx context.Context, d scaffold.Daemon, opts scaffold.Options) <-chan serveResult {
	t.Helper()
	done := make(chan serveResult, 1)
	go func() {
		done <- serveResult{exit: scaffold.Serve(ctx, nil, d, opts)}
	}()
	return done
}

func TestServeCallerCtxCancelReturnsExitSuccess(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.TestBindings{}}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeInBackground(t, ctx, asDaemon(f), opts)

	// Wait for daemon ready.
	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	rec := logH.findByMessage("shutdown initiated")
	require.NotNil(t, rec)
	reason, ok := recordAttr(*rec, "reason")
	require.True(t, ok)
	assert.Equal(t, "context", reason.Value.String())
}

func TestServeSIGTERMReturnsExitSuccessAndLogsSignalReason(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.TestBindings{}}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	done := runServeInBackground(t, context.Background(), asDaemon(f), opts)
	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after SIGTERM")
	}

	rec := logH.findByMessage("shutdown initiated")
	require.NotNil(t, rec)
	reason, ok := recordAttr(*rec, "reason")
	require.True(t, ok)
	assert.Equal(t, "signal", reason.Value.String())
	sigAttr, ok := recordAttr(*rec, "signal")
	require.True(t, ok)
	assert.Equal(t, syscall.SIGTERM.String(), sigAttr.Value.String())
}

func TestServeSIGINTReturnsExitSuccessAndLogsSignalReason(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.TestBindings{}}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	done := runServeInBackground(t, context.Background(), asDaemon(f), opts)
	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after SIGINT")
	}

	rec := logH.findByMessage("shutdown initiated")
	require.NotNil(t, rec)
	reason, ok := recordAttr(*rec, "reason")
	require.True(t, ok)
	assert.Equal(t, "signal", reason.Value.String())
	sigAttr, ok := recordAttr(*rec, "signal")
	require.True(t, ok)
	assert.Equal(t, syscall.SIGINT.String(), sigAttr.Value.String())
}

func TestServeOnStartErrorReturnsExitFailure(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.TestBindings{}}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, _ *scaffold.DaemonConfig) error {
			return errors.New("onstart fail")
		},
	}
	exit := scaffold.Serve(context.Background(), nil, asDaemon(f), opts)
	assert.Equal(t, scaffold.ExitFailure, exit)

	var found bool
	for _, r := range logH.Records() {
		if r.Level == slog.LevelError && strings.Contains(r.Message, "OnStart") {
			found = true
			break
		}
	}
	assert.True(t, found)
	assert.Contains(t, f.Events(), "onstop")
}

func TestServeBindFailureReturnsExitFailure(t *testing.T) {
	reserved, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = reserved.Close() }()
	reservedPort := reserved.Addr().(*net.TCPAddr).Port

	log, _ := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.DefaultBindings{}}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", reservedPort).SetMux(http.NewServeMux())
			return nil
		},
	}
	exit := scaffold.Serve(context.Background(), nil, asDaemon(f), opts)
	assert.Equal(t, scaffold.ExitFailure, exit)
	assert.Contains(t, f.Events(), "onstop")
}

func TestServeOnStartTimeoutCancelsOnStart(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{
		Log:            log,
		Bindings:       &scaffold.TestBindings{},
		OnStartTimeout: 100 * time.Millisecond,
	}
	f := &fakeDaemon{
		OnStart: func(ctx context.Context, _ *scaffold.DaemonConfig) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	exit := scaffold.Serve(context.Background(), nil, asDaemon(f), opts)
	assert.Equal(t, scaffold.ExitFailure, exit)

	var found bool
	for _, r := range logH.Records() {
		if r.Level == slog.LevelError && strings.Contains(r.Message, "OnStart") {
			found = true
			break
		}
	}
	assert.True(t, found)
}

func TestServeOnListenReadyAndBeforeShutdownFire(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{Log: log, Bindings: &scaffold.TestBindings{}}
	var mu sync.Mutex
	var seq []string
	record := func(label string) {
		mu.Lock()
		seq = append(seq, label)
		mu.Unlock()
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			sc.AddOnListenReady(func(_ context.Context) {
				record("listen_ready")
			})
			sc.AddBeforeShutdown(func(_ context.Context) {
				record("before_shutdown")
			})
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeInBackground(t, ctx, asDaemon(f), opts)
	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"listen_ready", "before_shutdown"}, seq)
}

func TestServeOnStartTimeoutCoversOnListenReadyCallbacks(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{
		Log:            log,
		Bindings:       &scaffold.TestBindings{},
		OnStartTimeout: 500 * time.Millisecond,
	}
	var deadline atomic.Value
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			sc.AddOnListenReady(func(ctx context.Context) {
				if dl, ok := ctx.Deadline(); ok {
					deadline.Store(dl)
				}
			})
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeInBackground(t, ctx, asDaemon(f), opts)
	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	dl, ok := deadline.Load().(time.Time)
	require.True(t, ok)
	assert.False(t, dl.IsZero())
}

func TestServeInvalidBindAddressReturnsExitFailure(t *testing.T) {
	log, _ := newTestLogger()
	opts := scaffold.Options{Log: log, BindAddress: "not-an-ip"}
	f := &fakeDaemon{}

	exit := scaffold.Serve(context.Background(), nil, asDaemon(f), opts)
	assert.Equal(t, scaffold.ExitFailure, exit)
	assert.NotContains(t, f.Events(), "onstart")
}

func TestServeInvalidAdvertisedAddressReturnsExitFailure(t *testing.T) {
	log, _ := newTestLogger()
	opts := scaffold.Options{Log: log, AdvertisedAddress: "not-an-ip"}
	f := &fakeDaemon{}

	exit := scaffold.Serve(context.Background(), nil, asDaemon(f), opts)
	assert.Equal(t, scaffold.ExitFailure, exit)
	assert.NotContains(t, f.Events(), "onstart")
}

func TestServeNetworkIdentityLogLine(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{
		Log:         log,
		Bindings:    &scaffold.TestBindings{},
		BindAddress: "127.0.0.1",
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeInBackground(t, ctx, asDaemon(f), opts)

	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	rec := logH.findByMessage("network identity resolved")
	require.NotNil(t, rec)

	bindAddr, ok := recordAttr(*rec, "bind_address")
	require.True(t, ok)
	assert.Equal(t, "127.0.0.1", bindAddr.Value.String())

	bindSource, ok := recordAttr(*rec, "bind_source")
	require.True(t, ok)
	assert.Equal(t, "configured", bindSource.Value.String())

	advAddr, ok := recordAttr(*rec, "advertised_address")
	require.True(t, ok)
	assert.Equal(t, "127.0.0.1", advAddr.Value.String())

	advSource, ok := recordAttr(*rec, "advertised_source")
	require.True(t, ok)
	assert.Equal(t, "derived", advSource.Value.String())

	// Verify "network identity resolved" appears after "daemon starting" in log sequence.
	var startingIdx, identityIdx int
	for i, r := range logH.Records() {
		if r.Message == "daemon starting" {
			startingIdx = i
		}
		if r.Message == "network identity resolved" {
			identityIdx = i
		}
	}
	assert.Greater(t, identityIdx, startingIdx)
}

func TestServeLoopbackWarning(t *testing.T) {
	log, logH := newTestLogger()
	opts := scaffold.Options{
		Log:         log,
		Bindings:    &scaffold.TestBindings{},
		BindAddress: "127.0.0.1",
	}
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Bindings.Add("api", 0).SetMux(http.NewServeMux())
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeInBackground(t, ctx, asDaemon(f), opts)

	waitForMessage(t, logH, "daemon ready", 5*time.Second)
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, scaffold.ExitSuccess, r.exit)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	rec := logH.findByMessage("advertised address is loopback; peers outside this host cannot reach this service")
	require.NotNil(t, rec)

	advAddr, ok := recordAttr(*rec, "advertised_address")
	require.True(t, ok)
	assert.Equal(t, "127.0.0.1", advAddr.Value.String())
	assert.Equal(t, slog.LevelWarn, rec.Level)
}

// waitForMessage polls logH until a record with the given message is
// captured or timeout expires.
func waitForMessage(t *testing.T, logH *testLogHandler, msg string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if logH.findByMessage(msg) != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log message %q", msg)
}
