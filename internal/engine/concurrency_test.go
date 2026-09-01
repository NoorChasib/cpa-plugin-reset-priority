package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// gateProvider blocks every fetch until release is closed, signalling each
// start. It honors ctx so a hung test cannot deadlock forever.
type gateProvider struct {
	id      string
	started chan struct{}
	release chan struct{}
	result  func() (providers.Observation, error)
}

func (p *gateProvider) ID() string { return p.id }

func (p *gateProvider) FetchWeeklyReset(ctx context.Context, creds providers.Credentials) (providers.Observation, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return providers.Observation{}, ctx.Err()
	}
	return p.result()
}

// TestReconcileInvocationsSerialized: overlapping full reconciles (e.g.
// hourly tick plus management refresh) must run one at a time.
func TestReconcileInvocationsSerialized(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))

	gate := &gateProvider{
		id:      "claude",
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{HasWeekly: true, ResetAt: day(1), ObservedAt: env.clk.Now()}, nil
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background(), "first")
	}()
	<-gate.started // first reconcile is inside its provider fetch

	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background(), "second")
	}()
	// Give the second reconcile a chance to (incorrectly) start: with
	// serialization it must still be waiting before its roster listing.
	time.Sleep(50 * time.Millisecond)
	if got := env.host.listCount(); got != 1 {
		t.Fatalf("roster listed %d times while a reconcile was in flight, want 1 (not serialized)", got)
	}

	close(gate.release)
	wg.Wait()
	if got := env.host.listCount(); got != 2 {
		t.Fatalf("roster listed %d times after both reconciles, want 2", got)
	}
	env.assertDesired(map[string]int{"a": 100})
}

// TestDeadlineDemotionNotBlockedByReconcileNetworkFetch: while a full
// reconcile is stuck in provider network work, the exact deadline event must
// still demote and write back synchronously.
func TestDeadlineDemotionNotBlockedByReconcileNetworkFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// Swap in a blocking provider and start a reconcile that hangs in the
	// fetch phase (holding the reconcile mutex).
	gate := &gateProvider{
		id:      "claude",
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{}, errFake("late response")
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background(), "blocked")
	}()
	<-gate.started

	// The deadline fires while the reconcile is still blocked: demotion and
	// writeback must complete immediately.
	env.clk.Advance(30 * time.Minute)
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "awaiting_new_window" {
		t.Errorf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}

	close(gate.release)
	wg.Wait()
	// The late fetch failure cannot re-promote a.
	env.assertDesired(map[string]int{"b": 200, "a": 100})
}

// TestDeadlineCrossedDuringWritebackGetsSecondSynchronousPass verifies that a
// deadline which passes inside host.auth.save is not lost when the old timer is
// replaced at the end of the flush. The newly expired account must be demoted
// and persisted before deadline-triggered provider work is armed.
func TestDeadlineCrossedDuringWritebackGetsSecondSynchronousPass(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// Create unrelated physical drift so the next reconcile has a write pass
	// while the reset deadline is still nominally in the future.
	env.host.mu.Lock()
	for i := range env.host.entries {
		if env.host.entries[i].AuthIndex == "idx-b" {
			env.host.entries[i].Priority = 999
		}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(env.host.docs["idx-b"], &doc); err != nil {
		env.host.mu.Unlock()
		t.Fatal(err)
	}
	doc["priority"] = json.RawMessage(`999`)
	raw, err := json.Marshal(doc)
	if err != nil {
		env.host.mu.Unlock()
		t.Fatal(err)
	}
	env.host.docs["idx-b"] = raw
	env.host.mu.Unlock()

	// This reconcile performs two validation reads and two provider reads before
	// its first read-before-write. Move wall time across A's deadline during that
	// write without firing the existing timer callback.
	var hookMu sync.Mutex
	getNumber := 0
	env.host.beforeGet = func(string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		getNumber++
		if getNumber == 5 {
			env.clk.Set(deadline.Add(time.Millisecond))
		}
	}
	env.reconcile()

	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != string(ResetAwaitingNewWindow) {
		t.Fatalf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}
}

// TestPerformWritesSkipsStaleOrRemovedPlanEntries: a write plan computed
// before a newer pass changed desired priorities must not overwrite the
// newer values, and entries for removed accounts are skipped entirely.
func TestPerformWritesSkipsStaleOrRemovedPlanEntries(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})
	savesBefore := env.host.saveCount()
	getsBefore := len(env.host.getCalls)

	// Stale entry: planned desired no longer matches live desired.
	stale := []writeItem{{authIndex: "idx-a", name: "a.json", provider: "claude", desired: 999, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), stale)
	if got := env.host.saveCount(); got != savesBefore {
		t.Errorf("stale plan entry executed: %d saves, want %d", got, savesBefore)
	}
	env.assertPhysical(map[string]int{"a": 200})

	// Removed account: skipped without even reading the credential.
	removed := []writeItem{{authIndex: "idx-gone", name: "gone.json", provider: "claude", desired: 100, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), removed)
	for _, idx := range env.host.getCalls[getsBefore:] {
		if idx == "idx-gone" {
			t.Errorf("removed account was read during writeback")
		}
	}

	// A plan entry matching live desired still executes.
	env.eng.mu.Lock()
	env.eng.accounts["idx-a"].desired = 300
	env.eng.mu.Unlock()
	fresh := []writeItem{{authIndex: "idx-a", name: "a.json", provider: "claude", desired: 300, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), fresh)
	env.assertPhysical(map[string]int{"a": 300})
}

