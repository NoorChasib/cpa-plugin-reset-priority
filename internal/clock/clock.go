// Package clock abstracts time for the reset-priority engine so that exact
// deadline timers, hourly reconciliation, and bounded retry schedules can be
// tested deterministically with a fake clock.
package clock

import "time"

// Timer is a cancellable scheduled callback.
type Timer interface {
	// Stop cancels the timer. It reports whether the timer was still pending.
	Stop() bool
}

// Clock provides current time and callback scheduling.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// AfterFunc schedules f to run after d and returns a cancellable Timer.
	// The duration must be used exactly; implementations must not round it.
	AfterFunc(d time.Duration, f func()) Timer
}

// Real is the production Clock backed by the time package.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now() }

// AfterFunc implements Clock.
func (Real) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{t: time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }
