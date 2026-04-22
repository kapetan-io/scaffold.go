package sprometheus

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
)

// responseWriter wraps an http.ResponseWriter to capture the status code for
// metric emission and to forward optional capabilities (Flush, Hijack, Unwrap)
// from the underlying writer.
//
// The wrapper tracks whether the request has been hijacked; once Hijack
// succeeds, the handler has taken ownership of the connection and no further
// metric updates should be recorded by the middleware for that request. The
// middleware still emits a single metric on handler return using the status
// captured at hijack time (typically 101 for WebSocket upgrades).
//
// http.Pusher is deliberately NOT forwarded: HTTP/2 server push is deprecated
// and not supported by the metrics wrapper.
type responseWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.hijacked {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.hijacked {
		return w.ResponseWriter.Write(b)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's Flush when it implements
// http.Flusher. When the underlying writer does not implement http.Flusher the
// call is a no-op; this matches the convention of chained middleware that
// unconditionally wraps responses.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer's Hijack when it implements
// http.Hijacker. On successful hijack, the wrapper marks itself hijacked so
// that subsequent WriteHeader/Write calls do not double-count or overwrite the
// captured status.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("sprometheus: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
		if w.status == 0 {
			w.status = http.StatusSwitchingProtocols
		}
	}
	return conn, rw, err
}

// Unwrap returns the underlying http.ResponseWriter so that callers using
// http.NewResponseController can reach capabilities we do not explicitly
// forward.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
