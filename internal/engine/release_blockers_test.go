package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

type sentinelCheckingProvider struct {
	host      *fakeHost
	authIndex string
	token     string
	resetAt   time.Time
	now       func() time.Time

	mu       sync.Mutex
	calls    int
	observed []int
}

func (p *sentinelCheckingProvider) ID() string { return "claude" }

func (p *sentinelCheckingProvider) FetchWeeklyReset(_ context.Context, creds providers.Credentials) (providers.Observation, error) {
	p.host.mu.Lock()
	var doc map[string]json.RawMessage
	err := json.Unmarshal(p.host.docs[p.authIndex], &doc)
	priority, ok := parsePriorityRaw(doc["priority"])
	p.host.mu.Unlock()
	if err != nil || !ok {
		return providers.Observation{}, fmt.Errorf("physical sentinel unavailable at fetch start")
	}
	if p.token == "" || creds.AccessToken == p.token {
		p.mu.Lock()
		p.calls++
		p.observed = append(p.observed, priority)
		p.mu.Unlock()
	}
	return providers.Observation{HasWeekly: true, ResetAt: p.resetAt, ObservedAt: p.now()}, nil
}

type controlledFetch struct {
	started chan struct{}
	release chan struct{}
	obs     providers.Observation
	err     error
}

type controlledProvider struct {
	id string

	mu    sync.Mutex
	next  int
	calls []*controlledFetch
}

func (p *controlledProvider) ID() string { return p.id }

func (p *controlledProvider) FetchWeeklyReset(ctx context.Context, _ providers.Credentials) (providers.Observation, error) {
	p.mu.Lock()
	if p.next >= len(p.calls) {
		p.mu.Unlock()
		return providers.Observation{}, fmt.Errorf("unexpected controlled fetch")
	}
	call := p.calls[p.next]
	p.next++
	p.mu.Unlock()
	close(call.started)
	select {
	case <-call.release:
		return call.obs, call.err
	case <-ctx.Done():
		return providers.Observation{}, ctx.Err()
	}
}

func newControlledFetch(obs providers.Observation) *controlledFetch {
	return &controlledFetch{started: make(chan struct{}), release: make(chan struct{}), obs: obs}
}

func TestDefinitiveQuarantinePersistsSentinelAndRecoverySentinelPrecedesFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	initialSaves := len(env.host.savesFor("a.json"))

	env.host.updateEntry("idx-a", func(entry *hostapi.AuthEntry) {
		entry.Status = "error"
		entry.StatusMessage = "quota service says unauthorized: invalid_grant"
		entry.Unavailable = true
	})
	env.reconcile()
	// Quarantine persists the sentinel to the physical document rather than
	// reporting it in status only.
	if got := len(env.host.savesFor("a.json")); got != initialSaves+1 {
		t.Fatalf("quarantine sentinel writes = %d, want %d", got, initialSaves+1)
	}
	if got, _ := env.host.docPriority(t, "idx-a"); got != 0 {
		t.Fatalf("physical priority after quarantine = %d, want sentinel 0", got)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthQuarantined) {
		t.Fatalf("health = %s, want quarantined", row.Health)
	}
	initialSaves = len(env.host.savesFor("a.json"))

	checker := &sentinelCheckingProvider{
		host: env.host, authIndex: "idx-a", resetAt: day(3), now: env.clk.Now,
	}
	env.eng.providers = map[string]providers.Provider{"claude": checker}
	recoverAccount(env, "idx-a")
	env.reconcile()

	checker.mu.Lock()
	calls := checker.calls
	observed := append([]int(nil), checker.observed...)
	checker.mu.Unlock()
	if calls != 1 || len(observed) != 1 || observed[0] != 0 {
		t.Fatalf("recovery fetch saw priorities %v across %d calls, want one call after physical sentinel 0", observed, calls)
	}
	// The sentinel was already persisted at quarantine time, so the recovery
	// pre-fetch sentinel verification re-reads the physical document, confirms
	// the sentinel, and performs NO redundant save. Only the promotion writes.
	saves := env.host.savesFor("a.json")
	if len(saves) != initialSaves+1 {
		t.Fatalf("recovery saves = %d total, want initial + promoted only (sentinel already physical)", len(saves))
	}
	if got, _ := parsePriorityRaw(saves[initialSaves].Doc["priority"]); got != 100 {
		t.Fatalf("post-recovery save priority = %d, want promoted 100", got)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthHealthy) {
		t.Fatalf("health after confirmed recovery = %s, want healthy", row.Health)
	}
}

