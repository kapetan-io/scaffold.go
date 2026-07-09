package scaffold

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// ErrorStatus identifies a terminal error response scaffold emits on its own
// behalf — not one a caller's handler produces. Only these statuses may be
// customized via Binding.SetErrorHandler; any other value panics there.
type ErrorStatus int

const (
	// ErrorStatus404 is the terminal response when no RPCHandler claims the
	// request and the binding's mux (if any) has no matching route.
	ErrorStatus404 ErrorStatus = http.StatusNotFound
	// ErrorStatus500 is the response PanicRecovery writes after recovering a
	// panic raised by a downstream handler.
	ErrorStatus500 ErrorStatus = http.StatusInternalServerError
)

// defaultErrorHandler returns scaffold's built-in handler for status, used
// when no override is registered. The body is a DUH-shaped JSON Reply so
// callers — especially RPC clients — always receive a machine-readable
// response.
func defaultErrorHandler(status ErrorStatus) http.Handler {
	switch status {
	case ErrorStatus500:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeReply(w, http.StatusInternalServerError, "internal server error")
		})
	default:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeReply(w, http.StatusNotFound, fmt.Sprintf("no route matches %s %s", r.Method, r.URL.Path))
		})
	}
}

// writeReply writes a DUH Reply: a JSON body of {"code":"<code>","message":...}
// with Content-Type application/json and the given HTTP status. The code is
// rendered as a string to match the DUH-RPC Reply wire format.
func writeReply(w http.ResponseWriter, code int, msg string) {
	body, _ := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: strconv.Itoa(code), Message: msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// errHandlerCtxKey keys the per-binding error handler map carried on the
// request context so middleware (notably PanicRecovery) can resolve a
// registered handler for a scaffold-emitted status.
type errHandlerCtxKey struct{}

// withErrorHandlers returns a context carrying m so downstream middleware can
// look up registered error handlers.
func withErrorHandlers(ctx context.Context, m map[ErrorStatus]http.Handler) context.Context {
	return context.WithValue(ctx, errHandlerCtxKey{}, m)
}

// registeredErrorHandler returns the handler registered for status on the
// request's binding, or nil when none was registered. Indexing a nil map is
// safe, so this handles requests served without any registered handlers.
func registeredErrorHandler(ctx context.Context, status ErrorStatus) http.Handler {
	m, _ := ctx.Value(errHandlerCtxKey{}).(map[ErrorStatus]http.Handler)
	return m[status]
}
