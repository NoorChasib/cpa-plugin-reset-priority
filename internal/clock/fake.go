package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a deterministic Clock for tests. Advance moves time forward and
// fires due timers in chronological order, including timers scheduled by the
// callbacks themselves, so exact-deadline and retry sequences can be driven
// step by step.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers []*fakeTimer
}

type fakeTimer struct {
	clk     *Fake
	at      time.Time
	seq     int
	f       func()
	stopped bool
	fired   bool
}

// NewFake returns a Fake clock starting at now.
func NewFake(now time.Time) *Fake { return &Fake{now: now} }

// Now implements Clock.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc implements Clock. Negative durations are treated as zero.
func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		d = 0
	}
	t := &fakeTimer{clk: c, at: c.now.Add(d), seq: c.seq, f: f}
	c.seq++
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Set moves the clock to target without firing timers. It is intended for
// tests that need to model wall time passing inside a blocking callback; normal
// timer-driven tests should use Advance or AdvanceTo.
func (c *Fake) Set(target time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = target
}

// Advance moves the clock forward by d, firing every due timer in order.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.AdvanceTo(target)
}

// AdvanceTo moves the clock to target, firing every due timer in order.
// Callbacks run synchronously on the calling goroutine without the clock
// lock held, so they may schedule further timers.
func (c *Fake) AdvanceTo(target time.Time) {
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if t.stopped || t.fired || t.at.After(target) {
				continue
			}
			if next == nil || t.at.Before(next.at) || (t.at.Equal(next.at) && t.seq < next.seq) {
				next = t
			}
		}
		if next == nil {
			if target.After(c.now) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		if next.at.After(c.now) {
			c.now = next.at
		}
		next.fired = true
		f := next.f
		c.mu.Unlock()
		f()
	}
}

// PendingAt returns the scheduled fire times of all pending timers, sorted.
// Tests use it to assert that deadline timers use exact timestamps.
func (c *Fake) PendingAt() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, 0, len(c.timers))
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			out = append(out, t.at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
