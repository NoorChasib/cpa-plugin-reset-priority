package hostapi

import (
	"errors"
	"sync"
)

// MaxInFlightHostCallbacks bounds how many native host callbacks may be inside
// the host's call function pointer at one time, across the whole process.
//
// Sizing: the engine issues at most maxConcurrentFetches (4) concurrent
// host.http.do calls, plus a small number of synchronous auth/log callbacks, so
// this leaves an order of magnitude of headroom over steady-state demand. It is
// a backstop against stuck callbacks piling up, not a throughput control:
// reaching it means callbacks are not returning, which is already a fault.
const MaxInFlightHostCallbacks = 64

// ErrHostCallbackLimit is returned when the process-wide in-flight bound is
// already fully held. It is a distinct error from ErrBridgeShuttingDown: the
// bridge is open, but too many earlier callbacks are still inside the host.
var ErrHostCallbackLimit = errors.New("host callback limit reached")

// callbackLimiter caps concurrent native host callbacks.
//
// Admission is deliberately non-blocking. A blocking semaphore would be worse
// than the problem it solves: waiters would themselves accumulate without
// bound, per-request timeouts would stop being honored (the wait happens before
// the context is consulted), and a shutdown drain could stall behind callers
// queued for capacity that only a stuck callback can release. Failing fast
// turns saturation into an ordinary callback error that the engine already
// knows how to retry.
type callbackLimiter struct {
	mu   sync.Mutex
	held int

	// capacity is immutable after construction.
	capacity int
}

func newCallbackLimiter(capacity int) *callbackLimiter {
	return &callbackLimiter{capacity: capacity}
}

// acquire takes one admission, reporting false when none is available.
func (l *callbackLimiter) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held >= l.capacity {
		return false
	}
	l.held++
	return true
}

// release hands one admission back. It must be called exactly once per
// successful acquire, and only after the native call has actually returned.
func (l *callbackLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held > 0 {
		l.held--
	}
}

// inFlight reports the currently held admissions.
func (l *callbackLimiter) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// globalCallbackLimiter is shared by every Bridge. The scarce resource is
// per-process (each callback blocked in cgo pins an OS thread, and the Go
// runtime hard-fails the whole process past its thread limit), so a per-Bridge
// bound would not actually bound anything.
var globalCallbackLimiter = newCallbackLimiter(MaxInFlightHostCallbacks)
