// Package engine implements discovery, weekly-reset observation, quarantine
// and recovery, deterministic ranking, exact deadline timers, bounded
// retries, and priority writeback for the reset-priority plugin.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/sanitize"
)

// maxConcurrentFetches bounds concurrent provider quota requests.
const maxConcurrentFetches = 4

// resetRetryDelays is the bounded reset-specific / recovery retry schedule
// (offsets from the triggering event). After the last attempt the normal
// reconciliation interval takes over (spec sections 6A.5 and 10).
var resetRetryDelays = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

// HostAuth is the subset of host callbacks the engine needs.
type HostAuth interface {
	AuthList(ctx context.Context) ([]hostapi.AuthEntry, error)
	AuthGet(ctx context.Context, authIndex string) (hostapi.AuthGetResponse, error)
	AuthSave(ctx context.Context, name string, doc json.RawMessage) error
}

// Logger receives sanitized log lines. Production loggers cross the native
// ABI (host.log) and may block or re-enter plugin code that acquires the
// engine state lock, so the engine must never invoke a Logger while holding
// Engine.mu.
type Logger func(level, message string)

// Deps wires the engine's testable dependencies.
type Deps struct {
	Clock     clock.Clock
	Host      HostAuth
	Providers map[string]providers.Provider
	Log       Logger
	// RunAsync runs deferred work. Production uses `go f()`; tests may run
	// inline or capture for step-by-step execution.
	RunAsync func(func())
}

// Engine owns all mutable plugin state.
type Engine struct {
	mu sync.Mutex

	workMu   sync.Mutex
	workWG   sync.WaitGroup
	terminal bool
	// reconcileMu serializes FULL reconciliation passes (startup, interval,
	// management refresh) so overlapping invocations cannot interleave
	// roster application and write planning. Exact-deadline demotion
	// (onDeadline) and bounded retries deliberately do NOT take this mutex:
	// a slow provider fetch inside a reconcile must never delay the
	// synchronous local demotion at a reset deadline.
	reconcileMu sync.Mutex
	// writeMu serializes performWrites passes so overlapping flushes cannot
	// interleave read-mutate-save cycles for the same account. It only
	// guards local host get/save callbacks, never provider network work.
	writeMu sync.Mutex

	cfg       config.Config
	clk       clock.Clock
	host      HostAuth
	providers map[string]providers.Provider
	logf      Logger
	runAsync  func(func())
	fetchSem  chan struct{}

	accounts map[string]*account // by authIndex
	stopped  bool

	reconcileTimer  clock.Timer
	nextReconcileAt time.Time
	deadlineTimer   clock.Timer
	nextDeadlineAt  time.Time
	deadlineSeq     uint64

	lastRosterError string
	status          Snapshot
}

// New builds an engine. Missing deps get production defaults.
func New(cfg config.Config, deps Deps) *Engine {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Log == nil {
		deps.Log = func(string, string) {}
	}
	if deps.RunAsync == nil {
		deps.RunAsync = func(f func()) { go f() }
	}
	e := &Engine{
		cfg:       cfg,
		clk:       deps.Clock,
		host:      deps.Host,
		providers: deps.Providers,
		logf:      deps.Log,
		fetchSem:  make(chan struct{}, maxConcurrentFetches),
		accounts:  make(map[string]*account),
		// A disabled registration builds an inspectable engine but must not allow
		// management refresh to activate reconciliation behind the config gate.
		stopped: !cfg.Enabled,
	}
	runAsync := deps.RunAsync
	e.runAsync = func(f func()) {
		if !e.reserveWork() {
			return
		}
		runAsync(func() {
			defer e.workWG.Done()
			f()
		})
	}
	e.mu.Lock()
	e.publishStatusLocked(e.clk.Now())
	e.mu.Unlock()
	return e
}

// Start triggers the startup reconciliation (spec section 7 trigger 1).
func (e *Engine) Start() {
	e.runAsync(func() { e.Reconcile(context.Background(), "startup") })
}

