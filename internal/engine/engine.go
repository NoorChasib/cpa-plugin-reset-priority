// Package engine implements discovery, weekly-reset observation, quarantine
// and recovery (including a short-cadence local health probe over
// host.auth.get_runtime while any account is quarantined or recovering),
// deterministic ranking, exact deadline timers, bounded retries, and
// priority writeback for the reset-priority plugin.
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

// ReconcileResult describes the outcome of one reconciliation pass.
type ReconcileResult string

const (
	// ReconcileResultSuccess reports that a full reconciliation pass completed.
	ReconcileResultSuccess ReconcileResult = "success"
	// ReconcileResultNoOp reports that the engine was stopped before the pass
	// could complete.
	ReconcileResultNoOp ReconcileResult = "no_op"
	// ReconcileResultError reports that roster discovery or validation failed,
	// so the engine retained its prior roster rather than applying a partial one.
	ReconcileResultError ReconcileResult = "error"
)

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

// healthPollInterval is the short cadence of the local credential-health
// probe that runs only while at least one account is quarantined or
// recovering. Each tick reads runtime health via host.auth.get_runtime (a
// cheap local manager lookup, never a provider request and never credential
// JSON) so CPA self-recovery — a successful token refresh or an operator
// re-enabling/re-authenticating an auth — is detected within about a minute
// instead of waiting for the next hourly reconciliation. The bounded
// resetRetryDelays ladder is deliberately NOT reused here: it ends ~22
// minutes after the triggering event, while self-recovery can happen hours
// later, which would reintroduce the reconcile-interval detection latency.
const healthPollInterval = time.Minute

