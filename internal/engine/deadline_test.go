package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// TestDeadlineRolloverDemotesBeforeAnyFetch is the core rollover test from
// spec section 21: A resets at T, B at T+24h. At T-1ms A is highest; at T,
// before any successful provider fetch, A is awaiting_new_window and B is
// higher priority, with the change already written back.
func TestDeadlineRolloverDemotesBeforeAnyFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// At T-1ms nothing changes.
	env.clk.Advance(deadline.Sub(baseTime) - time.Millisecond)
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	fetchesBefore := env.claude.callCount(token("a"))

	// At exactly T the deadline fires: local demotion + writeback happen
	// synchronously; the fresh fetch is queued but has NOT run yet.
	env.clk.Advance(time.Millisecond)
	if got := env.claude.callCount(token("a")); got != fetchesBefore {
		t.Fatalf("provider fetched before demotion assertion: %d calls", got)
	}
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "awaiting_new_window" {
		t.Errorf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}
	if env.async.pending() == 0 {
		t.Errorf("no asynchronous fresh fetch was queued at the deadline")
	}
}

func TestReconcileDemotesExpiredKnownDeadlineBeforeProviderFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// Advance the wall clock without firing the deadline timer. This models an
	// engine that resumes a full reconcile after the timer was missed.
	env.clk.Set(deadline.Add(time.Nanosecond))
	checker := &sentinelCheckingProvider{
		host: env.host, authIndex: "idx-a", resetAt: deadline.Add(7 * 24 * time.Hour), now: env.clk.Now,
	}
	env.eng.providers = map[string]providers.Provider{"claude": checker}

	env.reconcile()

	checker.mu.Lock()
	calls := checker.calls
	observed := append([]int(nil), checker.observed...)
	checker.mu.Unlock()
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
	if pending := env.async.pending(); pending != 0 {
		t.Fatalf("reconcile left %d duplicate immediate retry task(s), want 0", pending)
	}
	for _, priority := range observed {
		if priority != 100 {
			t.Fatalf("provider observed expired account priority %d, want demoted priority 100", priority)
		}
	}
}

func TestReconcileDemotesDeadlineCrossedDuringCredentialReadBeforeProviderFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// Begin just before the known deadline. The first A read validates the
	// roster; the second is the credential read immediately before provider HTTP.
	// Cross the deadline there without firing the timer to reproduce a delayed
	// exact-deadline callback.
	env.clk.Set(deadline.Add(-time.Nanosecond))
	var aReads int
	env.host.beforeGet = func(authIndex string) {
		if authIndex != "idx-a" {
			return
		}
		aReads++
		if aReads == 2 {
			env.clk.Set(deadline.Add(time.Nanosecond))
		}
	}
	checker := &sentinelCheckingProvider{
		host: env.host, authIndex: "idx-a", token: token("a"),
		resetAt: deadline.Add(7 * 24 * time.Hour), now: env.clk.Now,
	}
	env.eng.providers = map[string]providers.Provider{"claude": checker}

	env.reconcile()

	checker.mu.Lock()
	observed := append([]int(nil), checker.observed...)
	checker.mu.Unlock()
	if len(observed) != 1 || observed[0] != 100 {
		t.Fatalf("provider observed A priorities %v, want one post-demotion priority 100", observed)
	}
}

func TestReconcileReservesDeadlineFetchBeforeRosterValidation(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()
	env.clk.Set(deadline.Add(-time.Nanosecond))
	before := env.claude.callCount(token("a"))

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var hookMu sync.Mutex
	blocked := false
	env.host.beforeGet = func(authIndex string) {
		if authIndex != "idx-a" {
			return
		}
		hookMu.Lock()
		if blocked {
			hookMu.Unlock()
			return
		}
		blocked = true
		hookMu.Unlock()
		close(readStarted)
		<-releaseRead
	}

	done := make(chan struct{})
	var releaseOnce sync.Once
	releaseValidation := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	defer releaseValidation()
	go func() {
		env.eng.Reconcile(context.Background())
		close(done)
	}()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not reach roster credential validation")
	}

	// The exact deadline fires while roster validation is blocked. The active full
	// pass must already own the post-demotion fetch, so the timer cannot queue a
	// detached duplicate without the management callback context.
	env.clk.Advance(time.Nanosecond)
	if pending := env.async.pending(); pending != 0 {
		t.Fatalf("deadline during roster validation queued %d detached immediate fetches, want 0", pending)
	}

	releaseValidation()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not finish after roster validation release")
	}
	if got := env.claude.callCount(token("a")); got != before+1 {
		t.Fatalf("provider calls = %d, want exactly one full-pass post-demotion fetch", got-before)
	}
}