func TestRecoveryFailsClosedUntilSentinelWriteConfirmed(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	// The quarantine-time sentinel write fails too, so the physical document
	// keeps its pre-failure rank and recovery genuinely has a sentinel to
	// establish. (When the quarantine write succeeded, the recovery
	// verification is a read-only no-op; that path is covered separately.)
	env.host.saveErr["a.json"] = errFake("sentinel save failed")
	env.host.updateEntry("idx-a", func(entry *hostapi.AuthEntry) {
		entry.Status = "error"
		entry.StatusMessage = "revoked credential"
	})
	env.reconcile()
	if got, _ := env.host.docPriority(t, "idx-a"); got != 100 {
		t.Fatalf("physical priority after failed quarantine sentinel write = %d, want the unchanged 100", got)
	}

	callsBefore := env.claude.callCount(token("a"))
	recoverAccount(env, "idx-a")
	env.reconcile()
	if got := env.claude.callCount(token("a")); got != callsBefore {
		t.Fatalf("provider fetched before sentinel confirmation: %d calls, want %d", got, callsBefore)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthRecovering) {
		t.Fatalf("health after failed sentinel write = %s, want recovering", row.Health)
	}

	delete(env.host.saveErr, "a.json")
	env.reconcile()
	if got := env.claude.callCount(token("a")); got != callsBefore+1 {
		t.Fatalf("provider calls after sentinel recovery = %d, want %d", got, callsBefore+1)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthHealthy) {
		t.Fatalf("health after successful sentinel confirmation = %s, want healthy", row.Health)
	}
}

func TestDryRunMaySimulateRecoveryWithZeroSaves(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(2))
	env.host.updateEntry("idx-a", func(entry *hostapi.AuthEntry) {
		entry.Status = "error"
		entry.StatusMessage = "unauthorized"
	})
	env.reconcile()
	recoverAccount(env, "idx-a")
	env.reconcile()

	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("dry-run recovery performed %d saves, want 0", got)
	}
	if row, _ := env.statusRow("a"); row.Health != string(HealthHealthy) || row.ResetState != string(ResetConfirmed) {
		t.Fatalf("dry-run recovery = %s/%s, want healthy/confirmed simulation", row.Health, row.ResetState)
	}
}

func TestLatestStartedFetchWinsEvenWithEqualObservationTimes(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	first := newControlledFetch(providers.Observation{HasWeekly: true, ResetAt: day(2), ObservedAt: baseTime})
	second := newControlledFetch(providers.Observation{HasWeekly: true, ResetAt: day(5), ObservedAt: baseTime})
	controlled := &controlledProvider{id: "claude", calls: []*controlledFetch{first, second}}
	env.eng.providers = map[string]providers.Provider{"claude": controlled}

	firstDone := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(firstDone)
	}()
	<-first.started
	secondDone := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(secondDone)
	}()
	<-second.started
	close(second.release)
	<-secondDone
	close(first.release)
	<-firstDone
	env.eng.flush(context.Background())

	row, _ := env.statusRow("a")
	if row.ResetAtUTC != day(5).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("older in-flight fetch overwrote latest-started result: reset=%s, want %s", row.ResetAtUTC, day(5).UTC().Format(time.RFC3339Nano))
	}
}

