package engine

import (
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

func day(n int) time.Time { return baseTime.Add(time.Duration(n) * 24 * time.Hour) }

func TestDiscoveryOneClaudeAccount(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 100})
	env.assertPhysical(map[string]int{"a": 100})
}

func TestDiscoveryOneCodexAccount(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("codex", "c", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"c": 100})
	env.assertPhysical(map[string]int{"c": 100})
}

func TestDiscoveryMixedProvidersRankedIndependently(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "ca", day(1))
	env.addAccount("claude", "cb", day(2))
	env.addAccount("claude", "cc", day(3))
	env.addAccount("codex", "xa", day(1))
	env.addAccount("codex", "xb", day(5))
	env.reconcile()
	env.assertDesired(map[string]int{
		"ca": 300, "cb": 200, "cc": 100,
		"xa": 200, "xb": 100,
	})
	env.assertPhysical(map[string]int{
		"ca": 300, "cb": 200, "cc": 100,
		"xa": 200, "xb": 100,
	})
}

func TestDiscoveryNewAccountAppearsOnNextReconcile(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(2))
	env.addAccount("claude", "b", day(3))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// New account with the earliest reset is discovered and inserted first.
	env.addAccount("claude", "e", day(1))
	env.reconcile()
	env.assertDesired(map[string]int{"e": 300, "a": 200, "b": 100})
}

func TestDiscoveryInsertLatestAndMiddle(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(4))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// Latest insertion takes the floor.
	env.addAccount("claude", "z", day(9))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 300, "b": 200, "z": 100})

	// Middle insertion lands between.
	env.addAccount("claude", "m", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 400, "m": 300, "b": 200, "z": 100})
}

func TestDiscoveryRemovalContractsPriorities(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.addAccount("claude", "c", day(3))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 300, "b": 200, "c": 100})

	env.host.removeEntry("idx-a")
	env.reconcile()
	env.assertDesired(map[string]int{"b": 200, "c": 100})
	if _, ok := env.statusRow("a"); ok {
		t.Errorf("removed account a still appears in status")
	}
}

func TestDiscoveryDisabledAccountQuarantinedAtZeroAndNeverSaved(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Disabled = true
		e.Status = "disabled"
	})
	env.reconcile()

	// Quarantined at the sentinel, excluded from the rank count.
	env.assertDesired(map[string]int{"a": 0, "b": 100})
	row, _ := env.statusRow("a")
	if row.Health != "quarantined" || row.QuarantineReason != "disabled" {
		t.Errorf("account a health = %s (%s), want quarantined (disabled)", row.Health, row.QuarantineReason)
	}
	// host.auth.save on a disabled credential could re-enable it (upstream
	// FileTokenStore rewrites metadata disabled from the simplified runtime
	// record); the plugin must never write disabled credentials.
	if saves := env.host.savesFor("a.json"); len(saves) != 0 {
		t.Errorf("disabled account a was saved %d times, want 0", len(saves))
	}
}

func TestDiscoveryReauthRequiredQuarantined(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.addAccount("claude", "c", day(3))
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "unauthorized: invalid_grant, reauthentication required"
		e.Unavailable = true
	})
	env.reconcile()
	env.assertDesired(map[string]int{"a": 0, "b": 200, "c": 100})
	row, _ := env.statusRow("a")
	if row.Health != "quarantined" || row.QuarantineReason != "reauth_required" {
		t.Errorf("account a = %s (%s), want quarantined (reauth_required)", row.Health, row.QuarantineReason)
	}
}

func TestDiscoveryQuotaCooldownDoesNotQuarantineOrRerank(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// Ordinary quota cooldown / 429: unavailable but NOT a health failure.
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Unavailable = true
		e.StatusMessage = "429 rate limit exceeded, cooling down"
	})
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	row, _ := env.statusRow("a")
	if row.Health != "healthy" {
		t.Errorf("account a health = %s, want healthy during quota cooldown", row.Health)
	}

	// Even a StatusError with quota wording must not quarantine.
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "quota exceeded for the current window"
	})
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
}

func TestDiscoveryOneHealthyPlusOneQuarantinedLeavesFloor(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "healthy", day(2))
	env.addAccount("claude", "down", day(1)) // would rank first if not down
	env.host.updateEntry("idx-down", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "unauthorized"
	})
	env.reconcile()
	env.assertDesired(map[string]int{"healthy": 100, "down": 0})
}

func TestRecoveryRequiresFreshFutureObservation(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// a goes down.
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "unauthorized"
	})
	env.reconcile()
	env.assertDesired(map[string]int{"a": 0, "b": 100})

	// CPA reports it healthy again, but the quota endpoint still fails:
	// the account must stay out of the pool (recovering, priority 0).
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "active"
		e.StatusMessage = ""
	})
	env.claude.setErr(token("a"), errFake("usage endpoint unavailable"))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 0, "b": 100})
	row, _ := env.statusRow("a")
	if row.Health != "recovering" {
		t.Fatalf("account a health = %s, want recovering", row.Health)
	}

	// A fresh future observation re-inserts it at the correct rank.
	env.claude.setReset(token("a"), day(1))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	row, _ = env.statusRow("a")
	if row.Health != "healthy" || row.ResetState != "confirmed" {
		t.Errorf("recovered account a = %s/%s, want healthy/confirmed", row.Health, row.ResetState)
	}
}