// Stop quiesces the engine: all timers and retries are cancelled and no new
// work is accepted. State is retained for status display. Taking writeMu first
// drains any priority write already in progress and prevents a stale reconcile
// plan from beginning another write after Stop returns.
func (e *Engine) Stop() {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	if e.reconcileTimer != nil {
		e.reconcileTimer.Stop()
		e.reconcileTimer = nil
	}
	e.nextReconcileAt = time.Time{}
	e.deadlineSeq++
	if e.deadlineTimer != nil {
		e.deadlineTimer.Stop()
		e.deadlineTimer = nil
	}
	e.nextDeadlineAt = time.Time{}
	for _, acct := range e.accounts {
		acct.cancelRetriesLocked()
	}
	e.publishStatusLocked(e.clk.Now())
}

func (e *Engine) reserveWork() bool {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.terminal {
		return false
	}
	e.workWG.Add(1)
	return true
}

// Shutdown terminally stops the engine and waits for all work already queued
// or running through the engine's async and timer schedulers to finish.
func (e *Engine) Shutdown() {
	if e == nil {
		return
	}
	e.workMu.Lock()
	e.terminal = true
	e.workMu.Unlock()
	e.Stop()
	e.workWG.Wait()
}

// Reconfigure swaps configuration (plugin.reconfigure) and triggers a fresh
// reconciliation when enabled.
func (e *Engine) Reconfigure(cfg config.Config) {
	e.mu.Lock()
	e.cfg = cfg
	if cfg.Enabled {
		e.stopped = false
	}
	e.mu.Unlock()
	if !cfg.Enabled {
		e.Stop()
		return
	}
	e.runAsync(func() { e.Reconcile(context.Background(), "reconfigure") })
}

// Status returns the latest published snapshot.
func (e *Engine) Status() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Reconcile performs one full reconciliation pass: roster discovery,
// bounded-concurrency weekly-reset refresh, ranking, writeback, timer
// rescheduling, and status publication. It is synchronous, and concurrent
// invocations (startup, hourly, management refresh) are serialized.
func (e *Engine) Reconcile(ctx context.Context, trigger string) {
	if !e.reserveWork() {
		return
	}
	defer e.workWG.Done()

	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	entries, errRoster := e.host.AuthList(ctx)
	var validated []hostapi.AuthEntry
	if errRoster == nil {
		validated, errRoster = e.validateRoster(ctx, entries)
	}

	var fetchTargets []string
	var recoveryNeedsFlush bool
	if errRoster != nil {
		rosterError := sanitize.Error(errRoster)
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return
		}
		// A list or validation read was incomplete. Keep the prior roster as one
		// transaction rather than contracting the rank pool because one established
		// physical auth was temporarily unreadable.
		e.lastRosterError = rosterError
		e.mu.Unlock()
		// Cross-ABI host logging must never run under Engine.mu (see Logger).
		e.logf("warn", "reset-priority: auth roster reconciliation deferred: "+rosterError)
	} else {
		// Roster health transitions are serialized with writeback. Once CPA has
		// definitively quarantined an auth, no later host.auth.save may begin.
		e.writeMu.Lock()
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			e.writeMu.Unlock()
			return
		}
		e.lastRosterError = ""
		fetchTargets, recoveryNeedsFlush = e.applyRosterLocked(validated, e.clk.Now())
		e.mu.Unlock()
		e.writeMu.Unlock()
	}

	if recoveryNeedsFlush {
		// A quarantined -> recovering transition must synchronously verify or
		// write the sentinel before any recovery quota request can begin.
		e.flush(ctx)
	}
	e.fetchAll(ctx, fetchTargets)

	e.flush(ctx)

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.scheduleRecoveryRetriesLocked()
	e.scheduleNextReconcileLocked()
	e.publishStatusLocked(e.clk.Now())
	e.mu.Unlock()
	_ = trigger
}