// TestValidWeeklyObservationSurvivesLaterStartedFailure verifies that merely
// starting a later retry does not invalidate a fresh weekly result already in
// flight when that later retry fails. Only a newer valid weekly observation may
// supersede the older valid one.
func TestValidWeeklyObservationSurvivesLaterStartedFailure(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	valid := newControlledFetch(providers.Observation{HasWeekly: true, ResetAt: day(5), ObservedAt: baseTime})
	failed := newControlledFetch(providers.Observation{})
	failed.err = errFake("later retry timed out")
	controlled := &controlledProvider{id: "claude", calls: []*controlledFetch{valid, failed}}
	env.eng.providers = map[string]providers.Provider{"claude": controlled}

	validDone := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(validDone)
	}()
	<-valid.started
	failedDone := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(failedDone)
	}()
	<-failed.started

	close(failed.release)
	<-failedDone
	close(valid.release)
	<-validDone
	env.eng.flush(context.Background())

	row, _ := env.statusRow("a")
	if row.ResetState != string(ResetConfirmed) || row.ResetAtUTC != day(5).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("later failed retry discarded valid in-flight weekly result: state=%s reset=%s", row.ResetState, row.ResetAtUTC)
	}
	if row.LastError != "" {
		t.Fatalf("valid weekly result did not clear later retry error: %q", row.LastError)
	}
}

func TestPreQuarantineFetchCannotOverwriteRecoveredState(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(2))
	env.reconcile()

	old := newControlledFetch(providers.Observation{HasWeekly: true, ResetAt: day(1), ObservedAt: day(1)})
	controlled := &controlledProvider{id: "claude", calls: []*controlledFetch{old}}
	env.eng.providers = map[string]providers.Provider{"claude": controlled}
	oldDone := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(oldDone)
	}()
	<-old.started

	env.host.updateEntry("idx-a", func(entry *hostapi.AuthEntry) {
		entry.Status = "error"
		entry.StatusMessage = "unauthorized"
	})
	env.reconcile()
	env.eng.providers = map[string]providers.Provider{"claude": env.claude}
	env.claude.setReset(token("a"), day(6))
	recoverAccount(env, "idx-a")
	env.reconcile()

	close(old.release)
	<-oldDone
	env.eng.flush(context.Background())
	row, _ := env.statusRow("a")
	if row.Health != string(HealthHealthy) || row.ResetAtUTC != day(6).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("pre-quarantine result overwrote recovered state: health=%s reset=%s", row.Health, row.ResetAtUTC)
	}
}

func TestPreDeadlineFetchCannotRepromoteAfterExpiry(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	// Suppress the timer callback to exercise the result-vs-deadline lock race:
	// the stale request itself must notice that it crossed the known deadline.
	env.eng.mu.Lock()
	if env.eng.deadlineTimer != nil {
		env.eng.deadlineTimer.Stop()
		env.eng.deadlineTimer = nil
	}
	env.eng.nextDeadlineAt = time.Time{}
	env.eng.mu.Unlock()

	old := newControlledFetch(providers.Observation{HasWeekly: true, ResetAt: day(7), ObservedAt: deadline.Add(time.Second)})
	controlled := &controlledProvider{id: "claude", calls: []*controlledFetch{old}}
	env.eng.providers = map[string]providers.Provider{"claude": controlled}
	done := make(chan struct{})
	go func() {
		env.eng.fetchOne(context.Background(), "idx-a")
		close(done)
	}()
	<-old.started
	env.clk.AdvanceTo(deadline)
	close(old.release)
	<-done
	env.eng.flush(context.Background())

	row, _ := env.statusRow("a")
	if row.ResetState != string(ResetAwaitingNewWindow) || row.ResetAtUTC != deadline.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("pre-deadline result repromoted account: state=%s reset=%s", row.ResetState, row.ResetAtUTC)
	}
	env.assertDesired(map[string]int{"b": 200, "a": 100})
}