// HostAuth is the subset of host callbacks the engine needs.
type HostAuth interface {
	AuthList(ctx context.Context) ([]hostapi.AuthEntry, error)
	AuthGet(ctx context.Context, authIndex string) (hostapi.AuthGetResponse, error)
	// AuthGetRuntime returns the runtime health entry for one auth index
	// without exposing credential JSON. Errors mean "no usable observation"
	// (unknown index, disabled auth whose file disappeared, transient host
	// failure) and must never be interpreted as a health transition.
	AuthGetRuntime(ctx context.Context, authIndex string) (hostapi.AuthEntry, error)
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
	// saveMu makes a reconfiguration barrier mutually exclusive with the final
	// dry-run check and host.auth.save. This lets Reconfigure return while a
	// read-before-write is blocked, while still ensuring a dry-run update cannot
	// be followed by a stale save.
	saveMu sync.Mutex
	// providerCallMu is a shared admission barrier around actual provider calls.
	// Reconfigure takes it exclusively while publishing provider opt-outs, so it
	// waits for already-admitted calls and no stale call can begin afterward.
	// The read side preserves the normal four-way provider concurrency.
	providerCallMu sync.RWMutex

	cfg       config.Config
	configSeq uint64
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
	healthPollTimer clock.Timer
	healthPollSeq   uint64

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
	e.runAsync(func() { e.Reconcile(context.Background()) })
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
	e.healthPollSeq++
	if e.healthPollTimer != nil {
		e.healthPollTimer.Stop()
		e.healthPollTimer = nil
	}
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
	// Publish permission changes while holding the same gates as the final
	// AuthSave and provider-call admissions. Already-authorized work completes
	// before the config changes; work still blocked in a local credential read
	// reaches final admission afterward and observes the new generation. Thus no
	// stale save or newly-started opted-out provider call can follow this return.
	e.providerCallMu.Lock()
	e.saveMu.Lock()
	e.mu.Lock()
	e.cfg = cfg
	e.configSeq++
	e.stopped = !cfg.Enabled
	e.mu.Unlock()
	e.saveMu.Unlock()
	e.providerCallMu.Unlock()

	if !cfg.Enabled {
		e.Stop()
		return
	}
	e.runAsync(func() { e.Reconcile(context.Background()) })
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
// invocations (startup, hourly, management refresh) are serialized. It returns
// no_op when stopped, error when roster discovery or validation is deferred,
// and success after a complete pass.
func (e *Engine) Reconcile(ctx context.Context) (result ReconcileResult) {
	result = ReconcileResultNoOp
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
	result = ReconcileResultSuccess

	// Claim the current roster's immediate provider attempts before any blocking
	// local write, list, or validation callback. Otherwise an exact deadline
	// reached while this pass waits can launch a detached retry, followed by a
	// duplicate full-pass request that alone carries the management callback ID.
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return ReconcileResultNoOp
	}
	reservedTargets := e.reconcileFetchTargetsLocked()
	e.reserveReconcileFetchesLocked(reservedTargets)
	e.mu.Unlock()
	reservationsHeld := true
	defer func() {
		if reservationsHeld {
			e.releaseReconcileFetches(reservedTargets)
		}
	}()

	// A reconfiguration can disable one provider while retaining engine state
	// from the prior pass. Remove those accounts before the pre-fetch flush so
	// opted-out credentials can never receive one final rank/deadline write.
	e.pruneUnmanagedAccounts()

	// A missed exact-deadline callback must not let a stale known window reach a
	// provider fetch. Demote and persist every already-expired known deadline
	// before roster discovery can produce the next fetch targets. Do not launch
	// the deadline retry here: this full pass is about to fetch the same account,
	// and the post-fetch flush will arm retries only if it still needs a new window.
	e.flushBeforeReconcile(ctx)

	entries, errRoster := e.host.AuthList(ctx)
	var validated []hostapi.AuthEntry
	if errRoster == nil {
		validated, errRoster = e.validateRoster(ctx, entries)
	}

	var fetchTargets []string
	var preFetchFlushNeeded bool
	if errRoster != nil {
		result = ReconcileResultError
		rosterError := sanitize.Error(errRoster)
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return ReconcileResultNoOp
		}
		// A list or validation read was incomplete. Keep the prior roster as one
		// transaction rather than contracting the rank pool because one established
		// physical auth was temporarily unreadable.
		e.lastRosterError = rosterError
		e.mu.Unlock()
		// Cross-ABI host logging must never run under Engine.mu (see Logger).
		e.logf("warn", "reset-priority: auth roster reconciliation deferred: "+rosterError)

		// No full-pass provider fetch can be trusted without a validated roster.
		// Release its provisional ownership before consuming any missed-deadline
		// intent in ordinary mode: demotion is already physical before the required
		// immediate retry is launched, followed by the documented delayed ladder.
		e.releaseReconcileFetches(reservedTargets)
		reservationsHeld = false
		e.flush(ctx)
	} else {
		// Serialize roster health transitions with writeback so quarantine and
		// recovery cannot race a stale rank write. A newly quarantined auth may
		// then deliberately persist the sentinel through the next flush.
		e.writeMu.Lock()
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			e.writeMu.Unlock()
			return ReconcileResultNoOp
		}
		e.lastRosterError = ""
		var recoveryNeedsFlush bool
		fetchTargets, recoveryNeedsFlush = e.applyRosterLocked(validated, e.clk.Now())
		e.replaceReconcileFetchReservationsLocked(fetchTargets)
		reservedTargets = fetchTargets
		// Roster validation can block across the exact reset instant. Detect that
		// transition under the same state lock; the conditional flush below avoids
		// writing provisional rankings for newly discovered unknown accounts.
		preFetchFlushNeeded = recoveryNeedsFlush || e.demoteExpiredLocked(e.clk.Now())
		e.mu.Unlock()
		e.writeMu.Unlock()

		if preFetchFlushNeeded {
			// Persist every deadline demotion and verify/write every recovery sentinel
			// before provider traffic. The full pass owns the immediate fetch, so this
			// flush deliberately leaves awaiting retry intent pending.
			e.flushBeforeReconcile(ctx)
		}
		e.fetchAll(ctx, fetchTargets)
		e.releaseReconcileFetches(fetchTargets)
		reservationsHeld = false
		e.flushAfterFetch(ctx)
	}

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return ReconcileResultNoOp
	}
	e.scheduleRecoveryRetriesLocked()
	e.scheduleNextReconcileLocked()
	e.scheduleHealthPollLocked()
	e.publishStatusLocked(e.clk.Now())
	e.mu.Unlock()
	return result
}

// pruneUnmanagedAccounts removes retained state for providers disabled by the
// current configuration before any ranking or write planning. writeMu keeps the
// transition ordered after an older write pass; configSeq makes that older pass
// fail its final save admission if it was still blocked in AuthGet when the
// reconfiguration was published.
func (e *Engine) pruneUnmanagedAccounts() {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	for authIndex, acct := range e.accounts {
		if e.cfg.Manages(acct.provider) {
			continue
		}
		acct.cancelRetriesLocked()
		acct.fetchEpoch++
		delete(e.accounts, authIndex)
	}
}

// reconcileFetchTargetsLocked snapshots the retained accounts for which a full
// pass can own provider work before roster callbacks refine that set. Caller
// holds e.mu.
func (e *Engine) reconcileFetchTargetsLocked() []string {
	var authIndexes []string
	for authIndex, acct := range e.accounts {
		if e.cfg.Manages(acct.provider) && acct.health != HealthQuarantined {
			authIndexes = append(authIndexes, authIndex)
		}
	}
	return authIndexes
}

// reserveReconcileFetchesLocked gives this full pass ownership of each target's
// immediate provider attempt. Existing bounded retries are invalidated; an exact
// deadline may still demote/write synchronously, but collectAwaitingRetriesLocked
// leaves its retry intent pending for this pass. Caller holds e.mu.
func (e *Engine) reserveReconcileFetchesLocked(authIndexes []string) {
	for _, authIndex := range authIndexes {
		acct := e.accounts[authIndex]
		if acct == nil || !e.cfg.Manages(acct.provider) || acct.health == HealthQuarantined ||
			acct.reconcileFetchPending {
			continue
		}
		// The full pass replaces any live ladder with its own immediate attempt.
		// Preserve enough intent to rebuild the delayed ladder if that attempt
		// fails or cannot begin (for example, a recovery sentinel is still pending).
		if acct.resetState == ResetAwaitingNewWindow {
			acct.wantAwaitingRetry = true
			// The full pass replaces the prior immediate/delayed schedule. It clears
			// this bit only at actual provider admission; any earlier exit must restore
			// a true post-demotion immediate attempt rather than waiting five seconds.
			acct.awaitingNeedsImmediate = true
		}
		if acct.health == HealthRecovering {
			acct.wantRetrySchedule = true
		}
		acct.cancelRetriesLocked()
		acct.reconcileFetchPending = true
	}
}

// replaceReconcileFetchReservationsLocked updates the provisional pre-roster
// reservation to the validated fetch roster without re-invalidating accounts
// already owned by this pass. Caller holds e.mu.
func (e *Engine) replaceReconcileFetchReservationsLocked(authIndexes []string) {
	wanted := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		wanted[authIndex] = struct{}{}
	}
	for authIndex, acct := range e.accounts {
		if _, ok := wanted[authIndex]; !ok {
			acct.reconcileFetchPending = false
		}
	}
	e.reserveReconcileFetchesLocked(authIndexes)
}

