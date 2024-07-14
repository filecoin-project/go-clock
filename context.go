package clock

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func (m *Mock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return m.WithDeadline(parent, m.Now().Add(timeout))
}

func (m *Mock) WithDeadline(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if cur, ok := parent.Deadline(); ok && cur.Before(deadline) {
		// The current deadline is already sooner than the new one.
		return context.WithCancel(parent)
	}
	ctx := &timerCtx{
		clock:    m,
		parent:   parent,
		deadline: deadline,
		done:     make(chan struct{}),
		children: make(map[*func()]struct{}),
	}
	dur := m.Until(deadline)

	// If it has already expired, cancel and return immediately.
	if dur <= 0 {
		ctx.cancel(context.DeadlineExceeded) // deadline has already passed
		return ctx, func() {}
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	ctx.timer = m.AfterFunc(dur, func() { ctx.cancel(context.DeadlineExceeded) })
	if parent.Done() != nil {
		ctx.unlink = context.AfterFunc(parent, func() { ctx.cancel(parent.Err()) })
	}

	return ctx, func() { ctx.cancel(context.Canceled) }
}

type timerCtx struct {
	clock    Clock
	parent   context.Context
	deadline time.Time

	mu       sync.Mutex
	err      error
	children map[*func()]struct{}
	done     chan struct{}
	timer    *Timer
	unlink   func() bool
}

func (c *timerCtx) cancel(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return // already canceled
	}
	c.err = err
	close(c.done)
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.unlink != nil {
		c.unlink()
		c.unlink = nil
	}
	if c.children != nil {
		for c := range c.children {
			fn := *c
			go (fn)()
		}
		c.children = nil
	}
}

// Used by go to avoid spawning a goroutine when attaching a cancel func.
func (c *timerCtx) AfterFunc(fn func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Already canceled, fire immediately.
	if c.children == nil {
		go (fn)()
		return func() bool { return false }
	}

	cb := &fn
	c.children[cb] = struct{}{}

	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.children == nil {
			return false
		}
		delete(c.children, cb)
		return true
	}
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) { return c.deadline, true }

func (c *timerCtx) Done() <-chan struct{} { return c.done }

func (c *timerCtx) Err() error { return c.err }

func (c *timerCtx) Value(key interface{}) interface{} { return c.parent.Value(key) }

func (c *timerCtx) String() string {
	return fmt.Sprintf("clock.WithDeadline(%s [%s])", c.deadline, c.deadline.Sub(c.clock.Now()))
}
