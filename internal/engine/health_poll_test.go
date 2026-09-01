package engine

import (
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

// quarantine drives account a into definitive quarantine.
func quarantineAccount(env *testEnv, authIndex string) {
	env.host.updateEntry(authIndex, func(entry *hostapi.AuthEntry) {
		entry.Status = "error"
		entry.StatusMessage = "unauthorized: invalid_grant"
	})
}

// recoverAccount makes CPA report the credential healthy again.
func recoverAccount(env *testEnv, authIndex string) {
	env.host.updateEntry(authIndex, func(entry *hostapi.AuthEntry) {
		entry.Status = "active"
		entry.StatusMessage = ""
		entry.Unavailable = false
		// A real refresh/re-auth updates runtime freshness or the physical file.
		// This timestamp must be newer than the plugin's sentinel save so the
		// audited save-side active rebuild cannot masquerade as recovery.
		entry.LastRefresh = env.clk.Now().Add(time.Nanosecond)
	})
}

// TestQuarantinePersistsSentinelToPhysicalDocument covers orchestrator
// decision M3 directly: the quarantine sentinel reaches the physical auth
// JSON, every unrelated field survives verbatim, and the rest of the pool is
// ranked as if the quarantined account did not exist.
func TestQuarantinePersistsSentinelToPhysicalDocument(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	quarantineAccount(env, "idx-a")
	env.reconcile()

	env.assertDesired(map[string]int{"a": 0, "b": 100})
	env.assertPhysical(map[string]int{"a": 0, "b": 100})

	saves := env.host.savesFor("a.json")
	if len(saves) == 0 {
		t.Fatalf("quarantine did not persist the sentinel")
	}
	sentinel := saves[len(saves)-1].Doc
	if got, ok := parsePriorityRaw(sentinel["priority"]); !ok || got != 0 {
		t.Fatalf("persisted quarantine priority = %s, want 0", sentinel["priority"])
	}
	// Field preservation still holds on the quarantine write path.
	for _, key := range []string{"type", "access_token", "refresh_token", "email"} {
		if _, ok := sentinel[key]; !ok {
			t.Errorf("quarantine sentinel write dropped field %q", key)
		}
	}
	if len(sentinel) != 5 {
		t.Errorf("quarantine sentinel write changed the field set: %d fields, want 5", len(sentinel))
	}
}

// TestQuarantineSentinelDryRunPerformsNoSave keeps dry-run authoritative over
// the M3 write decision.
func TestQuarantineSentinelDryRunPerformsNoSave(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))
	quarantineAccount(env, "idx-a")
	env.reconcile()

	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("dry-run quarantine performed %d saves, want 0", got)
	}
	env.assertDesired(map[string]int{"a": 0})
}

// TestQuarantineSentinelWrittenOnceWithoutChurn is the no-write-churn
// guarantee: once the sentinel is physical, repeated reconciles and repeated
// health polls re-verify it by read only and never save again.
func TestQuarantineSentinelWrittenOnceWithoutChurn(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	rankWrites := len(env.host.savesFor("a.json"))

	quarantineAccount(env, "idx-a")
	env.reconcile()
	savesAfterQuarantine := env.host.saveCount()
	fetchesAfterQuarantine := env.claude.callCount(token("a"))
	if got := len(env.host.savesFor("a.json")) - rankWrites; got != 1 {
		t.Fatalf("quarantine sentinel writes = %d, want exactly 1", got)
	}

	// The fake mirrors the audited save-side runtime rebuild, so CPA now appears
	// active solely because this plugin wrote the sentinel. Repeated roster reads
	// must not treat that plugin-created state as genuine recovery.
	// Repeated full reconciles converge with zero further saves for any
	// account, even though the ambiguous omitempty list priority (0) forces a
	// read-and-verify pass for the quarantined account each time.
	for i := 0; i < 5; i++ {
		env.reconcile()
	}
	if got := env.host.saveCount(); got != savesAfterQuarantine {
		t.Fatalf("repeated reconciles caused %d extra saves, want 0", got-savesAfterQuarantine)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthQuarantined) {
		t.Fatalf("save-side active rebuild changed health to %s, want quarantined", row.Health)
	}
	if got := env.claude.callCount(token("a")); got != fetchesAfterQuarantine {
		t.Fatalf("save-side active rebuild caused %d recovery fetches, want 0", got-fetchesAfterQuarantine)
	}

	// Repeated health polls in a steady quarantined state are read-only: they
	// neither save nor trigger a reconcile (which would relist the roster).
	listsBefore := env.host.listCount()
	runtimeBefore := env.host.runtimeCallCount()
	env.clk.Advance(5 * healthPollInterval)
	if got := env.host.saveCount(); got != savesAfterQuarantine {
		t.Fatalf("health polls caused %d saves, want 0", got-savesAfterQuarantine)
	}
	if got := env.host.listCount(); got != listsBefore {
		t.Fatalf("steady-state health polls triggered %d reconciles, want 0", got-listsBefore)
	}
	if got := env.host.runtimeCallCount() - runtimeBefore; got < 5 {
		t.Fatalf("health polls ran %d runtime probes over 5 intervals, want at least 5", got)
	}
	env.assertPhysical(map[string]int{"a": 0, "b": 100})
}