func (e *Engine) releaseReconcileFetches(authIndexes []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, authIndex := range authIndexes {
		if acct := e.accounts[authIndex]; acct != nil {
			acct.reconcileFetchPending = false
		}
	}
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
	cfg := e.cfg
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
		if !cfg.Manages(provider) {
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

	for _, entry := range entries {
		provider := normalizeProvider(entry)
		if !e.cfg.Manages(provider) {
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
				acct.quarantineSentinelWrittenAt = time.Time{}
			}
			acct.health = HealthQuarantined
			acct.quarantineReason = reason
			acct.recoverySentinelReady = false
			acct.wantRetrySchedule = false
			acct.wantAwaitingRetry = false
			acct.awaitingNeedsImmediate = false
		case acct.health == HealthQuarantined && acct.hasPostSentinelRecoveryEvidence(entry):
			// CPA reports the credential healthy again with evidence newer than any
			// plugin-written sentinel. Discard all pre-failure reset/fetch state and
			// force a physical sentinel verification before a recovery request may
			// promote the account.
			acct.health = HealthRecovering
			acct.recoveredAt = now
			acct.recoverySentinelReady = false
			acct.quarantineSentinelWrittenAt = time.Time{}
			acct.fetchEpoch++
			acct.resetAt = time.Time{}
			acct.resetState = ResetUnknown
			acct.observedAt = time.Time{}
			acct.currentKnown = false
			acct.wantRetrySchedule = true
			acct.wantAwaitingRetry = false
			acct.awaitingNeedsImmediate = false
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
			e.fetchOneInternal(ctx, idx, true, noRetrySequence)
		}(authIndex)
	}
	wg.Wait()
}

const noRetrySequence = -1

type fetchAttempt struct {
	authIndex        string
	epoch            uint64
	seq              uint64
	configSeq        uint64
	expectedRetrySeq int
	startedAt        time.Time
}

// recoverySentinelPendingLocked reports whether live mode must verify the
// physical quarantine sentinel before a recovering account can fetch or
// promote. Caller holds e.mu.
func (e *Engine) recoverySentinelPendingLocked(acct *account) bool {
	return acct.health == HealthRecovering && !e.cfg.DryRun && !acct.recoverySentinelReady
}

// fetchOne is the direct/retry fetch path. Full reconciliations use
// fetchOneInternal with reconcileOwned=true so a delayed deadline callback can
// hand its immediate attempt to the already-running pass without duplication.
func (e *Engine) fetchOne(ctx context.Context, authIndex string) {
	e.fetchOneInternal(ctx, authIndex, false, noRetrySequence)
}

