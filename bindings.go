package scaffold

import (
	"fmt"
	"sync"
)

// Bindings is the contract used to register and look up named bindings on a
// daemon. Options.Bindings accepts any implementation; the two built-ins
// (DefaultBindings, TestBindings) cover production and standard test use.
//
// Third-party implementations are not a supported v1 extension point —
// the lifecycle needs Add-order iteration via an unexported method that
// only the built-in bindingStore provides.
type Bindings interface {
	// Add registers a new binding under name with the given port hint and
	// returns the *Binding for further configuration. Panics if name is
	// already registered.
	Add(name string, port int) *Binding
	// Get returns the *Binding registered under name, or nil if no such
	// binding exists.
	Get(name string) *Binding
}

// bindingStore is the shared implementation embedded by DefaultBindings and
// TestBindings. It tracks bindings both by name (for Get) and by insertion
// order so the lifecycle orchestrator can iterate in Add-order for
// deterministic open / reverse-teardown sequencing.
type bindingStore struct {
	mu     sync.Mutex
	order  []string
	byName map[string]*Binding
}

// add is the shared mutator DefaultBindings and TestBindings call to insert
// a new binding. The caller supplies the listen address so the two built-ins
// can diverge on that dimension without duplicating the store bookkeeping.
func (s *bindingStore) add(name, listenAddr string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byName == nil {
		s.byName = make(map[string]*Binding)
	}
	if _, exists := s.byName[name]; exists {
		panic(fmt.Sprintf("scaffold: Bindings.Add: duplicate binding name %q", name))
	}
	b := &Binding{
		name:       name,
		listenAddr: listenAddr,
	}
	s.byName[name] = b
	s.order = append(s.order, name)
	return b
}

// Get returns the *Binding registered under name, or nil if no binding
// with that name has been added.
func (s *bindingStore) Get(name string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byName[name]
}

// orderedBindings returns a snapshot of the registered bindings in the
// order they were added. The lifecycle orchestrator uses this to open
// listeners in Add-order and tear them down in reverse order.
func (s *bindingStore) orderedBindings() []*Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Binding, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.byName[name])
	}
	return out
}

// DefaultBindings is the production Bindings implementation. Each
// registered binding listens on :<port> (all interfaces).
type DefaultBindings struct {
	bindingStore
}

// Add registers a new binding under name that will listen on :port when the
// daemon starts. Panics if name is already registered.
func (d *DefaultBindings) Add(name string, port int) *Binding {
	return d.add(name, fmt.Sprintf(":%d", port))
}

// TestBindings is the test-oriented Bindings implementation. Every
// registered binding listens on 127.0.0.1:0 (loopback, kernel-assigned
// port) regardless of the port argument supplied to Add. Tests read the
// assigned port via Instance.Addr after Start returns.
type TestBindings struct {
	bindingStore
}

// Add registers a new binding under name that will listen on 127.0.0.1:0
// when the daemon starts. The port argument is accepted for interface
// compatibility but ignored. Panics if name is already registered.
func (t *TestBindings) Add(name string, port int) *Binding {
	_ = port
	return t.add(name, "127.0.0.1:0")
}