func TestRecoverySentinelFlushDoesNotConsumeAnotherAccountsReconcileFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()

	// Quarantine B, then provide genuine post-sentinel recovery evidence without
	// reconciling yet. The next pass must both persist/verify B's sentinel and
	// handle A's missed deadline.
	env.host.updateEntry("idx-b", func(entry *hostapi.AuthEntry) {
		entry.Disabled = true
		entry.Status = "disabled"
	})
	env.reconcile()
	recoverAccount(env, "idx-b")

	env.clk.Set(deadline.Add(time.Nanosecond))
	env.claude.setReset(token("a"), deadline.Add(7*24*time.Hour))
	env.claude.setReset(token("b"), deadline.Add(6*24*time.Hour))
	before := env.claude.callCount(token("a"))

	env.reconcile()

	if got := env.claude.callCount(token("a")); got != before+1 {
		t.Fatalf("A provider calls = %d, want exactly one full-pass fetch", got-before)
	}
	if pending := env.async.pending(); pending != 0 {
		t.Fatalf("recovery sentinel flush queued %d duplicate immediate fetch(es)", pending)
	}
}

func TestRosterFailureAfterMissedDeadlineKeepsImmediateRetryStep(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()

	env.clk.Set(deadline.Add(time.Nanosecond))
	env.host.mu.Lock()
	env.host.listErr = errFake("roster unavailable")
	env.host.mu.Unlock()
	before := env.claude.callCount(token("a"))

	if result := env.eng.Reconcile(context.Background()); result != ReconcileResultError {
		t.Fatalf("reconcile result = %s, want error", result)
	}
	env.assertPhysical(map[string]int{"a": 100})
	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("roster failure queued %d immediate retry tasks, want 1", pending)
	}
	env.async.drain()
	if got := env.claude.callCount(token("a")); got != before+1 {
		t.Fatalf("immediate retry calls = %d, want 1", got-before)
	}
}

func TestReconcileFailureAfterMissedDeadlineDoesNotLaunchDuplicateImmediateRetry(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()

	before := env.claude.callCount(token("a"))
	env.clk.Set(deadline.Add(time.Nanosecond))
	env.claude.setErr(token("a"), errFake("provider unavailable"))
	env.reconcile()

	if got := env.claude.callCount(token("a")); got != before+1 {
		t.Fatalf("provider calls = %d, want one full-pass retry after missed deadline", got-before)
	}
	if pending := env.async.pending(); pending != 0 {
		t.Fatalf("failed reconcile left %d duplicate immediate retry task(s), want 0", pending)
	}
	wantRetryAt := env.clk.Now().Add(5 * time.Second)
	found := false
	for _, at := range env.clk.PendingAt() {
		if at.Equal(wantRetryAt) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("failed reconcile did not arm the documented +5s delayed retry; pending: %v", env.clk.PendingAt())
	}
}

func TestProviderOptOutPrunesBeforeMissedDeadlineFlush(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// Move past A's deadline without firing timers, then opt out of Claude. The
	// reconfiguration reconcile must remove the retained Claude state before its
	// pre-fetch deadline flush can rewrite either physical priority.
	env.clk.Set(deadline.Add(time.Nanosecond))
	cfg := defaultConfig()
	cfg.ManageClaude = false
	env.eng.Reconfigure(cfg)
	env.async.drain()

	env.assertPhysical(map[string]int{"a": 200, "b": 100})
	for _, group := range env.eng.Status().Providers {
		if len(group.Accounts) != 0 {
			t.Fatalf("status retained opted-out provider accounts: %+v", group.Accounts)
		}
	}
}