// validateRoster fails closed before an auth can enter engine state. Only
// unique, physical file-backed entries whose latest document has the expected
// OAuth credential shape are returned. Extracted credentials stay on this
// stack only long enough to validate and are never cached.
//
// A new unreadable candidate is skipped. If an already-managed physical auth
// becomes temporarily unreadable or its get response is inconsistent, the
// whole roster transaction is deferred: contracting N because of one transient
// read would churn every remaining priority. A readable document that no longer
// has an OAuth shape is conclusive and is removed from management.
func (e *Engine) validateRoster(ctx context.Context, entries []hostapi.AuthEntry) ([]hostapi.AuthEntry, error) {
	e.mu.Lock()
	managedProviders := make(map[string]bool)
	for _, provider := range e.cfg.ManagedProviders() {
		managedProviders[provider] = true
	}
	known := make(map[string]bool, len(e.accounts))
	for authIndex := range e.accounts {
		known[authIndex] = true
	}
	e.mu.Unlock()

	type candidate struct {
		entry    hostapi.AuthEntry
		provider string
		nameKey  string
		pathKey  string
	}
	var candidates []candidate
	authIndexCounts := make(map[string]int)
	nameCounts := make(map[string]int)
	pathCounts := make(map[string]int)
	for _, entry := range entries {
		provider := normalizeProvider(entry)
		if !managedProviders[provider] {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		path := strings.TrimSpace(entry.Path)
		// Upstream changes Source from "file" to "memory" when the physical
		// file has disappeared but a runtime record briefly remains. Excluding it
		// here contracts the physical pool without misclassifying it as OAuth
		// credential health.
		if entry.RuntimeOnly || !strings.EqualFold(strings.TrimSpace(entry.Source), "file") || path == "" {
			continue
		}
		if entry.AuthIndex == "" || name == "" {
			continue
		}
		candidates = append(candidates, candidate{entry: entry, provider: provider, nameKey: name, pathKey: path})
		authIndexCounts[entry.AuthIndex]++
		nameCounts[name]++
		pathCounts[path]++
	}

	validated := make([]hostapi.AuthEntry, 0, len(candidates))
	for _, candidate := range candidates {
		entry := candidate.entry
		if authIndexCounts[entry.AuthIndex] != 1 || nameCounts[candidate.nameKey] != 1 || pathCounts[candidate.pathKey] != 1 {
			e.logf("warn", "reset-priority: skipping ambiguous duplicate physical auth entry")
			continue
		}
		got, errGet := e.host.AuthGet(ctx, entry.AuthIndex)
		if errGet != nil {
			message := sanitize.Error(errGet)
			if known[entry.AuthIndex] {
				return nil, fmt.Errorf("established physical auth validation read failed: %s", message)
			}
			e.logf("warn", "reset-priority: skipping unreadable new physical auth: "+message)
			continue
		}
		if got.AuthIndex != "" && got.AuthIndex != entry.AuthIndex {
			if known[entry.AuthIndex] {
				return nil, fmt.Errorf("established physical auth validation returned a mismatched auth index")
			}
			e.logf("warn", "reset-priority: skipping new physical auth with mismatched auth index")
			continue
		}
		if got.Name != "" && got.Name != entry.Name {
			if known[entry.AuthIndex] {
				return nil, fmt.Errorf("established physical auth validation returned a mismatched file name")
			}
			e.logf("warn", "reset-priority: skipping new physical auth with mismatched file name")
			continue
		}
		if _, errCreds := providers.ExtractCredentials(candidate.provider, got.JSON); errCreds != nil {
			e.logf("warn", "reset-priority: skipping non-OAuth physical auth: "+sanitize.Error(errCreds))
			continue
		}
		validated = append(validated, entry)
	}
	return validated, nil
}

// applyRosterLocked reconciles validated auth entries into account state and
// returns the auth indexes to refresh plus whether recovery needs a synchronous
// pre-fetch sentinel flush.
func (e *Engine) applyRosterLocked(entries []hostapi.AuthEntry, now time.Time) ([]string, bool) {
	managed := make(map[string]bool, len(entries))
	var fetchTargets []string
	var recoveryNeedsFlush bool

	managedProviders := make(map[string]bool)
	for _, p := range e.cfg.ManagedProviders() {
		managedProviders[p] = true
	}

	for _, entry := range entries {
		provider := normalizeProvider(entry)
		if !managedProviders[provider] {
			continue // unrelated providers are never touched
		}
		if entry.RuntimeOnly {
			continue // no physical file; cannot be read or saved safely
		}
		if entry.AuthIndex == "" || entry.Name == "" {
			continue
		}
		managed[entry.AuthIndex] = true

		acct := e.accounts[entry.AuthIndex]
		if acct == nil {
			acct = &account{
				authIndex:  entry.AuthIndex,
				provider:   provider,
				health:     HealthHealthy,
				resetState: ResetUnknown,
			}
			e.accounts[entry.AuthIndex] = acct
		}
		acct.id = entry.ID
		acct.name = entry.Name
		acct.provider = provider
		acct.label = accountLabel(entry)
		acct.disabled = entry.Disabled

		// Priority uses omitempty on the wire, so zero is ambiguous
		// (missing vs. physical zero). It must explicitly invalidate any
		// cached nonzero value so drift to zero is re-read and corrected.
		if entry.Priority == 0 {
			acct.currentKnown = false
		} else {
			acct.currentPriority = entry.Priority
			acct.currentKnown = true
		}

		quarantined, reason := quarantineReasonFor(entry)
		switch {
		case quarantined:
			if acct.health != HealthQuarantined {
				acct.cancelRetriesLocked()
				acct.fetchEpoch++
			}
			acct.health = HealthQuarantined
			acct.quarantineReason = reason
			acct.recoverySentinelReady = false
			acct.wantRetrySchedule = false
			acct.wantAwaitingRetry = false
		case acct.health == HealthQuarantined:
			// CPA reports the credential healthy again. Discard all pre-failure
			// reset/fetch state and force a physical sentinel verification before
			// a recovery request may promote the account.
			acct.health = HealthRecovering
			acct.recoveredAt = now
			acct.recoverySentinelReady = false
			acct.fetchEpoch++
			acct.resetAt = time.Time{}
			acct.resetState = ResetUnknown
			acct.observedAt = time.Time{}
			acct.currentKnown = false
			acct.wantRetrySchedule = true
			acct.wantAwaitingRetry = false
			recoveryNeedsFlush = true
		}
		if acct.health == HealthRecovering && (!acct.currentKnown || acct.currentPriority != e.cfg.QuarantinePriority) {
			acct.recoverySentinelReady = false
			recoveryNeedsFlush = true
		}

		if acct.health != HealthQuarantined {
			fetchTargets = append(fetchTargets, entry.AuthIndex)
		}
	}

	// Removed/disappeared accounts contract the pool on this pass.
	for authIndex, acct := range e.accounts {
		if !managed[authIndex] {
			acct.cancelRetriesLocked()
			delete(e.accounts, authIndex)
		}
	}
	return fetchTargets, recoveryNeedsFlush
}

func normalizeProvider(entry hostapi.AuthEntry) string {
	p := entry.Provider
	if p == "" {
		p = entry.Type
	}
	return strings.ToLower(strings.TrimSpace(p))
}

func accountLabel(entry hostapi.AuthEntry) string {
	if entry.Email != "" {
		return entry.Email
	}
	if entry.Label != "" {
		return entry.Label
	}
	return entry.Name
}

// fetchAll refreshes weekly-reset observations. fetchOne owns the engine-wide
// semaphore, so full reconciliations, exact-deadline fetches, and retry waves
// all share the same bounded-concurrency limit.
func (e *Engine) fetchAll(ctx context.Context, authIndexes []string) {
	if len(authIndexes) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, authIndex := range authIndexes {
		wg.Add(1)
		go func(idx string) {
			defer wg.Done()
			e.fetchOne(ctx, idx)
		}(authIndex)
	}
	wg.Wait()
}

type fetchAttempt struct {
	authIndex string
	epoch     uint64
	seq       uint64
	startedAt time.Time
}

// fetchOne reads the latest credential JSON, performs the provider quota
// request, and applies the observation. Every request captures the account's
// current epoch and latest-started sequence so obsolete in-flight results fail
// closed even when provider timestamps are equal.
func (e *Engine) fetchOne(ctx context.Context, authIndex string) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case e.fetchSem <- struct{}{}:
		defer func() { <-e.fetchSem }()
	case <-ctx.Done():
		return
	}

	e.mu.Lock()
	acct := e.accounts[authIndex]
	if acct == nil || e.stopped || acct.health == HealthQuarantined ||
		(acct.health == HealthRecovering && !e.cfg.DryRun && !acct.recoverySentinelReady) {
		e.mu.Unlock()
		return
	}
	acct.nextFetchSeq++
	acct.latestStartedFetchSeq = acct.nextFetchSeq
	attempt := fetchAttempt{
		authIndex: authIndex,
		epoch:     acct.fetchEpoch,
		seq:       acct.latestStartedFetchSeq,
		startedAt: e.clk.Now(),
	}
	providerID := acct.provider
	timeout := e.cfg.RequestTimeout
	e.mu.Unlock()

	provider := e.providers[providerID]
	if provider == nil {
		e.applyFetchFailure(attempt, "no provider adapter for "+providerID)
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Read the latest credential JSON just before use; never cache tokens.
	got, errGet := e.host.AuthGet(fetchCtx, authIndex)
	if errGet != nil {
		e.applyFetchFailure(attempt, sanitize.Error(errGet))
		return
	}
	creds, errCreds := providers.ExtractCredentials(providerID, got.JSON)
	if errCreds != nil {
		e.applyFetchFailure(attempt, sanitize.Error(errCreds))
		return
	}
	obs, errFetch := provider.FetchWeeklyReset(fetchCtx, creds)
	if errFetch != nil {
		e.applyFetchFailure(attempt, sanitize.Error(errFetch))
		return
	}
	e.applyObservation(attempt, obs)
}

