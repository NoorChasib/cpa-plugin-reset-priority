package hostapi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// blockingCaller admits any number of concurrent calls and blocks each one
// inside Call until released, modelling a native host callback that has entered
// the host's call function pointer and cannot be cancelled.
type blockingCaller struct {
	mu          sync.Mutex
	entered     int
	releaseOnce sync.Once

	response []byte
	release  chan struct{}
	entry    chan struct{}
}

func newBlockingCaller(response []byte, entrySignals int) *blockingCaller {
	return &blockingCaller{
		response: response,
		release:  make(chan struct{}),
		entry:    make(chan struct{}, entrySignals),
	}
}

func (c *blockingCaller) Call(method string, request []byte) ([]byte, error) {
	c.mu.Lock()
	c.entered++
	c.mu.Unlock()
	select {
	case c.entry <- struct{}{}:
	default:
	}
	<-c.release
	return c.response, nil
}

// enteredCount reports how many calls reached the host. This is the number the
// bound exists to cap: every entered-but-unreturned call pins a goroutine and
// an OS thread for as long as the host takes to return.
func (c *blockingCaller) enteredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entered
}

func (c *blockingCaller) releaseAll() {
	c.releaseOnce.Do(func() { close(c.release) })
}

// awaitEntry blocks until one more call has entered the fake host.
func (c *blockingCaller) awaitEntry(t *testing.T) {
	t.Helper()
	select {
	case <-c.entry:
	case <-time.After(5 * time.Second):
		t.Fatalf("no host callback entered within the timeout")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func httpProbe() HTTPRequest {
	return HTTPRequest{Method: "GET", URL: "https://example.com/usage"}
}

// TestStuckCallbackRetainsAdmissionAfterContextCancellation: cancelling the Go
// context ends the caller's wait, but the native call is still inside the host.
// Its admission must therefore be retained, not handed back — releasing it at
// cancellation time would let an unbounded number of native calls be inside the
// host at once while the accounting claimed otherwise.
func TestStuckCallbackRetainsAdmissionAfterContextCancellation(t *testing.T) {
	limiter := newCallbackLimiter(1)
	caller := newBlockingCaller(envelopeWith(t, HTTPResponse{StatusCode: 200}), 4)
	bridge := newBridgeWithLimiter(caller, limiter)
	t.Cleanup(func() {
		caller.releaseAll()
		bridge.Drain()
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := bridge.HTTPDo(ctx, httpProbe())
		errCh <- err
	}()

	caller.awaitEntry(t)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("HTTPDo err = %v, want context.Canceled", err)
	}

	if got := limiter.inFlight(); got != 1 {
		t.Fatalf("in-flight admissions = %d, want 1: the stuck callback must keep its slot", got)
	}

	// With the only slot still held by the stuck callback, a new callback must
	// be refused rather than entering the host alongside it. The timeout is a
	// diagnostic, not the expected path: if admission were wrongly handed back
	// at cancellation this call would enter the blocked host and surface as
	// DeadlineExceeded rather than hanging the test.
	refusedCtx, cancelRefused := context.WithTimeout(context.Background(), time.Second)
	defer cancelRefused()
	if _, err := bridge.HTTPDo(refusedCtx, httpProbe()); !errors.Is(err, ErrHostCallbackLimit) {
		t.Fatalf("second HTTPDo err = %v, want ErrHostCallbackLimit", err)
	}
	if got := caller.enteredCount(); got != 1 {
		t.Fatalf("host callbacks entered = %d, want 1: the refused call must not reach the host", got)
	}

	// Once the native call returns, its admission is handed back and normal
	// service resumes.
	caller.releaseAll()
	waitFor(t, "the stuck callback to return its admission", func() bool { return limiter.inFlight() == 0 })
	recoveredCtx, cancelRecovered := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRecovered()
	if _, err := bridge.HTTPDo(recoveredCtx, httpProbe()); err != nil {
		t.Fatalf("HTTPDo after recovery: %v", err)
	}
}

// TestStuckCallbacksCannotAccumulateUnboundedly: a host that never returns
// (the audited host.http.do carries no plugin-side deadline) turns every
// request-timeout into a permanently stuck native call. Repeated timeouts must
// not keep entering the host; past the bound they must fail fast instead.
func TestStuckCallbacksCannotAccumulateUnboundedly(t *testing.T) {
	const bound = 2
	const attempts = 25

	limiter := newCallbackLimiter(bound)
	caller := newBlockingCaller(envelopeWith(t, HTTPResponse{StatusCode: 200}), attempts)
	bridge := newBridgeWithLimiter(caller, limiter)
	t.Cleanup(func() {
		caller.releaseAll()
		bridge.Drain()
	})

	var timedOut, refused int
	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := bridge.HTTPDo(ctx, httpProbe())
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			timedOut++
		case errors.Is(err, ErrHostCallbackLimit):
			refused++
		default:
			t.Fatalf("attempt %d err = %v, want DeadlineExceeded or ErrHostCallbackLimit", i, err)
		}
	}

	if timedOut != bound {
		t.Errorf("timed-out attempts = %d, want %d", timedOut, bound)
	}
	if refused != attempts-bound {
		t.Errorf("refused attempts = %d, want %d", refused, attempts-bound)
	}
	if got := caller.enteredCount(); got != bound {
		t.Errorf("host callbacks entered = %d, want %d: stuck callbacks accumulated past the bound", got, bound)
	}
	if got := limiter.inFlight(); got != bound {
		t.Errorf("in-flight admissions = %d, want %d", got, bound)
	}
}

