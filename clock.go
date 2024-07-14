package clock

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Re-export of time.Duration
type Duration = time.Duration

// Clock represents an interface to the functions in the standard library time
// package. Two implementations are available in the clock package. The first
// is a real-time clock which simply wraps the time package's functions. The
// second is a mock clock which will only change when
// programmatically adjusted.
type Clock interface {
	After(d time.Duration) <-chan time.Time
	AfterFunc(d time.Duration, f func()) *Timer
	Now() time.Time
	Since(t time.Time) time.Duration
	Until(t time.Time) time.Duration
	Sleep(d time.Duration)
	Tick(d time.Duration) <-chan time.Time
	Ticker(d time.Duration) *Ticker
	Timer(d time.Duration) *Timer
	WithDeadline(parent context.Context, d time.Time) (context.Context, context.CancelFunc)
	WithTimeout(parent context.Context, t time.Duration) (context.Context, context.CancelFunc)
}

// New returns an instance of a real-time clock.
func New() Clock {
	return &clock{}
}

// clock implements a real-time clock by simply wrapping the time package functions.
type clock struct{}

func (c *clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (c *clock) AfterFunc(d time.Duration, f func()) *Timer {
	return &Timer{realtime: time.AfterFunc(d, f)}
}

func (c *clock) Now() time.Time { return time.Now() }

func (c *clock) Since(t time.Time) time.Duration { return time.Since(t) }

func (c *clock) Until(t time.Time) time.Duration { return time.Until(t) }

func (c *clock) Sleep(d time.Duration) { time.Sleep(d) }

func (c *clock) Tick(d time.Duration) <-chan time.Time {
	//lint:ignore SA1015 we're intentionally replicating this API.
	return time.Tick(d)
}

func (c *clock) Ticker(d time.Duration) *Ticker {
	t := time.NewTicker(d)
	return &Ticker{C: t.C, realtime: t}
}

func (c *clock) Timer(d time.Duration) *Timer {
	t := time.NewTimer(d)
	return &Timer{C: t.C, realtime: t}
}

func (c *clock) WithDeadline(parent context.Context, d time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, d)
}

func (c *clock) WithTimeout(parent context.Context, t time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, t)
}

// Mock represents a mock clock that only moves forward programmically.
// It can be preferable to a real-time clock when testing time-based functionality.
type Mock struct {
	// mu protects all other fields in this struct, and the data that they
	// point to.
	mu sync.Mutex

	now    time.Time   // current time
	timers clockTimers // tickers & timers
}

// NewMock returns an instance of a mock clock.
// The current time of the mock clock on initialization is the Unix epoch.
func NewMock() *Mock {
	return &Mock{now: time.Unix(0, 0)}
}

// Add moves the current time of the mock clock forward by the specified duration.
func (m *Mock) Add(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set(m.now.Add(d))
}

// WaitForAllTimers keeps advancing the clock until no more timers are scheduled.
//
// WARNING: Tickers will cause this function to continue forever until something cancels the ticker.
// It will sleep after each bit of progress to the tickers a chance to run, but using this with
// tickers is not recommended.
func (m *Mock) WaitForAllTimers() time.Time {
	// Continue to execute timers until there are no more
	m.mu.Lock()
	defer m.mu.Unlock()

	for len(m.timers) > 0 {
		m.fireNext()
	}

	return m.now
}

// Set sets the current time.
func (m *Mock) Set(until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.set(until)
}

func (m *Mock) set(until time.Time) {
	for len(m.timers) > 0 && !m.timers[0].next.After(until) {
		m.fireNext()
	}

	// Check to make sure we're still in the past, a concurrent call may have moved us forward.
	if m.now.Before(until) {
		m.now = until

		// And pause to let others run.
		m.mu.Unlock()
		gosched()
		m.mu.Lock()
	}
}

func (m *Mock) fireNext() {
	t := heap.Pop(&m.timers).(*mockTimer)
	m.now = t.next
	t.fire()

	// Pause for a bit to let other goroutines run.
	m.mu.Unlock()
	gosched()
	m.mu.Lock()
}

// After waits for the duration to elapse and then sends the current time on the returned channel.
func (m *Mock) After(d time.Duration) <-chan time.Time {
	return m.Timer(d).C
}

// AfterFunc waits for the duration to elapse and then executes a function in its own goroutine.
// A Timer is returned that can be stopped.
func (m *Mock) AfterFunc(d time.Duration, f func()) *Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &mockTimer{
		fn:    func(_ time.Time) { go f() },
		mock:  m,
		next:  m.now.Add(d),
		index: -1,
	}

	if d <= 0 {
		t.fire()
	} else {
		m.schedule(t)
	}
	return &Timer{mocktime: t}
}

func (m *Mock) schedule(t *mockTimer) {
	if t.index >= 0 {
		panic("already scheduled!")
	}
	heap.Push(&m.timers, t)
}