func (e *Engine) fetchAttemptCurrentLocked(acct *account, attempt fetchAttempt) bool {
	return acct != nil && !e.stopped && acct.health != HealthQuarantined && acct.fetchEpoch == attempt.epoch
}

// A failure is authoritative only for the latest-started attempt. An older
// failure must not overwrite a later result or error.
func (e *Engine) fetchFailureCurrentLocked(acct *account, attempt fetchAttempt) bool {
	return e.fetchAttemptCurrentLocked(acct, attempt) && acct.latestStartedFetchSeq == attempt.seq
}

// A valid weekly observation is allowed to complete after a later-started
// attempt fails. Only a newer valid weekly observation blocks it; this avoids
// losing fresh rollover data solely because an overlapping retry timed out.
func (e *Engine) weeklyObservationCurrentLocked(acct *account, attempt fetchAttempt) bool {
	return e.fetchAttemptCurrentLocked(acct, attempt) && attempt.seq >= acct.latestWeeklyFetchSeq
}

// fetchCrossedKnownDeadlineLocked rejects a request that began before the
// account's then-current weekly deadline but completed after it. This closes
// the race where the provider result acquires the state lock before the exact
// deadline callback has had a chance to bump the fetch epoch.
func (e *Engine) fetchCrossedKnownDeadlineLocked(acct *account, attempt fetchAttempt, now time.Time) bool {
	if acct.health != HealthHealthy || (acct.resetState != ResetConfirmed && acct.resetState != ResetStale) {
		return false
	}
	return !acct.resetAt.After(now) && attempt.startedAt.Before(acct.resetAt)
}

