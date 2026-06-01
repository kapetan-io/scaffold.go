package sprometheus_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	sprometheus "github.com/kapetan-io/scaffold.go/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseWriterStatusCapturedFromWriteHeader(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Pattern = "/status"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, float64(1), gatherCounter(t, reg, "GET", "/status", "418"))
}

func TestResponseWriterDefaultStatusIs200(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not call WriteHeader. Default status must be 200.
	}))

	req := httptest.NewRequest(http.MethodGet, "/default", nil)
	req.Pattern = "/default"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, float64(1), gatherCounter(t, reg, "GET", "/default", "200"))
}

func TestResponseWriterForwardsFlush(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)

	rec := httptest.NewRecorder()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok)
		f.Flush()
	}))

	req := httptest.NewRequest(http.MethodGet, "/flush", nil)
	req.Pattern = "/flush"
	handler.ServeHTTP(rec, req)

	assert.True(t, rec.Flushed)
}

// hijackRecorder is an http.ResponseWriter that also implements http.Hijacker
// so tests can observe wrapper forwarding of Hijack.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn       net.Conn
	buf        *bufio.ReadWriter
	hijackErr  error
	hijackedOk bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h.hijackErr != nil {
		return nil, nil, h.hijackErr
	}
	h.hijackedOk = true
	return h.conn, h.buf, nil
}

func TestResponseWriterForwardsHijack(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	inner := &hijackRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             server,
		buf:              bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)),
	}

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, rw, err := h.Hijack()
		require.NoError(t, err)
		assert.Same(t, server, conn)
		assert.Same(t, inner.buf, rw)
	}))

	req := httptest.NewRequest(http.MethodGet, "/hijack", nil)
	req.Pattern = "/hijack"
	handler.ServeHTTP(inner, req)

	assert.True(t, inner.hijackedOk)
}

func TestResponseWriterUnwrapReturnsInnerForResponseController(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)

	inner := httptest.NewRecorder()
	var observed http.ResponseWriter
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uw, ok := w.(interface{ Unwrap() http.ResponseWriter })
		require.True(t, ok)
		observed = uw.Unwrap()
	}))

	req := httptest.NewRequest(http.MethodGet, "/unwrap", nil)
	req.Pattern = "/unwrap"
	handler.ServeHTTP(inner, req)

	assert.Same(t, inner, observed)
}

func TestResponseWriterAfterHijackDoesNotRecordFurtherUpdates(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	inner := &hijackRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             server,
		buf:              bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)),
	}

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		h, ok := w.(http.Hijacker)
		require.True(t, ok)
		_, _, err := h.Hijack()
		require.NoError(t, err)
		// After hijack, a misbehaving handler might still attempt to write.
		// These must not change the recorded status used for the metric label.
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Pattern = "/ws"
	handler.ServeHTTP(inner, req)

	// Exactly one metric observation with status 101, none with 500.
	assert.Equal(t, float64(1), gatherCounter(t, reg, "GET", "/ws", "101"))
	assert.Equal(t, float64(0), gatherCounter(t, reg, "GET", "/ws", "500"))
}