// fetchOneInternal reads the latest credential JSON, performs the provider
// quota request, and applies the observation. Every request captures the
// account epoch, config generation, and latest-started sequence so obsolete
// in-flight work fails closed even when provider timestamps are equal.
//
// Expiry is checked both before the credential read and at final provider-call
// admission. If a reset crosses during a local host callback, the old attempt
// is invalidated, demotion is synchronously persisted, and only then does a
// reconcile-owned replacement attempt proceed.
func (e *Engine) fetchOneInternal(
	ctx context.Context,
	authIndex string,
	reconcileOwned bool,
	expectedRetrySeq int,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case e.fetchSem <- struct{}{}:
		defer func() { <-e.fetchSem }()
	case <-ctx.Done():
		return
	}

	for {
		e.mu.Lock()
		acct := e.accounts[authIndex]
		if acct == nil || e.stopped || !e.cfg.Manages(acct.provider) ||
			acct.health == HealthQuarantined || e.recoverySentinelPendingLocked(acct) ||
			(expectedRetrySeq != noRetrySequence && acct.retrySeq != expectedRetrySeq) {
			e.mu.Unlock()
			return
		}
		now := e.clk.Now()
		if acct.health == HealthHealthy &&
			(acct.resetState == ResetConfirmed || acct.resetState == ResetStale) &&
			!acct.resetAt.After(now) {
			e.enterAwaitingNewWindowLocked(acct)
			e.mu.Unlock()
			e.flushBeforeReconcile(ctx)
			continue
		}
		acct.nextFetchSeq++
		acct.latestStartedFetchSeq = acct.nextFetchSeq
		attempt := fetchAttempt{
			authIndex:        authIndex,
			epoch:            acct.fetchEpoch,
			seq:              acct.latestStartedFetchSeq,
			configSeq:        e.configSeq,
			expectedRetrySeq: expectedRetrySeq,
			startedAt:        now,
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

		// Read the latest credential JSON just before use; never cache tokens.
		got, errGet := e.host.AuthGet(fetchCtx, authIndex)
		if errGet != nil {
			cancel()
			e.applyFetchFailure(attempt, sanitize.Error(errGet))
			return
		}
		creds, errCreds := providers.ExtractCredentials(providerID, got.JSON)
		if errCreds != nil {
			cancel()
			e.applyFetchFailure(attempt, sanitize.Error(errCreds))
			return
		}

		// Pair final provider admission with configuration publication. The shared
		// side allows normal concurrent fetches; Reconfigure's exclusive side waits
		// for admitted calls and prevents stale provider traffic after opt-out.
		e.providerCallMu.RLock()
		e.mu.Lock()
		acct = e.accounts[authIndex]
		if !e.fetchAttemptCurrentLocked(acct, attempt) {
			restart := reconcileOwned && acct != nil && !e.stopped &&
				e.cfg.Manages(acct.provider) && acct.reconcileFetchPending &&
				acct.health != HealthQuarantined && acct.resetState == ResetAwaitingNewWindow
			e.mu.Unlock()
			e.providerCallMu.RUnlock()
			cancel()
			if restart {
				e.flushBeforeReconcile(ctx)
				continue
			}
			return
		}
		now = e.clk.Now()
		if acct.health == HealthHealthy &&
			(acct.resetState == ResetConfirmed || acct.resetState == ResetStale) &&
			!acct.resetAt.After(now) {
			e.enterAwaitingNewWindowLocked(acct)
			e.mu.Unlock()
			e.providerCallMu.RUnlock()
			cancel()
			e.flushBeforeReconcile(ctx)
			continue
		}
		if acct.resetState == ResetAwaitingNewWindow {
			// Admission to the provider call is the point at which a request can
			// satisfy the required post-demotion immediate attempt. Credential-read
			// failures and requests invalidated before this gate do not count.
			acct.awaitingNeedsImmediate = false
		}
		e.mu.Unlock()

		obs, errFetch := provider.FetchWeeklyReset(fetchCtx, creds)
		e.providerCallMu.RUnlock()
		cancel()
		if errFetch != nil {
			e.applyFetchFailure(attempt, sanitize.Error(errFetch))
			return
		}
		e.applyObservation(attempt, obs)
		return
	}
}

func (e *Engine) fetchAttemptCurrentLocked(acct *account, attempt fetchAttempt) bool {
	return acct != nil && !e.stopped && e.cfg.Manages(acct.provider) &&
		attempt.configSeq == e.configSeq && acct.health != HealthQuarantined &&
		acct.fetchEpoch == attempt.epoch &&
		(attempt.expectedRetrySeq == noRetrySequence || acct.retrySeq == attempt.expectedRetrySeq)
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
	// If a full-pass request was already reserved, it may have begun before this
	// demotion. Provider admission clears the flag only for a request that reaches
	// the provider after entering awaiting_new_window; otherwise the post-fetch
	// flush restores the required immediate attempt.
	acct.awaitingNeedsImmediate = acct.reconcileFetchPending
	acct.wantRetrySchedule = false
	return true
}