// enterAwaitingNewWindowLocked records one expiry transition and invalidates
// every request that began before it. The next flush owns demotion writeback
// and retry scheduling for this transition.
func (e *Engine) enterAwaitingNewWindowLocked(acct *account) bool {
	if acct.resetState == ResetAwaitingNewWindow {
		return false
	}
	acct.resetState = ResetAwaitingNewWindow
	acct.fetchEpoch++
	acct.wantAwaitingRetry = true
	acct.wantRetrySchedule = false
	return true
}

// applyFetchFailure records a refresh failure without reshuffling ranking
// prematurely: a still-future confirmed reset is retained (marked stale); an
// expired reset can no longer be used and forces awaiting_new_window.
func (e *Engine) applyFetchFailure(attempt fetchAttempt, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	acct := e.accounts[attempt.authIndex]
	if !e.fetchFailureCurrentLocked(acct, attempt) {
		return
	}
	now := e.clk.Now()
	acct.lastError = message
	if e.fetchCrossedKnownDeadlineLocked(acct, attempt, now) {
		e.enterAwaitingNewWindowLocked(acct)
		return
	}
	switch acct.resetState {
	case ResetConfirmed, ResetStale:
		if acct.resetAt.After(now) {
			acct.resetState = ResetStale
		} else {
			e.enterAwaitingNewWindowLocked(acct)
		}
	}
}