func TestFullReconcileFailureRearmsExistingAwaitingRetryLadder(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()

	env.claude.setErr(token("a"), errFake("provider unavailable"))
	env.clk.AdvanceTo(deadline)
	env.async.drain() // the deadline-owned immediate attempt fails; delayed ladder remains

	// A full pass replaces that ladder with its own immediate attempt. If that
	// attempt also fails, it must arm a fresh +5s... ladder rather than canceling
	// all retries until the hourly reconciliation.
	env.reconcile()
	want := env.clk.Now().Add(5 * time.Second)
	for _, at := range env.clk.PendingAt() {
		if at.Equal(want) {
			return
		}
	}
	t.Fatalf("failed full reconcile did not rearm +5s retry; pending: %v", env.clk.PendingAt())
}

func TestPreDeadlineReconcileResultDoesNotCountAsPostDemotionImmediateAttempt(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()
	env.clk.Set(deadline.Add(-time.Nanosecond))

	call := &controlledFetch{
		started: make(chan struct{}),
		release: make(chan struct{}),
		obs: providers.Observation{
			HasWeekly:  true,
			ResetAt:    deadline.Add(7 * 24 * time.Hour),
			ObservedAt: deadline.Add(time.Nanosecond),
		},
	}
	env.eng.providers = map[string]providers.Provider{
		"claude": &controlledProvider{id: "claude", calls: []*controlledFetch{call}},
	}

	done := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(call.release) }) }
	defer releaseProvider()
	go func() {
		env.eng.Reconcile(context.Background())
		close(done)
	}()
	select {
	case <-call.started:
	case <-time.After(time.Second):
		t.Fatal("reconcile provider call did not start before deadline")
	}
	env.clk.Set(deadline.Add(time.Nanosecond))
	releaseProvider()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not finish after provider release")
	}

	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("discarded pre-deadline result queued %d immediate retry tasks, want 1", pending)
	}
}

func TestRetryInvalidatedByReconcileReservationCannotReachProvider(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	before := env.claude.callCount(token("a"))

	env.eng.mu.Lock()
	acct := env.eng.accounts["idx-a"]
	acct.resetState = ResetAwaitingNewWindow
	acct.resetAt = baseTime
	acct.fetchEpoch++
	seq := acct.retrySeq
	env.eng.mu.Unlock()

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	env.host.beforeGet = func(authIndex string) {
		if authIndex != "idx-a" {
			return
		}
		close(readStarted)
		<-releaseRead
	}
	done := make(chan struct{})
	var releaseOnce sync.Once
	releaseCredentialRead := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	defer releaseCredentialRead()
	go func() {
		env.eng.retryFetch("idx-a", seq)
		close(done)
	}()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("retry did not reach credential read")
	}

	env.eng.mu.Lock()
	env.eng.reserveReconcileFetchesLocked([]string{"idx-a"})
	env.eng.mu.Unlock()
	releaseCredentialRead()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("invalidated retry did not finish")
	}
	if got := env.claude.callCount(token("a")); got != before {
		t.Fatalf("invalidated retry issued %d provider call(s), want 0", got-before)
	}
}

func TestDeadlineTimerUsesExactTimestamp(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	// Odd sub-second deadline: the timer must fire at exactly this instant,
	// not rounded to any hour/minute/day boundary.
	deadline := baseTime.Add(83*time.Minute + 17*time.Second + 123456789*time.Nanosecond)
	env.addAccount("claude", "a", deadline)
	env.reconcile()

	found := false
	for _, at := range env.clk.PendingAt() {
		if at.Equal(deadline) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no timer armed at the exact deadline %s; pending: %v", deadline, env.clk.PendingAt())
	}

	snap := env.eng.Status()
	if snap.NextDeadlineAt == nil || !snap.NextDeadlineAt.Equal(deadline) {
		t.Errorf("status next deadline = %v, want %s", snap.NextDeadlineAt, deadline)
	}
}

