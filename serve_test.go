package scaffold_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
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
	defer reserved.Close()
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