func TestRecoveryStalePreFailureResetCannotReenter(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	staleReset := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", staleReset)
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// a goes down; time passes beyond its old reset.
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "unauthorized"
	})
	env.claude.setErr(token("a"), errFake("unauthorized"))
	env.reconcile()
	env.clk.Advance(2 * time.Hour) // stale reset is now in the past
	env.async.drain()

	// Recovery: CPA healthy again, but the provider still reports the
	// EXPIRED pre-failure reset. The account must not re-enter the pool.
	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "active"
		e.StatusMessage = ""
	})
	env.claude.setReset(token("a"), staleReset)
	env.reconcile()
	env.assertDesired(map[string]int{"a": 0, "b": 100})
	row, _ := env.statusRow("a")
	if row.Health != "recovering" {
		t.Errorf("account a health = %s, want recovering (stale pre-failure reset)", row.Health)
	}

	// Only a fresh FUTURE reset re-enters, at the correct rank (latest).
	env.claude.setReset(token("a"), day(6))
	env.reconcile()
	env.assertDesired(map[string]int{"b": 200, "a": 100})
}

func TestRecoveryRetriesFollowBoundedSchedule(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "error"
		e.StatusMessage = "unauthorized"
	})
	env.reconcile()

	env.host.updateEntry("idx-a", func(e *hostapi.AuthEntry) {
		e.Status = "active"
		e.StatusMessage = ""
	})
	env.claude.setErr(token("a"), errFake("still failing"))
	env.reconcile()
	calls := env.claude.callCount(token("a"))

	// Bounded retries at +5s, +30s, +2m, +5m, +15m after the reconcile.
	env.clk.Advance(5 * time.Second)
	if got := env.claude.callCount(token("a")); got != calls+1 {
		t.Errorf("after +5s: %d calls, want %d", got, calls+1)
	}
	env.clk.Advance(25 * time.Second) // +30s
	if got := env.claude.callCount(token("a")); got != calls+2 {
		t.Errorf("after +30s: %d calls, want %d", got, calls+2)
	}
	// Recovery succeeds at the +2m attempt; later attempts are cancelled.
	env.claude.setReset(token("a"), day(3))
	env.clk.Advance(90 * time.Second) // +2m
	if got := env.claude.callCount(token("a")); got != calls+3 {
		t.Errorf("after +2m: %d calls, want %d", got, calls+3)
	}
	env.assertDesired(map[string]int{"a": 100})
	env.clk.Advance(20 * time.Minute) // +5m and +15m must not fire
	if got := env.claude.callCount(token("a")); got != calls+3 {
		t.Errorf("after recovery: %d calls, want %d (no further retries)", got, calls+3)
	}
}

func TestDiscoveryUnrelatedProviderUntouched(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-gemini",
		Name:      "gemini.json",
		Provider:  "gemini",
		Status:    "active",
	}, map[string]any{"type": "gemini", "access_token": "tok-gemini", "priority": 7})
	env.reconcile()

	if _, ok := env.statusRow("gemini"); ok {
		t.Errorf("unrelated gemini auth appears in status")
	}
	if saves := env.host.savesFor("gemini.json"); len(saves) != 0 {
		t.Errorf("unrelated gemini auth was saved")
	}
	if got, _ := env.host.docPriority(t, "idx-gemini"); got != 7 {
		t.Errorf("gemini priority changed to %d, want 7", got)
	}
	// Its credential JSON must never even be read.
	for _, idx := range env.host.getCalls {
		if idx == "idx-gemini" {
			t.Errorf("unrelated gemini auth JSON was read")
		}
	}
}

func TestDiscoveryRuntimeOnlyEntriesSkipped(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex:   "idx-rt",
		Name:        "runtime-only",
		Provider:    "claude",
		Status:      "active",
		RuntimeOnly: true,
		Source:      "file",
		Path:        "/auth/runtime-only",
	}, map[string]any{"type": "claude"})
	env.reconcile()
	env.assertDesired(map[string]int{"a": 100})
	if _, ok := env.statusRow("runtime-only"); ok {
		t.Errorf("runtime-only auth appears in status")
	}
}

func TestDiscoveryProviderFilteringByConfig(t *testing.T) {
	cfg := defaultConfig()
	cfg.ManageCodex = false
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))
	env.addAccount("codex", "x", day(1))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 100})
	if _, ok := env.statusRow("x"); ok {
		t.Errorf("codex account managed although manage-codex=false")
	}
	if saves := env.host.savesFor("x.json"); len(saves) != 0 {
		t.Errorf("unmanaged codex account was saved")
	}
}

func TestDiscoveryUnknownResetSortsLastAndCounts(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.addAccount("claude", "c", day(3))
	env.addAccount("claude", "d", time.Time{}) // provider reports no weekly window
	env.reconcile()
	env.assertDesired(map[string]int{"a": 400, "b": 300, "c": 200, "d": 100})
	row, _ := env.statusRow("d")
	if row.ResetState != "unknown" {
		t.Errorf("account d reset state = %s, want unknown", row.ResetState)
	}
}

func TestRosterListFailureKeepsState(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	env.host.mu.Lock()
	env.host.listErr = errFake("management api down")
	env.host.mu.Unlock()
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	if env.eng.Status().RosterError == "" {
		t.Errorf("roster error not surfaced in status")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
