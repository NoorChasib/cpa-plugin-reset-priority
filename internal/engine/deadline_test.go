package engine

import (
	"testing"
	"time"
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