// TestHealthPollDetectsRuntimeSelfRecoveryPromptly is the latency guarantee:
// CPA self-recovery (a successful background refresh, or an operator
// re-authenticating) is picked up on the short poll cadence rather than
// waiting for the hourly reconciliation.
func TestHealthPollDetectsRuntimeSelfRecoveryPromptly(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()

	quarantineAccount(env, "idx-a")
	env.reconcile()
	if row, _ := env.statusRow("a"); row.Health != string(HealthQuarantined) {
		t.Fatalf("health = %s, want quarantined", row.Health)
	}
	env.assertPhysical(map[string]int{"a": 0, "b": 100})

	// CPA repairs the credential on its own. Nothing else happens: no
	// management refresh, and the hourly reconcile timer is far away.
	env.claude.setReset(token("a"), day(3))
	recoverAccount(env, "idx-a")

	// One poll interval is enough to notice and drive a full reconciliation.
	env.clk.Advance(healthPollInterval)

	row, _ := env.statusRow("a")
	if row.Health != string(HealthHealthy) || row.ResetState != string(ResetConfirmed) {
		t.Fatalf("self-recovery not detected within one poll interval: health=%s state=%s", row.Health, row.ResetState)
	}
	// b resets sooner than a's fresh post-recovery window, so a takes the floor.
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})

	if next := env.eng.Status().NextReconcileAt; next == nil || next.Sub(baseTime) > time.Hour+healthPollInterval {
		t.Fatalf("hourly reconcile scheduling was disturbed: %v", next)
	}
}

// TestHealthPollStaysSentinelUntilFreshPostRecoveryObservation proves the poll
// only accelerates DETECTION. Promotion still requires a fresh future weekly
// reset observed by a request begun after recovery.
func TestHealthPollStaysSentinelUntilFreshPostRecoveryObservation(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	staleReset := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", staleReset)
	env.addAccount("claude", "b", day(2))
	env.reconcile()

	quarantineAccount(env, "idx-a")
	env.claude.setErr(token("a"), errFake("unauthorized"))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 0, "b": 100})

	// Time passes beyond the pre-failure reset, then CPA reports health again
	// while the provider still returns only the EXPIRED pre-failure window.
	env.clk.Advance(2 * time.Hour)
	env.claude.setReset(token("a"), staleReset)
	recoverAccount(env, "idx-a")

	env.clk.Advance(healthPollInterval)
	row, _ := env.statusRow("a")
	if row.Health != string(HealthRecovering) {
		t.Fatalf("health after poll-detected recovery with a stale window = %s, want recovering", row.Health)
	}
	env.assertDesired(map[string]int{"a": 0, "b": 100})
	env.assertPhysical(map[string]int{"a": 0, "b": 100})

	// Only a fresh FUTURE weekly observation promotes, and it ranks normally.
	env.claude.setReset(token("a"), day(6))
	env.reconcile()
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
}