// applyObservation folds a provider observation into account state. Taking the
// write lock first prevents a recovery promotion from racing a sentinel save.
func (e *Engine) applyObservation(attempt fetchAttempt, obs providers.Observation) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	acct := e.accounts[attempt.authIndex]
	if !e.fetchAttemptCurrentLocked(acct, attempt) {
		return
	}
	if obs.HasWeekly {
		if !e.weeklyObservationCurrentLocked(acct, attempt) {
			return
		}
		acct.latestWeeklyFetchSeq = attempt.seq
	} else if acct.latestStartedFetchSeq != attempt.seq {
		// A missing-window response is failure-like: an older one must not
		// overwrite any later-started attempt, but it also must not block an
		// older in-flight request from returning a valid weekly timestamp.
		return
	}
	now := e.clk.Now()
	if e.fetchCrossedKnownDeadlineLocked(acct, attempt, now) {
		acct.lastError = "discarded provider result from request begun before the expired weekly reset"
		e.enterAwaitingNewWindowLocked(acct)
		return
	}

	if !obs.HasWeekly {
		// The provider answered but exposed no regular weekly window. Never
		// substitute another window or signal (spec section 11).
		acct.observedAt = obs.ObservedAt
		acct.lastError = "provider did not report a regular weekly quota window"
		switch acct.resetState {
		case ResetConfirmed, ResetStale:
			if acct.resetAt.After(now) {
				acct.resetState = ResetStale
			} else {
				e.enterAwaitingNewWindowLocked(acct)
			}
		}
		return
	}

	if obs.ResetAt.After(now) {
		// Fresh future weekly reset. A recovering account additionally requires
		// a request begun in the current recovery epoch and a confirmed physical
		// sentinel, except that dry-run may simulate promotion without saving.
		if acct.health == HealthRecovering {
			if attempt.startedAt.Before(acct.recoveredAt) || (!e.cfg.DryRun && !acct.recoverySentinelReady) {
				return
			}
			acct.health = HealthHealthy
			acct.quarantineReason = ""
		}
		acct.resetAt = obs.ResetAt
		acct.resetState = ResetConfirmed
		acct.observedAt = obs.ObservedAt
		acct.lastSuccessAt = now
		acct.lastError = ""
		acct.wantRetrySchedule = false
		acct.wantAwaitingRetry = false
		acct.cancelRetriesLocked()
		return
	}

	// The provider still reports an expired weekly window (Codex lazy reset,
	// or a stale response after a rollover). The expired timestamp must never
	// rank the account (spec sections 9, 10, 14).
	acct.resetAt = obs.ResetAt
	acct.observedAt = obs.ObservedAt
	acct.lastSuccessAt = now
	acct.lastError = "provider still reports an expired weekly window"
	e.enterAwaitingNewWindowLocked(acct)
}

type retrySchedule struct {
	authIndex string
	epoch     uint64
}

// flush recomputes ranking and synchronously persists local demotions before
// any reset-specific network fetch is armed. If another exact deadline passes
// while host writeback is in progress, the loop demotes and writes that account
// too; rescheduling must never silently discard a deadline crossed mid-write.
func (e *Engine) flush(ctx context.Context) {
	var pendingRetries []retrySchedule
	for {
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return
		}
		now := e.clk.Now()
		e.demoteExpiredLocked(now)
		e.recomputeDesiredLocked(now)
		plan := e.writePlanLocked()
		pendingRetries = append(pendingRetries, e.collectAwaitingRetriesLocked()...)
		e.mu.Unlock()

		e.performWrites(ctx, plan)

		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return
		}
		now = e.clk.Now()
		if e.demoteExpiredLocked(now) {
			// A deadline crossed while writes were in progress. Do not arm provider
			// reads yet: the newly expired account's local demotion must be written
			// first on the next iteration.
			e.mu.Unlock()
			continue
		}
		e.armAwaitingRetriesLocked(pendingRetries)
		e.rescheduleDeadlineLocked(now)
		e.publishStatusLocked(now)
		e.mu.Unlock()
		return
	}
}