// TestDrainWaitsForStuckCallbacksAndReturnsAdmissions: bounding admission must
// not weaken shutdown. Drain still blocks until every entered callback has left
// the host, and every admission is handed back so the bound does not erode
// across a plugin lifecycle.
func TestDrainWaitsForStuckCallbacksAndReturnsAdmissions(t *testing.T) {
	const bound = 2

	limiter := newCallbackLimiter(bound)
	caller := newBlockingCaller(envelopeWith(t, HTTPResponse{StatusCode: 200}), bound)
	bridge := newBridgeWithLimiter(caller, limiter)
	released := false
	defer func() {
		if !released {
			caller.releaseAll()
		}
	}()

	for i := 0; i < bound; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := bridge.HTTPDo(ctx, httpProbe())
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d err = %v, want context.DeadlineExceeded", i, err)
		}
	}
	if got := limiter.inFlight(); got != bound {
		t.Fatalf("in-flight admissions = %d, want %d", got, bound)
	}

	bridge.Quiesce()
	// Shutdown outranks the bound: a caller arriving during shutdown learns the
	// bridge is closing, not that it is momentarily saturated.
	if _, err := bridge.AuthList(context.Background()); !errors.Is(err, ErrBridgeShuttingDown) {
		t.Fatalf("callback during shutdown err = %v, want ErrBridgeShuttingDown", err)
	}

	drained := make(chan struct{})
	go func() {
		bridge.Drain()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatalf("Drain returned while stuck native callbacks were still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	caller.releaseAll()
	released = true
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatalf("Drain did not return after the native callbacks completed")
	}

	if got := limiter.inFlight(); got != 0 {
		t.Errorf("in-flight admissions after Drain = %d, want 0: admissions leaked", got)
	}
}

// TestHostCallbackBoundIsProcessWide: the resource being protected (OS threads
// pinned inside cgo) is per-process, so the bound must be too. Every Bridge
// built by NewBridge shares one limiter.
func TestHostCallbackBoundIsProcessWide(t *testing.T) {
	first := NewBridge(&scriptedCaller{})
	second := NewBridge(&scriptedCaller{})
	if first.limiter != second.limiter {
		t.Errorf("bridges have separate limiters; the bound must be process-wide")
	}
	if first.limiter != globalCallbackLimiter {
		t.Errorf("NewBridge did not bind the process-wide limiter")
	}
	if got := globalCallbackLimiter.capacity; got != MaxInFlightHostCallbacks {
		t.Errorf("global limiter capacity = %d, want %d", got, MaxInFlightHostCallbacks)
	}
}

// TestHostCallbackBoundSharedAcrossBridges: a stuck callback admitted through
// one bridge consumes shared capacity, and the bound applies to the synchronous
// callbacks too, not just to HTTPDo's worker.
func TestHostCallbackBoundSharedAcrossBridges(t *testing.T) {
	limiter := newCallbackLimiter(1)
	stuck := newBlockingCaller(envelopeWith(t, HTTPResponse{StatusCode: 200}), 2)
	occupying := newBridgeWithLimiter(stuck, limiter)
	t.Cleanup(func() {
		stuck.releaseAll()
		occupying.Drain()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := occupying.HTTPDo(ctx, httpProbe()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTPDo err = %v, want context.DeadlineExceeded", err)
	}

	other := newBridgeWithLimiter(&scriptedCaller{
		response: envelopeWith(t, AuthListResponse{Files: []AuthEntry{{Name: "a.json"}}}),
	}, limiter)
	if _, err := other.AuthList(context.Background()); !errors.Is(err, ErrHostCallbackLimit) {
		t.Fatalf("AuthList on a second bridge err = %v, want ErrHostCallbackLimit", err)
	}
}
