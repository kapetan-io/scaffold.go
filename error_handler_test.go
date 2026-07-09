package scaffold_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartUnmatchedNoMuxRepliesWith404JSON exercises the terminal default
// when no mux is set: an unmatched request must receive a DUH-shaped JSON
// Reply rather than an empty-body 404.
func TestStartUnmatchedNoMuxRepliesWith404JSON(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			// A binding with an RPC handler that never claims and no mux.
			sc.Bindings.Add("api", 0).AddRPC(noopRPC{})
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/nope")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	body, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"code":"404","message":"no route matches GET /nope"}`, string(body))
}

// TestStartUnmatchedWithMuxRepliesWith404JSON covers the common case: a mux is
// set (as duh-generated daemons do for /readyz), and an unmatched path must
// still yield the DUH-shaped JSON Reply instead of ServeMux's plain-text
// "404 page not found".
func TestStartUnmatchedWithMuxRepliesWith404JSON(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	// A path the mux does not know about falls through to the terminal reply.
	resp, err := httpGetViaDaemon(t, inst, "api", "/unknown")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	body, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"code":"404","message":"no route matches GET /unknown"}`, string(body))
}

// TestStartMuxMatchedRouteStillServed guards the intercept: a path the mux does
// match must be served by the mux, not replaced by the terminal 404 reply.
func TestStartMuxMatchedRouteStillServed(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			})
			sc.Bindings.Add("api", 0).SetMux(mux)
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ok", string(body))
}

// TestStartRegisteredNotFoundHandlerOverridesDefault verifies a caller-supplied
// ErrorStatus404 handler replaces scaffold's built-in default.
func TestStartRegisteredNotFoundHandlerOverridesDefault(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			b := sc.Bindings.Add("api", 0)
			b.SetMux(http.NewServeMux())
			b.SetErrorHandler(scaffold.ErrorStatus404, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"404","message":"did you mean /readyz?"}`))
			}))
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/unknown")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"code":"404","message":"did you mean /readyz?"}`, string(body))
}

// TestStartRegisteredInternalHandlerOverridesDefault verifies a caller-supplied
// ErrorStatus500 handler replaces PanicRecovery's built-in default when a
// downstream handler panics.
func TestStartRegisteredInternalHandlerOverridesDefault(t *testing.T) {
	opts, _ := newTestOptions()
	f := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/boom", func(_ http.ResponseWriter, _ *http.Request) {
				panic("kaboom")
			})
			b := sc.Bindings.Add("api", 0)
			b.UseMiddleware(scaffold.PanicRecovery(discardLogger()))
			b.SetMux(mux)
			b.SetErrorHandler(scaffold.ErrorStatus500, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":"500","message":"our fault, retry later"}`))
			}))
			return nil
		},
	}
	inst, err := scaffold.Start(context.Background(), asDaemon(f), &opts)
	require.NoError(t, err)
	defer func() { _ = inst.Stop(context.Background()) }()

	resp, err := httpGetViaDaemon(t, inst, "api", "/boom")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"code":"500","message":"our fault, retry later"}`, string(body))
}

func TestBindingSetErrorHandlerPanics(t *testing.T) {
	okHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, test := range []struct {
		name    string
		setup   func(b *scaffold.Binding)
		wantMsg string
	}{
		{
			name:    "nil handler",
			setup:   func(b *scaffold.Binding) { b.SetErrorHandler(scaffold.ErrorStatus404, nil) },
			wantMsg: `scaffold: binding "api": SetErrorHandler called with nil handler`,
		},
		{
			name:    "unknown status",
			setup:   func(b *scaffold.Binding) { b.SetErrorHandler(scaffold.ErrorStatus(200), okHandler) },
			wantMsg: `scaffold: binding "api": SetErrorHandler unknown ErrorStatus 200`,
		},
		{
			name: "duplicate status",
			setup: func(b *scaffold.Binding) {
				b.SetErrorHandler(scaffold.ErrorStatus404, okHandler)
				b.SetErrorHandler(scaffold.ErrorStatus404, okHandler)
			},
			wantMsg: `scaffold: binding "api": SetErrorHandler already called for ErrorStatus 404`,
		},
		{
			name: "after ServeFunc",
			setup: func(b *scaffold.Binding) {
				b.ServeFunc(func(net.Listener) error { return nil })
				b.SetErrorHandler(scaffold.ErrorStatus404, okHandler)
			},
			wantMsg: `scaffold: binding "api": SetErrorHandler is mutually exclusive with ServeFunc`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := newBinding(t, "api")
			require.PanicsWithValue(t, test.wantMsg, func() { test.setup(b) })
		})
	}
}

func TestBindingServeFuncPanicsWhenErrorHandlerPresent(t *testing.T) {
	b := newBinding(t, "api")
	b.SetErrorHandler(scaffold.ErrorStatus404, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	require.PanicsWithValue(t,
		`scaffold: binding "api": ServeFunc is mutually exclusive with SetErrorHandler`,
		func() { b.ServeFunc(func(net.Listener) error { return nil }) })
}
