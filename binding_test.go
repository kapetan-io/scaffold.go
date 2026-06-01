package scaffold_test

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBinding returns a freshly added *Binding from a zero-value
// DefaultBindings, which is sufficient for exercising setter behavior.
func newBinding(t *testing.T, name string) *scaffold.Binding {
	t.Helper()
	var b scaffold.DefaultBindings
	return b.Add(name, 0)
}

// noopRPC is an RPCHandler that does nothing and returns false; sufficient
// for exercising mutual-exclusion rules without invoking the dispatcher.
type noopRPC struct{}

func (noopRPC) ServeHTTP(http.ResponseWriter, *http.Request) bool { return false }

func TestBindingDuplicateSetMuxPanics(t *testing.T) {
	b := newBinding(t, "api")
	b.SetMux(http.NewServeMux())
	require.PanicsWithValue(t,
		`scaffold: binding "api": SetMux already called (duplicate fallback mux)`,
		func() { b.SetMux(http.NewServeMux()) })
}

func TestBindingServeFuncPanicsWhenMiddlewarePresent(t *testing.T) {
	b := newBinding(t, "api")
	b.UseMiddleware(func(next http.Handler) http.Handler { return next })
	require.PanicsWithValue(t,
		`scaffold: binding "api": ServeFunc is mutually exclusive with UseMiddleware`,
		func() { b.ServeFunc(func(net.Listener) error { return nil }) })
}

func TestBindingServeFuncPanicsWhenRPCHandlerPresent(t *testing.T) {
	b := newBinding(t, "api")
	b.AddRPC(noopRPC{})
	require.PanicsWithValue(t,
		`scaffold: binding "api": ServeFunc is mutually exclusive with AddRPC`,
		func() { b.ServeFunc(func(net.Listener) error { return nil }) })
}

func TestBindingServeFuncPanicsWhenMuxPresent(t *testing.T) {
	b := newBinding(t, "api")
	b.SetMux(http.NewServeMux())
	require.PanicsWithValue(t,
		`scaffold: binding "api": ServeFunc is mutually exclusive with SetMux`,
		func() { b.ServeFunc(func(net.Listener) error { return nil }) })
}

func TestBindingServeFuncPanicsWhenTLSPresent(t *testing.T) {
	b := newBinding(t, "api")
	b.SetTLS(&tls.Config{MinVersion: tls.VersionTLS13})
	require.PanicsWithValue(t,
		`scaffold: binding "api": ServeFunc is mutually exclusive with SetTLS`,
		func() { b.ServeFunc(func(net.Listener) error { return nil }) })
}

func TestBindingServeFuncPanicsWhenTimeoutsNonZero(t *testing.T) {
	for _, test := range []struct {
		name  string
		read  time.Duration
		write time.Duration
		idle  time.Duration
	}{
		{name: "read", read: time.Second},
		{name: "write", write: time.Second},
		{name: "idle", idle: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := newBinding(t, "api-"+test.name)
			b.SetTimeouts(test.read, test.write, test.idle)
			require.PanicsWithValue(t,
				`scaffold: binding "api-`+test.name+`": ServeFunc is mutually exclusive with SetTimeouts`,
				func() { b.ServeFunc(func(net.Listener) error { return nil }) })
		})
	}
}

func TestBindingUseMiddlewarePanicsAfterServeFunc(t *testing.T) {
	b := newBinding(t, "api")
	b.ServeFunc(func(net.Listener) error { return nil })
	require.PanicsWithValue(t,
		`scaffold: binding "api": UseMiddleware is mutually exclusive with ServeFunc`,
		func() { b.UseMiddleware(func(next http.Handler) http.Handler { return next }) })
}

func TestBindingAddRPCPanicsAfterServeFunc(t *testing.T) {
	b := newBinding(t, "api")
	b.ServeFunc(func(net.Listener) error { return nil })
	require.PanicsWithValue(t,
		`scaffold: binding "api": AddRPC is mutually exclusive with ServeFunc`,
		func() { b.AddRPC(noopRPC{}) })
}

func TestBindingSetMuxPanicsAfterServeFunc(t *testing.T) {
	b := newBinding(t, "api")
	b.ServeFunc(func(net.Listener) error { return nil })
	require.PanicsWithValue(t,
		`scaffold: binding "api": SetMux is mutually exclusive with ServeFunc`,
		func() { b.SetMux(http.NewServeMux()) })
}

func TestBindingSetTLSPanicsAfterServeFunc(t *testing.T) {
	b := newBinding(t, "api")
	b.ServeFunc(func(net.Listener) error { return nil })
	require.PanicsWithValue(t,
		`scaffold: binding "api": SetTLS is mutually exclusive with ServeFunc`,
		func() { b.SetTLS(&tls.Config{MinVersion: tls.VersionTLS13}) })
}

func TestBindingSetTimeoutsPanicsAfterServeFunc(t *testing.T) {
	b := newBinding(t, "api")
	b.ServeFunc(func(net.Listener) error { return nil })
	require.PanicsWithValue(t,
		`scaffold: binding "api": SetTimeouts is mutually exclusive with ServeFunc`,
		func() { b.SetTimeouts(time.Second, time.Second, time.Second) })
}

func TestBindingDuplicateAddRPCAllowed(t *testing.T) {
	b := newBinding(t, "api")
	assert.NotPanics(t, func() {
		b.AddRPC(noopRPC{})
		b.AddRPC(noopRPC{})
		b.AddRPC(noopRPC{}, noopRPC{})
	})
}