// Now returns the current wall time on the mock clock.
func (m *Mock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Since returns time since `t` using the mock clock's wall time.
func (m *Mock) Since(t time.Time) time.Duration {
	return m.Now().Sub(t)
}

// Until returns time until `t` using the mock clock's wall time.
func (m *Mock) Until(t time.Time) time.Duration {
	return t.Sub(m.Now())
}

// Sleep pauses the goroutine for the given duration on the mock clock.
// The clock must be moved forward in a separate goroutine.
func (m *Mock) Sleep(d time.Duration) {
	<-m.After(d)
}

// Tick is a convenience function for Ticker().
// It will return a ticker channel that cannot be stopped.
func (m *Mock) Tick(d time.Duration) <-chan time.Time {
	return m.Ticker(d).C
}

// Ticker creates a new instance of Ticker.
func (m *Mock) Ticker(d time.Duration) *Ticker {
	if d <= 0 {
		panic("ticker duration must be > 0")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan time.Time, 1)
	t := &mockTimer{
		mock: m,
		fn: func(now time.Time) {
			select {
			case ch <- now:
			default:
			}
		},
		d:     &d,
		index: -1,
		next:  m.now.Add(d),
	}
	m.schedule(t)
	return &Ticker{C: ch, mocktime: t}
}

// Timer creates a new instance of Timer.
func (m *Mock) Timer(d time.Duration) *Timer {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan time.Time, 1)
	t := &mockTimer{
		mock: m,
		fn: func(now time.Time) {
			select {
			case ch <- now:
			default:
			}
		},
		index: -1,
		next:  m.now.Add(d),
	}
	if d <= 0 {
		t.fire()
	} else {
		m.schedule(t)
	}
	return &Timer{C: ch, mocktime: t}
}

// clockTimers represents a heap of timers.
type clockTimers []*mockTimer

func (a *clockTimers) Len() int { return len(*a) }
func (a *clockTimers) Swap(i, j int) {
	(*a)[i], (*a)[j] = (*a)[j], (*a)[i]
	(*a)[i].index = i
	(*a)[j].index = j
}
func (a *clockTimers) Less(i, j int) bool {
	return (*a)[i].next.Before((*a)[j].next)
}

func (a *clockTimers) Pop() any {
	s := *a
	item := s[len(s)-1]
	s[len(s)-1] = nil
	*a = s[:len(s)-1]
	item.index = -1
	return item
}

func (a *clockTimers) Push(x any) {
	t := x.(*mockTimer)
	t.index = len(*a)
	*a = append(*a, t)
}

// Stop turns off the ticker.
func (t *Timer) Stop() bool {
	if t.realtime != nil {
		return t.realtime.Stop()
	}
	return t.mocktime.stop()
}

// Reset changes the expiry time of the timer
func (t *Timer) Reset(d time.Duration) bool {
	if t.realtime != nil {
		return t.realtime.Reset(d)
	}

	return t.mocktime.reset(d)
}

// Ticker is a mock implementation of [time.Ticker].
type Ticker struct {
	C        <-chan time.Time
	realtime *time.Ticker
	mocktime *mockTimer
}

// Timer is a mock implementation of [time.Timer].
type Timer struct {
	C        <-chan time.Time
	realtime *time.Timer
	mocktime *mockTimer
}

// Stop turns off the ticker.
func (t *Ticker) Stop() {
	if t.realtime != nil {
		t.realtime.Stop()
	} else {
		t.mocktime.stop()
	}
}

// Reset resets the ticker to a new duration.
func (t *Ticker) Reset(dur time.Duration) {
	if t.realtime != nil {
		t.realtime.Reset(dur)
		return
	}

	t.mocktime.reset(dur)
}

type mockTimer struct {
	next  time.Time       // next tick time
	d     *time.Duration  // time between ticks, if a ticker.
	fn    func(time.Time) // function to call when the timer elapses
	mock  *Mock
	index int // the index in the timer heap, if running
}

func (t *mockTimer) fire() {
	now := t.mock.now

	if t.d != nil {
		t.next = now.Add(*t.d)
		t.mock.schedule(t)
	}

	(t.fn)(now)
}

func (t *mockTimer) stop() bool {
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	if t.index < 0 {
		return false
	}
	heap.Remove(&t.mock.timers, t.index)
	t.index = -1
	return true
}

func (t *mockTimer) reset(dur time.Duration) bool {
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()

	if t.d != nil {
		*t.d = dur
	}

	t.next = t.mock.now.Add(dur)

	if t.index < 0 {
		heap.Push(&t.mock.timers, t)
		return false
	} else {
		heap.Fix(&t.mock.timers, t.index)
		return true
	}
}

// Sleep momentarily so that other goroutines can process.
func gosched() { time.Sleep(1 * time.Millisecond) }

var (
	// type checking
	_ Clock = &Mock{}
)