// degradeResetAfterRefreshFailureLocked preserves a still-future known reset
// as stale and expires any known reset whose deadline has passed. Caller holds
// e.mu.
func (e *Engine) degradeResetAfterRefreshFailureLocked(acct *account, now time.Time) {
	switch acct.resetState {
	case ResetConfirmed, ResetStale:
		if acct.resetAt.After(now) {
			acct.resetState = ResetStale
		} else {
			e.enterAwaitingNewWindowLocked(acct)
		}
	}
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
		acct.awaitingNeedsImmediate = true
		return
	}
	e.degradeResetAfterRefreshFailureLocked(acct, now)
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
		acct.awaitingNeedsImmediate = true
		return
	}

	if !obs.HasWeekly {
		// The provider answered but exposed no regular weekly window. Never
		// substitute another window or signal (spec section 11).
		acct.observedAt = obs.ObservedAt
		acct.lastError = "provider did not report a regular weekly quota window"
		e.degradeResetAfterRefreshFailureLocked(acct, now)
		return
	}

	if obs.ResetAt.After(now) {
		// Fresh future weekly reset. A recovering account additionally requires
		// a request begun in the current recovery epoch and a confirmed physical
		// sentinel, except that dry-run may simulate promotion without saving.
		if acct.health == HealthRecovering {
			if attempt.startedAt.Before(acct.recoveredAt) || e.recoverySentinelPendingLocked(acct) {
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
		acct.awaitingNeedsImmediate = false
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
	retrySeq  int
	immediate bool
}

// flush recomputes ranking and synchronously persists local demotions before
// any reset-specific network fetch is armed. If another exact deadline passes
// while host writeback is in progress, the loop demotes and writes that account
// too; rescheduling must never silently discard a deadline crossed mid-write.
type awaitingRetryMode uint8

const (
	awaitingRetryNone awaitingRetryMode = iota
	awaitingRetryImmediate
	awaitingRetryDelayed
)

func (e *Engine) flush(ctx context.Context) {
	e.flushInternal(ctx, awaitingRetryImmediate)
}

// flushBeforeReconcile performs the same local demotion and writeback but leaves
// awaiting-window retry intent pending. The enclosing full reconciliation is
// about to fetch every managed healthy account itself; launching an immediate
// retry here would duplicate that provider request and lose its management
// callback context. The ordinary post-fetch flush either clears the intent after
// a fresh observation or arms retries if the account still awaits a new window.
func (e *Engine) flushBeforeReconcile(ctx context.Context) {
	e.flushInternal(ctx, awaitingRetryNone)
}

// flushAfterFetch treats the just-completed provider request as the immediate
// rollover attempt. If the account still awaits a new window, only the
// documented +5s/+30s/+2m/+5m/+15m delayed attempts are armed.
func (e *Engine) flushAfterFetch(ctx context.Context) {
	e.flushInternal(ctx, awaitingRetryDelayed)
}

func (e *Engine) flushInternal(ctx context.Context, retryMode awaitingRetryMode) {
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
		if retryMode != awaitingRetryNone {
			pendingRetries = append(pendingRetries, e.collectAwaitingRetriesLocked()...)
		}
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
		if retryMode != awaitingRetryNone {
			e.armAwaitingRetriesLocked(pendingRetries, retryMode == awaitingRetryImmediate)
		}
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
		if !acct.wantAwaitingRetry || acct.reconcileFetchPending {
			continue
		}
		acct.wantAwaitingRetry = false
		retries = append(retries, retrySchedule{
			authIndex: acct.authIndex,
			epoch:     acct.fetchEpoch,
			retrySeq:  acct.retrySeq,
			immediate: acct.awaitingNeedsImmediate,
		})
		acct.awaitingNeedsImmediate = false
	}
	return retries
}

func (e *Engine) armAwaitingRetriesLocked(retries []retrySchedule, immediate bool) {
	for _, retry := range retries {
		acct := e.accounts[retry.authIndex]
		if acct == nil || !e.cfg.Manages(acct.provider) || acct.fetchEpoch != retry.epoch ||
			acct.retrySeq != retry.retrySeq || acct.reconcileFetchPending ||
			acct.health == HealthQuarantined || acct.resetState != ResetAwaitingNewWindow {
			continue
		}
		acct.wantRetrySchedule = false
		e.startRetrySequenceLocked(acct, immediate || retry.immediate)
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
	e.fetchOneInternal(ctx, authIndex, false, seq)
	e.flushAfterFetch(ctx)
}

func (e *Engine) retryStillNeeded(authIndex string, seq int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	acct := e.accounts[authIndex]
	return acct != nil && !e.stopped && e.cfg.Manages(acct.provider) && seq == acct.retrySeq &&
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

// scheduleHealthPollLocked arms (or disarms) the short-cadence local health
// probe. It runs only while at least one account is quarantined or
// recovering, keeps a single timer regardless of how many accounts are
// unhealthy, and never resets a countdown that is already armed, so repeated
// reconciles cannot stretch the detection latency.
func (e *Engine) scheduleHealthPollLocked() {
	needed := false
	if !e.stopped {
		for _, acct := range e.accounts {
			if acct.health == HealthQuarantined || acct.health == HealthRecovering {
				needed = true
				break
			}
		}
	}
	if !needed {
		e.healthPollSeq++
		if e.healthPollTimer != nil {
			e.healthPollTimer.Stop()
			e.healthPollTimer = nil
		}
		return
	}
	if e.healthPollTimer != nil {
		return
	}
	e.healthPollSeq++
	seq := e.healthPollSeq
	e.healthPollTimer = e.afterFunc(healthPollInterval, func() { e.onHealthPoll(seq) })
}

// onHealthPoll performs one health-probe generation. It reads runtime health
// through host.auth.get_runtime — never credential JSON, never a provider
// request, and never while holding Engine.mu (host callbacks may block or
// re-enter, see Logger) — and compares it against the engine's last-known
// health classification. Any transition-worthy difference (a quarantined
// account no longer reporting a definitive credential-health failure, or a
// recovering account regressing to one) triggers a full serialized
// reconciliation: the probe itself never mutates account state, because only
// the roster transaction owns health transitions, sentinel writes, and the
// sentinel-before-fetch recovery ordering. Probe errors are skipped; the
// reconcile interval remains the safety net.
func (e *Engine) onHealthPoll(seq uint64) {
	e.mu.Lock()
	if e.stopped || seq != e.healthPollSeq {
		e.mu.Unlock()
		return
	}
	e.healthPollTimer = nil
	type probe struct {
		authIndex                   string
		health                      Health
		quarantineSentinelWrittenAt time.Time
	}
	var probes []probe
	for _, acct := range e.accounts {
		if acct.health == HealthQuarantined || acct.health == HealthRecovering {
			probes = append(probes, probe{
				authIndex:                   acct.authIndex,
				health:                      acct.health,
				quarantineSentinelWrittenAt: acct.quarantineSentinelWrittenAt,
			})
		}
	}
	e.mu.Unlock()

	changed := false
	for _, p := range probes {
		entry, err := e.host.AuthGetRuntime(context.Background(), p.authIndex)
		if err != nil {
			// Unknown/removed auths and transient host failures carry no health
			// information. Roster reconciliation owns removal and error surfacing.
			continue
		}
		if entry.AuthIndex != "" && entry.AuthIndex != p.authIndex {
			continue
		}
		quarantined, _ := quarantineReasonFor(entry)
		if p.health == HealthQuarantined && !quarantined && !p.quarantineSentinelWrittenAt.IsZero() {
			if !entry.LastRefresh.After(p.quarantineSentinelWrittenAt) && !entry.ModTime.After(p.quarantineSentinelWrittenAt) {
				// host.auth.save itself rebuilds the runtime record as active. Do not
				// mistake that plugin-created state for external recovery.
				continue
			}
		}
		if (p.health == HealthQuarantined) != quarantined {
			changed = true
			break
		}
	}
	if changed {
		// The reconcile tail re-arms or disarms the poll under its own seq.
		e.Reconcile(context.Background())
		return
	}
	e.mu.Lock()
	if !e.stopped && seq == e.healthPollSeq {
		e.scheduleHealthPollLocked()
	}
	e.mu.Unlock()
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
		e.Reconcile(context.Background())
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
