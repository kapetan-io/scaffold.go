package scaffold_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/kapetan-io/scaffold.go"
)

// testLogHandlerCore holds the shared state for a testLogHandler; values of
// this type are always referenced via pointer so that WithAttrs / WithGroup
// clones see the same mutex and record slice.
type testLogHandlerCore struct {
	mu      sync.Mutex
	records []slog.Record
}

// testLogHandler captures slog.Records emitted through it into an in-memory
// slice so tests can assert on the structured output produced by scaffold
// components. It is safe for concurrent use.
type testLogHandler struct {
	core   *testLogHandlerCore
	attrs  []slog.Attr
	groups []string
}

func newTestLogHandler() *testLogHandler {
	return &testLogHandler{core: &testLogHandlerCore{}}
}

func (h *testLogHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *testLogHandler) Handle(_ context.Context, r slog.Record) error {
	cloned := r.Clone()
	h.core.mu.Lock()
	h.core.records = append(h.core.records, cloned)
	h.core.mu.Unlock()
	return nil
}

func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := &testLogHandler{core: h.core}
	clone.attrs = append([]slog.Attr(nil), h.attrs...)
	clone.attrs = append(clone.attrs, attrs...)
	clone.groups = append([]string(nil), h.groups...)
	return clone
}

func (h *testLogHandler) WithGroup(name string) slog.Handler {
	clone := &testLogHandler{core: h.core}
	clone.attrs = append([]slog.Attr(nil), h.attrs...)
	clone.groups = append([]string(nil), h.groups...)
	clone.groups = append(clone.groups, name)
	return clone
}

// Records returns a copy of the captured records for assertion.
func (h *testLogHandler) Records() []slog.Record {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	out := make([]slog.Record, len(h.core.records))
	copy(out, h.core.records)
	return out
}

// findByMessage returns the first captured record whose message equals msg,
// or nil if no such record exists.
func (h *testLogHandler) findByMessage(msg string) *slog.Record {
	for _, r := range h.Records() {
		if r.Message == msg {
			rec := r
			return &rec
		}
	}
	return nil
}

// newTestLogger returns a *slog.Logger wrapping a freshly-created
// testLogHandler. The handler is returned so callers can query captured
// records.
func newTestLogger() (*slog.Logger, *testLogHandler) {
	h := newTestLogHandler()
	return slog.New(h), h
}

// recordAttr extracts the attribute named key from r as a string, returning
// "" if absent.
func recordAttr(r slog.Record, key string) (slog.Attr, bool) {
	var found slog.Attr
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = a
			ok = true
			return false
		}
		return true
	})
	return found, ok
}

// discardLogger returns a *slog.Logger that discards all records, useful for
// subjects that require a non-nil logger but whose log output is not under
// test.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeDaemon is a minimal scaffold.Daemon used by lifecycle tests. OnStart
// and OnStop hooks are configurable; the events slice records the
// invocation sequence for ordering assertions. The mu mutex guards events
// so concurrent cleaner functions can record safely.
type fakeDaemon struct {
	mu      sync.Mutex
	events  []string
	OnStart func(ctx context.Context, sc *scaffold.DaemonConfig) error
	OnStop  func(ctx context.Context) error
}

func (f *fakeDaemon) onStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
	f.appendEvent("onstart")
	if f.OnStart != nil {
		return f.OnStart(ctx, sc)
	}
	return nil
}

func (f *fakeDaemon) onStop(ctx context.Context) error {
	f.appendEvent("onstop")
	if f.OnStop != nil {
		return f.OnStop(ctx)
	}
	return nil
}

func (f *fakeDaemon) appendEvent(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

// Events returns a copy of the captured event slice.
func (f *fakeDaemon) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	return out
}

// daemonAdapter wraps fakeDaemon to satisfy scaffold.Daemon using the
// unexported onStart / onStop methods so tests can share a single
// fakeDaemon type.
type daemonAdapter struct{ f *fakeDaemon }

func (d daemonAdapter) OnStart(ctx context.Context, sc *scaffold.DaemonConfig) error {
	return d.f.onStart(ctx, sc)
}

func (d daemonAdapter) OnStop(ctx context.Context) error {
	return d.f.onStop(ctx)
}

// asDaemon returns a scaffold.Daemon backed by f.
func asDaemon(f *fakeDaemon) scaffold.Daemon {
	return daemonAdapter{f: f}
}

// fakeRPCHandler is a minimal scaffold.RPCHandler used by RPC chain tests.
// result is the value ServeHTTP returns; called is incremented on each
// invocation so tests can assert ordering.
type fakeRPCHandler struct {
	result bool
	called *int
	write  func(w http.ResponseWriter)
}

func (h *fakeRPCHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) bool {
	if h.called != nil {
		*h.called++
	}
	if h.result && h.write != nil {
		h.write(w)
	}
	return h.result
}
