package scaffold

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// MiddlewareFunc wraps an http.Handler with additional behavior.
type MiddlewareFunc func(next http.Handler) http.Handler

// PanicRecovery returns a middleware that recovers panics raised by the
// wrapped handler. On panic it logs the recovered value and stack at error
// level through log, then writes 500 Internal Server Error with body
// "internal server error" and Content-Type "text/plain; charset=utf-8". The
// log record's message and field names, and the response body, are pinned
// operator and user-facing contracts.
//
// If the response has already been written by the wrapped handler, the
// middleware emits the log record but does not attempt to overwrite the
// in-progress response.
//
// A nil log panics at construction.
func PanicRecovery(log *slog.Logger) MiddlewareFunc {
	if log == nil {
		panic("scaffold: PanicRecovery called with nil *slog.Logger")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pw := &panicTrackingWriter{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				log.Error("panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(v),
					"stack", string(debug.Stack()))
				if pw.written {
					return
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal server error"))
			}()
			next.ServeHTTP(pw, r)
		})
	}
}

// panicTrackingWriter wraps an http.ResponseWriter to observe whether the
// handler has committed to a status or body before a panic occurs. The
// wrapper delegates every call to the underlying writer.
type panicTrackingWriter struct {
	http.ResponseWriter
	written bool
}

func (w *panicTrackingWriter) WriteHeader(code int) {
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *panicTrackingWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}