// demoteExpiredLocked applies the critical rollover rule locally: any
// rankable account whose known reset time has passed immediately leaves
// earliest-deadline preference, independent of any network request (spec
// section 9).
func (e *Engine) demoteExpiredLocked(now time.Time) bool {
	changed := false
	for _, acct := range e.accounts {
		if acct.health != HealthHealthy {
			continue
		}
		if acct.resetState != ResetConfirmed && acct.resetState != ResetStale {
			continue
		}
		if acct.resetAt.After(now) {
			continue
		}
		if e.enterAwaitingNewWindowLocked(acct) {
			changed = true
		}
	}
	return changed
}

func (e *Engine) collectAwaitingRetriesLocked() []retrySchedule {
	var retries []retrySchedule
	for _, acct := range e.accounts {
		if !acct.wantAwaitingRetry {
			continue
		}
		acct.wantAwaitingRetry = false
		retries = append(retries, retrySchedule{authIndex: acct.authIndex, epoch: acct.fetchEpoch})
	}
	return retries
}

func (e *Engine) armAwaitingRetriesLocked(retries []retrySchedule) {
	for _, retry := range retries {
		acct := e.accounts[retry.authIndex]
		if acct == nil || acct.fetchEpoch != retry.epoch || acct.health == HealthQuarantined || acct.resetState != ResetAwaitingNewWindow {
			continue
		}
		acct.wantRetrySchedule = false
		e.startRetrySequenceLocked(acct, true)
	}
}

// recomputeDesiredLocked ranks each provider group independently (spec
// section 5). Quarantined and recovering accounts receive the quarantine
// sentinel and do not count toward N (spec section 6A.3).
func (e *Engine) recomputeDesiredLocked(now time.Time) {
	groups := make(map[string][]*account)
	for _, acct := range e.accounts {
		if acct.health == HealthHealthy {
			groups[acct.provider] = append(groups[acct.provider], acct)
		} else {
			acct.desired = e.cfg.QuarantinePriority
		}
	}
	for _, group := range groups {
		entries := make([]RankEntry, 0, len(group))
		byKey := make(map[string]*account, len(group))
		for _, acct := range group {
			key := acct.stableKey()
			byKey[key] = acct
			entries = append(entries, RankEntry{
				Key:            key,
				ResetAt:        acct.resetAt,
				HasFutureReset: acct.hasFutureReset(now),
			})
		}
		for key, priority := range Rank(entries, e.cfg.PriorityFloor, e.cfg.PriorityStep) {
			byKey[key].desired = priority
		}
	}
}

// scheduleRecoveryRetriesLocked starts bounded retry sequences for accounts
// that entered recovery this pass and were not confirmed by the fetch phase.
func (e *Engine) scheduleRecoveryRetriesLocked() {
	for _, acct := range e.accounts {
		if !acct.wantRetrySchedule {
			continue
		}
		acct.wantRetrySchedule = false
		if acct.health != HealthRecovering {
			continue
		}
		e.startRetrySequenceLocked(acct, false)
	}
}

// startRetrySequenceLocked schedules the bounded +5s/+30s/+2m/+5m/+15m retry
// sequence for one account, optionally with an immediate asynchronous first
// attempt (used at reset deadlines). Retries only ever try to obtain a fresh
// observation; they never re-promote with stale data.
func (e *Engine) startRetrySequenceLocked(acct *account, immediate bool) {
	acct.cancelRetriesLocked()
	seq := acct.retrySeq
	authIndex := acct.authIndex
	if immediate {
		e.runAsync(func() { e.retryFetch(authIndex, seq) })
	}
	for _, delay := range resetRetryDelays {
		timer := e.afterFunc(delay, func() { e.retryFetch(authIndex, seq) })
		acct.retryTimers = append(acct.retryTimers, timer)
	}
}