// hangingCaller blocks host.http.do until released, proving that
// request-timeout bounds provider fetches end-to-end through the bridge.
type hangingCaller struct {
	release chan struct{}
}

func (c *hangingCaller) Call(method string, request []byte) ([]byte, error) {
	if method != hostapi.MethodHostHTTPDo {
		return nil, fmt.Errorf("unexpected host callback %s", method)
	}
	<-c.release
	return nil, fmt.Errorf("released")
}

// TestRequestTimeoutBoundsProviderFetchEndToEnd: a hung upstream must not
// hang reconciliation; the configured request-timeout applies through
// engine -> provider -> bridge, including fetches started from
// context.Background (deadline/recovery retries use the same fetchOne path).
func TestRequestTimeoutBoundsProviderFetchEndToEnd(t *testing.T) {
	cfg := defaultConfig()
	cfg.RequestTimeout = 50 * time.Millisecond
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))

	caller := &hangingCaller{release: make(chan struct{})}
	t.Cleanup(func() { close(caller.release) })
	bridge := hostapi.NewBridge(caller)
	env.eng.providers = map[string]providers.Provider{
		"claude": providers.NewClaude(bridge, env.clk.Now),
	}

	done := make(chan struct{})
	go func() {
		env.eng.Reconcile(context.Background(), "hung-upstream")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("reconcile did not return; request-timeout not applied end-to-end")
	}

	row, _ := env.statusRow("a")
	if row.ResetState != "unknown" {
		t.Errorf("reset state = %s, want unknown after timeout", row.ResetState)
	}
	if !strings.Contains(row.LastError, "context deadline exceeded") {
		t.Errorf("last error = %q, want a deadline error", row.LastError)
	}
	// The account still counts and holds the floor.
	env.assertDesired(map[string]int{"a": 100})
}

// TestAllFetchPathsShareGlobalConcurrencyLimit drives fetchOne directly, as
// deadline and recovery retry callbacks do. No more than four provider calls
// may enter even when many retry-style fetches overlap outside fetchAll.
func TestAllFetchPathsShareGlobalConcurrencyLimit(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	const accounts = 8
	for i := 0; i < accounts; i++ {
		env.addAccount("claude", fmt.Sprintf("account-%d", i), day(i+1))
	}
	env.reconcile()

	gate := &gateProvider{
		id:      "claude",
		started: make(chan struct{}, accounts),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{HasWeekly: true, ResetAt: day(9), ObservedAt: env.clk.Now()}, nil
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}

	var wg sync.WaitGroup
	for i := 0; i < accounts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			env.eng.fetchOne(context.Background(), fmt.Sprintf("idx-account-%d", index))
		}(i)
	}
	for i := 0; i < maxConcurrentFetches; i++ {
		select {
		case <-gate.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d provider calls entered, want %d", i, maxConcurrentFetches)
		}
	}
	select {
	case <-gate.started:
		t.Fatalf("a fifth provider call entered before one of the first four completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release)
	wg.Wait()
}
