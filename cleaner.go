package scaffold

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// Cleaner runs registered cleanup functions in LIFO order during daemon
// shutdown. Functions registered through Add are invoked by Clean in reverse
// registration order so resources opened later tear down first. Per-function
// panics and errors are logged through the logger supplied at construction
// and do not abort the remainder of the cleanup sequence.
type Cleaner struct {
	mu      sync.Mutex
	log     *slog.Logger
	stack   []func(context.Context) error
	running bool
	done    bool
}

// NewCleaner returns a Cleaner that reports per-function failures through
// log. A nil log panics at construction so that panic recovery and error
// reporting inside Clean always have a non-nil target.
func NewCleaner(log *slog.Logger) *Cleaner {
	if log == nil {
		panic("scaffold: NewCleaner called with nil *slog.Logger")
	}
	return &Cleaner{log: log}
}

// Add registers fn to be invoked during Clean. Calling Add after Clean has
// started panics to surface the programmer error.
func (c *Cleaner) Add(fn func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running || c.done {
		panic("scaffold: Cleaner.Add called after Clean started")
	}
	c.stack = append(c.stack, fn)
}

// Clean invokes each registered function in LIFO order. Per-function panics
// and errors are logged; execution proceeds to the next function regardless.
// The first call returns nil. Subsequent calls return a non-nil error
// identifying the double-call and do not rerun the stack.
func (c *Cleaner) Clean(ctx context.Context) error {
	c.mu.Lock()
	if c.running || c.done {
		c.mu.Unlock()
		return errors.New("scaffold: Cleaner.Clean called more than once")
	}
	c.running = true
	snapshot := c.stack
	c.stack = nil
	c.mu.Unlock()

	for i := len(snapshot) - 1; i >= 0; i-- {
		c.runOne(ctx, snapshot[i])
	}

	c.mu.Lock()
	c.running = false
	c.done = true
	c.mu.Unlock()
	return nil
}

func (c *Cleaner) runOne(ctx context.Context, fn func(context.Context) error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("cleaner function panicked",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if err := fn(ctx); err != nil {
		c.log.Error("cleaner function returned error", "error", err)
	}
}