func TestDeadlineFreshFutureResetReinsertsAtNewPosition(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.addAccount("claude", "c", deadline.Add(48*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 300, "b": 200, "c": 100})

	// A's provider will report the next weekly window (latest of all).
	env.claude.setReset(token("a"), deadline.Add(7*24*time.Hour))
	env.clk.AdvanceTo(deadline)
	// Before the fetch: A demoted below confirmed accounts.
	env.assertDesired(map[string]int{"b": 300, "c": 200, "a": 100})

	env.async.drain() // run the queued fresh fetch
	env.assertDesired(map[string]int{"b": 300, "c": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "confirmed" {
		t.Errorf("account a reset state = %s, want confirmed after fresh fetch", row.ResetState)
	}
	env.assertPhysical(map[string]int{"b": 300, "c": 200, "a": 100})

	// The next deadline is B's reset now.
	snap := env.eng.Status()
	if snap.NextDeadlineAt == nil || !snap.NextDeadlineAt.Equal(deadline.Add(24*time.Hour)) {
		t.Errorf("next deadline = %v, want %s", snap.NextDeadlineAt, deadline.Add(24*time.Hour))
	}
}

// TestDeadlineStaleProviderResponseCannotRepromote covers the Codex
// lazy-reset caveat (spec section 10) generically: the provider still
// reports the expired window after the deadline; the account must stay
// demoted and keep retrying.
func TestDeadlineStaleProviderResponseCannotRepromote(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("codex", "a", deadline)
	env.addAccount("codex", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// Provider keeps reporting the expired window.
	env.clk.AdvanceTo(deadline)
	env.async.drain()
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "awaiting_new_window" {
		t.Errorf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}

	// Bounded retries: +5s, +30s, +2m, +5m, +15m, still stale.
	calls := env.codex.callCount(token("a"))
	env.clk.Advance(5 * time.Second)
	if got := env.codex.callCount(token("a")); got != calls+1 {
		t.Errorf("after +5s: %d calls, want %d", got, calls+1)
	}
	env.clk.Advance(16 * time.Minute) // covers +30s, +2m, +5m, +15m
	if got := env.codex.callCount(token("a")); got != calls+5 {
		t.Errorf("after retry window: %d calls, want %d", got, calls+5)
	}
	env.assertDesired(map[string]int{"b": 200, "a": 100})

	// The hourly reconciliation eventually confirms the new window.
	env.codex.setReset(token("a"), deadline.Add(7*24*time.Hour))
	env.clk.Advance(time.Hour)
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	row, _ = env.statusRow("a")
	if row.ResetState != "confirmed" {
		t.Errorf("account a reset state = %s, want confirmed via hourly reconcile", row.ResetState)
	}
}

func TestDeadlineNetworkFailureKeepsDemotedAndRetries(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()

	env.claude.setErr(token("a"), errFake("network unreachable"))
	env.clk.AdvanceTo(deadline)
	env.async.drain()
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "awaiting_new_window" {
		t.Errorf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}

	// A later bounded retry succeeds with a fresh future reset.
	env.claude.setReset(token("a"), deadline.Add(7*24*time.Hour))
	env.clk.Advance(5 * time.Second)
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	row, _ = env.statusRow("a")
	if row.ResetState != "confirmed" {
		t.Errorf("account a reset state = %s, want confirmed after retry", row.ResetState)
	}
}

// TestObsoleteTimerCannotOverwriteNewerObservation: once a newer observation
// moves the reset into the future, an expired-deadline event for the old
// timestamp must not demote the account or overwrite the new data.
func TestObsoleteTimerCannotOverwriteNewerObservation(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	oldDeadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", oldDeadline)
	env.addAccount("claude", "b", oldDeadline.Add(24*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	env.eng.mu.Lock()
	oldTimerSeq := env.eng.deadlineSeq
	env.eng.mu.Unlock()

	// Before the old deadline arrives, a refresh confirms a NEW future
	// window for A (e.g. the provider rolled early).
	newDeadline := oldDeadline.Add(7 * 24 * time.Hour)
	env.claude.setReset(token("a"), newDeadline)
	env.reconcile()
	env.eng.mu.Lock()
	newTimer := env.eng.deadlineTimer
	newTimerSeq := env.eng.deadlineSeq
	newTimerAt := env.eng.nextDeadlineAt
	env.eng.mu.Unlock()
	if newTimerSeq == oldTimerSeq {
		t.Fatalf("replacement deadline timer reused obsolete generation %d", oldTimerSeq)
	}

	// Even a spurious deadline event right now must not demote A or clear the
	// replacement timer: its current confirmed reset is in the future.
	env.eng.onDeadline(oldTimerSeq)
	env.eng.mu.Lock()
	if env.eng.deadlineTimer != newTimer || env.eng.deadlineSeq != newTimerSeq || !env.eng.nextDeadlineAt.Equal(newTimerAt) {
		env.eng.mu.Unlock()
		t.Fatalf("obsolete callback cleared or replaced the current deadline timer")
	}
	env.eng.mu.Unlock()
	env.assertDesired(map[string]int{"b": 200, "a": 100})

	// Advancing past the old deadline must not demote A either.
	env.clk.AdvanceTo(oldDeadline.Add(time.Second))
	env.async.drain()
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "confirmed" {
		t.Errorf("account a reset state = %s, want confirmed (obsolete deadline ignored)", row.ResetState)
	}
	if row.ResetAtUTC != newDeadline.UTC().Format(time.RFC3339Nano) {
		t.Errorf("account a reset = %s, want %s", row.ResetAtUTC, newDeadline.UTC().Format(time.RFC3339Nano))
	}
}

// TestObsoleteRetryCancelledAfterConfirmation: pending bounded retries are
// invalidated once a fresh confirmation lands through another path.
func TestObsoleteRetryCancelledAfterConfirmation(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.reconcile()

	env.claude.setErr(token("a"), errFake("network unreachable"))
	env.clk.AdvanceTo(deadline)
	env.async.drain() // immediate fetch fails; retries armed

	// A management refresh (full reconcile) confirms the fresh window.
	env.claude.setReset(token("a"), deadline.Add(7*24*time.Hour))
	env.reconcile()
	calls := env.claude.callCount(token("a"))

	// The previously armed +5s/+30s/... retries must not fire anymore.
	env.clk.Advance(20 * time.Minute)
	// (the hourly reconcile has not elapsed yet at +20m from the reconcile)
	if got := env.claude.callCount(token("a")); got != calls {
		t.Errorf("cancelled retries still fired: %d calls, want %d", got, calls)
	}
}

func TestHourlyReconciliationRuns(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(5))
	env.reconcile()
	calls := env.claude.callCount(token("a"))

	env.clk.Advance(time.Hour)
	if got := env.claude.callCount(token("a")); got != calls+1 {
		t.Errorf("after 1h: %d calls, want %d", got, calls+1)
	}
	env.clk.Advance(time.Hour)
	if got := env.claude.callCount(token("a")); got != calls+2 {
		t.Errorf("after 2h: %d calls, want %d", got, calls+2)
	}
}

func TestStaleFetchFailureBeforeDeadlineKeepsRanking(t *testing.T) {
	// Spec section 14: a failed refresh before a still-future reset retains
	// the last-known timestamp (marked stale) and does not reshuffle.
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	env.claude.setErr(token("a"), errFake("temporary outage"))
	env.clk.Advance(time.Hour) // hourly reconcile with failing refresh
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "stale" {
		t.Errorf("account a reset state = %s, want stale", row.ResetState)
	}
	if row.LastError == "" {
		t.Errorf("stale account has no last error in status")
	}
}

func TestQuiescedEngineFiresNoTimers(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", baseTime.Add(30*time.Minute))
	env.reconcile()
	calls := env.claude.callCount(token("a"))
	saves := env.host.saveCount()

	env.eng.Stop()
	env.clk.Advance(3 * time.Hour)
	env.async.drain()
	if got := env.claude.callCount(token("a")); got != calls {
		t.Errorf("stopped engine still fetched: %d calls, want %d", got, calls)
	}
	if got := env.host.saveCount(); got != saves {
		t.Errorf("stopped engine still saved: %d, want %d", got, saves)
	}
}
