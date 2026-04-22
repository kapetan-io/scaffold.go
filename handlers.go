package scaffold

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// HealthHandler returns an http.Handler that invokes fn, marshals the result
// to JSON, and writes it as a 200 response. On marshal failure it logs the
// failure through log and writes a 500 response with body
// {"error":"health marshal failed"}. Any HTTP method is accepted.
//
// Panics raised inside fn are not caught; service authors should mount
// PanicRecovery in the middleware chain. A nil log panics at construction.
func HealthHandler(log *slog.Logger, fn func(ctx context.Context) any) http.Handler {
	if log == nil {
		panic("scaffold: HealthHandler called with nil *slog.Logger")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := fn(r.Context())
		body, err := json.Marshal(payload)
		if err != nil {
			log.Error("health marshal failed",
				"path", r.URL.Path,
				"error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"health marshal failed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// ReadyHandler returns an http.Handler that invokes fn and, on (true, _),
// writes a 200 response with empty body; on (false, reason), writes 503
// with Content-Type "text/plain; charset=utf-8" and reason as the body.
// Any HTTP method is accepted.
//
// Panics raised inside fn are not caught.
func ReadyHandler(fn func(ctx context.Context) (bool, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready, reason := fn(r.Context())
		if ready {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(reason))
	})
}