// TestHealthPollDetectsRegressionWhileRecovering covers the other direction:
// a recovering account that CPA re-quarantines is noticed on the same cadence.
func TestHealthPollDetectsRegressionWhileRecovering(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	quarantineAccount(env, "idx-a")
	env.reconcile()

	// Recovery is reported but the provider keeps failing, so a stays
	// recovering with the health poll armed.
	env.claude.setErr(token("a"), errFake("usage endpoint unavailable"))
	recoverAccount(env, "idx-a")
	env.reconcile()
	if row, _ := env.statusRow("a"); row.Health != string(HealthRecovering) {
		t.Fatalf("health = %s, want recovering", row.Health)
	}

	quarantineAccount(env, "idx-a")
	env.clk.Advance(healthPollInterval)
	if row, _ := env.statusRow("a"); row.Health != string(HealthQuarantined) {
		t.Fatalf("regression to quarantine not detected within one poll interval: %s", row.Health)
	}
}

// TestHealthPollDisarmedWhenAllAccountsHealthy keeps the probe off the hot
// path: a fully healthy pool performs no runtime probes at all.
func TestHealthPollDisarmedWhenAllAccountsHealthy(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()

	before := env.host.runtimeCallCount()
	env.clk.Advance(10 * healthPollInterval)
	if got := env.host.runtimeCallCount(); got != before {
		t.Fatalf("healthy pool performed %d runtime probes, want 0", got-before)
	}

	// Arming and disarming round-trips cleanly.
	quarantineAccount(env, "idx-a")
	env.reconcile()
	env.clk.Advance(healthPollInterval)
	if env.host.runtimeCallCount() == before {
		t.Fatalf("quarantine did not arm the health poll")
	}

	env.claude.setReset(token("a"), day(3))
	recoverAccount(env, "idx-a")
	env.clk.Advance(healthPollInterval)
	if row, _ := env.statusRow("a"); row.Health != string(HealthHealthy) {
		t.Fatalf("health = %s, want healthy", row.Health)
	}
	after := env.host.runtimeCallCount()
	env.clk.Advance(10 * healthPollInterval)
	if got := env.host.runtimeCallCount(); got != after {
		t.Fatalf("poll kept probing after full recovery: %d extra probes", got-after)
	}
}

// TestHealthPollProbeErrorsNeverTransitionHealth: an unreadable runtime entry
// (unknown index, disabled auth whose file vanished, transient host failure)
// carries no health information and must not move the account or stop the
// poll.
func TestHealthPollProbeErrorsNeverTransitionHealth(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	quarantineAccount(env, "idx-a")
	env.reconcile()
	savesBefore := env.host.saveCount()
	listsBefore := env.host.listCount()

	env.host.mu.Lock()
	env.host.runtimeErr["idx-a"] = errFake("auth not found for auth_index idx-a")
	env.host.mu.Unlock()
	env.clk.Advance(3 * healthPollInterval)

	if row, _ := env.statusRow("a"); row.Health != string(HealthQuarantined) {
		t.Fatalf("probe error changed health to %s, want quarantined", row.Health)
	}
	if got := env.host.saveCount(); got != savesBefore {
		t.Fatalf("probe error caused %d saves, want 0", got-savesBefore)
	}
	if got := env.host.listCount(); got != listsBefore {
		t.Fatalf("probe error triggered %d reconciles, want 0", got-listsBefore)
	}

	// The poll is still armed, so a later real recovery is still prompt.
	env.host.mu.Lock()
	delete(env.host.runtimeErr, "idx-a")
	env.host.mu.Unlock()
	env.claude.setReset(token("a"), day(4))
	recoverAccount(env, "idx-a")
	env.clk.Advance(healthPollInterval)
	if row, _ := env.statusRow("a"); row.Health != string(HealthHealthy) {
		t.Fatalf("health after probe errors then real recovery = %s, want healthy", row.Health)
	}
}

// TestHealthPollStopsAfterEngineStop verifies the poll participates in
// quiesce/shutdown like the other timers.
func TestHealthPollStopsAfterEngineStop(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	quarantineAccount(env, "idx-a")
	env.reconcile()

	env.eng.Stop()
	before := env.host.runtimeCallCount()
	env.clk.Advance(10 * healthPollInterval)
	if got := env.host.runtimeCallCount(); got != before {
		t.Fatalf("stopped engine performed %d runtime probes, want 0", got-before)
	}
}
