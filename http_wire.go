package scaffold

import "net/http"

// newHTTPServer constructs an *http.Server for b. The binding's
// finalHandler must have been composed before this call. Field mapping is
// per the tech spec HTTP Server Wiring table: Handler, ReadTimeout,
// WriteTimeout, IdleTimeout, and TLSConfig map directly from the binding.
// ReadHeaderTimeout is left zero so Go inherits ReadTimeout, and
// MaxHeaderBytes is left at Go's 1 MB default.
func newHTTPServer(b *Binding) *http.Server {
	return &http.Server{
		Handler:      b.finalHandler,
		ReadTimeout:  b.readTO,
		WriteTimeout: b.writeTO,
		IdleTimeout:  b.idleTO,
		TLSConfig:    b.tls,
	}
}