// retryFetch is one bounded retry attempt. It validates the sequence and the
// account's need for a fresh observation before doing any work.
func (e *Engine) retryFetch(authIndex string, seq int) {
	ctx := context.Background()
	if !e.retryStillNeeded(authIndex, seq) {
		return
	}
	// A recovering auth whose earlier sentinel write failed gets another
	// synchronous verification attempt before this provider request.
	e.flush(ctx)
	if !e.retryStillNeeded(authIndex, seq) {
		return
	}
	e.fetchOne(ctx, authIndex)
	e.flush(ctx)
}

func (e *Engine) retryStillNeeded(authIndex string, seq int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	acct := e.accounts[authIndex]
	return acct != nil && !e.stopped && seq == acct.retrySeq &&
		(acct.health == HealthRecovering || acct.resetState == ResetAwaitingNewWindow)
}

// onDeadline handles one exact reset timer generation. A callback that fired
// just before a newer observation replaced its timer must not clear or orphan
// that replacement. The current generation delegates to flush, whose first
// operation is synchronous local demotion/writeback and whose network retries
// are armed only afterward (spec sections 9 and 18).
func (e *Engine) onDeadline(seq uint64) {
	e.mu.Lock()
	if e.stopped || seq != e.deadlineSeq {
		e.mu.Unlock()
		return
	}
	e.nextDeadlineAt = time.Time{}
	e.deadlineTimer = nil
	e.mu.Unlock()

	e.flush(context.Background())
}

// rescheduleDeadlineLocked arms the single exact-deadline timer for the
// earliest upcoming rankable reset. The timer uses the exact timestamp with
// no rounding (spec section 8).
func (e *Engine) rescheduleDeadlineLocked(now time.Time) {
	var earliest time.Time
	for _, acct := range e.accounts {
		if acct.health != HealthHealthy || !acct.hasFutureReset(now) {
			continue
		}
		if earliest.IsZero() || acct.resetAt.Before(earliest) {
			earliest = acct.resetAt
		}
	}
	if earliest.IsZero() {
		if e.deadlineTimer != nil {
			e.deadlineSeq++
			e.deadlineTimer.Stop()
			e.deadlineTimer = nil
		}
		e.nextDeadlineAt = time.Time{}
		return
	}
	if e.deadlineTimer != nil && earliest.Equal(e.nextDeadlineAt) {
		return
	}
	if e.deadlineTimer != nil {
		e.deadlineTimer.Stop()
	}
	e.deadlineSeq++
	seq := e.deadlineSeq
	e.nextDeadlineAt = earliest
	e.deadlineTimer = e.afterFunc(earliest.Sub(now), func() { e.onDeadline(seq) })
}

// scheduleNextReconcileLocked arms the background reconciliation interval.
func (e *Engine) scheduleNextReconcileLocked() {
	if e.stopped {
		return
	}
	if e.reconcileTimer != nil {
		e.reconcileTimer.Stop()
	}
	interval := e.cfg.ReconcileInterval
	e.nextReconcileAt = e.clk.Now().Add(interval)
	e.reconcileTimer = e.afterFunc(interval, func() {
		e.Reconcile(context.Background(), "interval")
	})
}

// afterFunc reserves engine work before exposing a timer to the clock. A
// successful Stop releases a callback that will never run; otherwise the
// callback releases itself after returning. This closes the fired-but-not-yet-
// running timer race during terminal shutdown.
func (e *Engine) afterFunc(delay time.Duration, f func()) clock.Timer {
	if !e.reserveWork() {
		return inactiveTimer{}
	}
	tracked := &trackedTimer{release: e.workWG.Done}
	tracked.timer = e.clk.AfterFunc(delay, func() {
		defer tracked.finish()
		f()
	})
	return tracked
}

type trackedTimer struct {
	timer   clock.Timer
	once    sync.Once
	release func()
}

func (t *trackedTimer) Stop() bool {
	if t == nil || t.timer == nil || !t.timer.Stop() {
		return false
	}
	t.finish()
	return true
}

func (t *trackedTimer) finish() {
	t.once.Do(t.release)
}

type inactiveTimer struct{}

func (inactiveTimer) Stop() bool { return false }