func TestRosterFailsClosedForNonPhysicalAndInvalidOAuthEntries(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "valid", day(1))
	entries := []struct {
		entry hostapi.AuthEntry
		doc   map[string]any
	}{
		{
			entry: hostapi.AuthEntry{AuthIndex: "idx-no-source", Name: "no-source.json", Provider: "claude", Status: "active", Path: "/auth/no-source.json"},
			doc:   map[string]any{"type": "claude", "access_token": "tok-no-source"},
		},
		{
			entry: hostapi.AuthEntry{AuthIndex: "idx-no-path", Name: "no-path.json", Provider: "claude", Status: "active", Source: "file"},
			doc:   map[string]any{"type": "claude", "access_token": "tok-no-path"},
		},
		{
			entry: hostapi.AuthEntry{AuthIndex: "idx-runtime", Name: "runtime.json", Provider: "claude", Status: "active", Source: "file", Path: "/auth/runtime.json", RuntimeOnly: true},
			doc:   map[string]any{"type": "claude", "access_token": "tok-runtime"},
		},
		{
			// Audited CPA changes Source to "memory" while retaining the stale
			// path when a physical file disappears but its runtime entry remains.
			entry: hostapi.AuthEntry{AuthIndex: "idx-memory", Name: "memory.json", Provider: "claude", Status: "active", Source: "memory", Path: "/auth/memory.json"},
			doc:   map[string]any{"type": "claude", "access_token": "tok-memory"},
		},
		{
			entry: hostapi.AuthEntry{AuthIndex: "idx-invalid", Name: "invalid.json", Provider: "claude", Status: "active", Source: "file", Path: "/auth/invalid.json"},
			doc:   map[string]any{"type": "claude", "refresh_token": "refresh-only"},
		},
	}
	for _, fixture := range entries {
		env.host.setEntry(fixture.entry, fixture.doc)
	}
	env.reconcile()

	env.assertDesired(map[string]int{"valid": 100})
	for _, name := range []string{"no-source", "no-path", "runtime", "memory", "invalid"} {
		if _, ok := env.statusRow(name); ok {
			t.Errorf("invalid roster entry %s entered engine state", name)
		}
		if saves := env.host.savesFor(name + ".json"); len(saves) != 0 {
			t.Errorf("invalid roster entry %s was saved", name)
		}
	}
	if got := env.claude.callCount("tok-no-source") + env.claude.callCount("tok-no-path") + env.claude.callCount("tok-runtime") + env.claude.callCount("tok-memory"); got != 0 {
		t.Fatalf("nonphysical credentials reached provider fetch %d times", got)
	}
}

func TestEstablishedRosterReadFailureDefersContraction(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	savesBefore := env.host.saveCount()

	// A transient validation read for an established auth must not contract N
	// and rewrite every remaining account as if the auth were removed.
	env.host.getErr["idx-a"] = errFake("temporary host callback failure")
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	if got := env.host.saveCount(); got != savesBefore {
		t.Fatalf("transient validation failure caused %d new saves, want 0", got-savesBefore)
	}
	if env.eng.Status().RosterError == "" {
		t.Fatalf("transient validation failure was not surfaced as a roster error")
	}

	// A later authoritative list contraction still removes the account.
	delete(env.host.getErr, "idx-a")
	env.host.removeEntry("idx-a")
	env.reconcile()
	env.assertDesired(map[string]int{"b": 100})
	if env.eng.Status().RosterError != "" {
		t.Fatalf("roster error remained after successful reconciliation: %q", env.eng.Status().RosterError)
	}
}

func TestRosterDuplicatePhysicalNamesAreExcludedWithoutReadsFetchesOrWrites(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	for _, name := range []string{"one", "two"} {
		env.host.setEntry(hostapi.AuthEntry{
			AuthIndex: "idx-" + name,
			Name:      "duplicate.json",
			Provider:  "claude",
			Status:    "active",
			Source:    "file",
			Path:      "/auth/" + name + ".json",
		}, map[string]any{"type": "claude", "access_token": token(name)})
		env.claude.setReset(token(name), day(1))
	}
	env.reconcile()

	if got := len(env.eng.Status().Providers[0].Accounts); got != 0 {
		t.Fatalf("duplicate physical names produced %d managed accounts, want 0", got)
	}
	if got := len(env.host.getCalls); got != 0 {
		t.Fatalf("duplicate physical entries were read %d times, want 0", got)
	}
	if got := env.claude.callCount(token("one")) + env.claude.callCount(token("two")); got != 0 {
		t.Fatalf("duplicate credentials reached provider fetch %d times", got)
	}
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("duplicate physical entries were saved %d times", got)
	}
}

func TestFlushExpirySchedulesExactlyOneImmediateAndBoundedRetrySequence(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", day(2))
	env.reconcile()

	// Suppress the exact-deadline callback so this transition is discovered by
	// an ordinary flush, proving retry scheduling is not timer-callback-only.
	env.eng.mu.Lock()
	if env.eng.deadlineTimer != nil {
		env.eng.deadlineTimer.Stop()
		env.eng.deadlineTimer = nil
	}
	env.eng.nextDeadlineAt = time.Time{}
	env.eng.mu.Unlock()
	env.clk.Advance(31 * time.Minute)
	callsBefore := env.claude.callCount(token("a"))
	env.eng.flush(context.Background())

	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	if got := env.async.pending(); got != 1 {
		t.Fatalf("flush expiry queued %d immediate fetches, want 1", got)
	}
	// A repeated flush in the same awaiting state must not duplicate the ladder.
	env.eng.flush(context.Background())
	if got := env.async.pending(); got != 1 {
		t.Fatalf("repeated flush queued duplicate immediate fetches: %d pending", got)
	}

	env.async.drain()
	if got := env.claude.callCount(token("a")); got != callsBefore+1 {
		t.Fatalf("immediate passive fetch count = %d, want %d", got, callsBefore+1)
	}
	env.clk.Advance(5 * time.Second)
	env.clk.Advance(25 * time.Second)
	env.clk.Advance(14*time.Minute + 30*time.Second)
	if got := env.claude.callCount(token("a")); got != callsBefore+6 {
		t.Fatalf("immediate plus bounded retry calls = %d, want %d", got, callsBefore+6)
	}
}

func TestListPriorityZeroInvalidatesCachedNonzeroAndRepairsDrift(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	savesBefore := env.host.saveCount()

	env.host.mu.Lock()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(env.host.docs["idx-a"], &doc); err != nil {
		env.host.mu.Unlock()
		t.Fatal(err)
	}
	doc["priority"] = json.RawMessage("0")
	raw, _ := json.Marshal(doc)
	env.host.docs["idx-a"] = raw
	for i := range env.host.entries {
		if env.host.entries[i].AuthIndex == "idx-a" {
			env.host.entries[i].Priority = 0
		}
	}
	env.host.mu.Unlock()

	env.reconcile()
	env.assertPhysical(map[string]int{"a": 100})
	if got := env.host.saveCount(); got != savesBefore+1 {
		t.Fatalf("zero drift repair saves = %d, want %d", got, savesBefore+1)
	}
}

func TestDefinitiveAuthMarkersOverrideQuotaWordingButGenericMarkersYield(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		// Narrow definitive credential failures stay authoritative even
		// alongside quota/rate-limit wording.
		{name: "unauthorized wins", message: "quota exhausted; unauthorized credential", want: true},
		{name: "invalid grant wins", message: "429 cooldown after invalid_grant", want: true},
		// Generic auth-adjacent markers yield to benign quota/rate-limit/
		// cooldown wording: CPA's native availability handling owns those.
		{name: "revoked yields to rate-limit wording", message: "rate limit response says token revoked", want: false},
		{name: "reauth yields to quota wording", message: "quota unavailable; reauthentication required", want: false},
		// Generic markers without benign wording still quarantine.
		{name: "revoked alone", message: "token revoked", want: true},
		{name: "reauth alone", message: "reauthentication required", want: true},
		{name: "quota alone", message: "quota exceeded with HTTP 429 cooldown", want: false},
		{name: "rate limit alone", message: "rate limit overloaded", want: false},
		{name: "numeric substring 401", message: "request 14031 failed temporarily", want: false},
		{name: "numeric substring 403", message: "trace 40312 unavailable", want: false},
		{name: "standalone 401", message: "HTTP 401 from token endpoint", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := quarantineReasonFor(hostapi.AuthEntry{Status: "error", StatusMessage: test.message})
			if got != test.want {
				t.Fatalf("quarantineReasonFor(%q) = %v, want %v", test.message, got, test.want)
			}
		})
	}
}
