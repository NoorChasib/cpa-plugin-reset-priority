package engine

import (
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

// ResetState describes the trustworthiness of an account's weekly reset
// timestamp. Only Confirmed and Stale timestamps may be used for ranking,
// and only while they are still in the future.
type ResetState string

const (
	// ResetConfirmed: the provider reported a future weekly reset.
	ResetConfirmed ResetState = "confirmed"
	// ResetStale: a refresh failed but the last-known confirmed reset is
	// still in the future; it keeps ranking until it expires.
	ResetStale ResetState = "stale"
	// ResetAwaitingNewWindow: the known reset deadline has passed and no
	// fresh future window has been confirmed. The expired timestamp must
	// never rank the account again.
	ResetAwaitingNewWindow ResetState = "awaiting_new_window"
	// ResetUnknown: no weekly reset has ever been confirmed (new account,
	// missing window, parser failure, ...). Never substituted with another
	// quota window.
	ResetUnknown ResetState = "unknown"
)

// Health describes credential health, which is deliberately independent from
// weekly-reset ranking (spec section 6A).
type Health string

const (
	// HealthHealthy: rankable.
	HealthHealthy Health = "healthy"
	// HealthQuarantined: definitively not a usable credential (disabled or
	// reauth-required). Excluded from the rank count; desired priority is
	// the quarantine sentinel.
	HealthQuarantined Health = "quarantined"
	// HealthRecovering: CPA reports the credential healthy again, but no
	// FRESH post-recovery future weekly reset has been confirmed yet. Stays
	// at the quarantine priority so stale pre-failure data cannot promote
	// it (spec section 6A.4).
	HealthRecovering Health = "recovering"
)

// account is the engine's mutable per-credential state. All access is
// guarded by Engine.mu.
type account struct {
	authIndex string
	id        string
	name      string // physical auth file name used for host.auth.save
	provider  string
	label     string
	disabled  bool

	health           Health
	quarantineReason string
	// recoveredAt marks the quarantined -> recovering transition. Recovery
	// fetches must begin at or after this instant, and non-dry-run promotion is
	// blocked until recoverySentinelReady confirms the physical sentinel write.
	recoveredAt           time.Time
	recoverySentinelReady bool
	fetchEpoch            uint64
	nextFetchSeq          uint64
	latestStartedFetchSeq uint64
	latestWeeklyFetchSeq  uint64

	resetAt       time.Time
	resetState    ResetState
	observedAt    time.Time
	lastSuccessAt time.Time
	lastError     string
	writeError    string

	// currentPriority is the best-known physical priority. currentKnown is
	// false when only the ambiguous omitempty list value is available.
	currentPriority int
	currentKnown    bool
	desired         int

	// retrySeq invalidates outstanding bounded retry callbacks; retryTimers
	// holds their cancellable handles.
	retrySeq    int
	retryTimers []clock.Timer
	// wantRetrySchedule asks the current reconcile pass to start a recovery
	// retry sequence if the account is still not healthy after the fetch phase.
	wantRetrySchedule bool
	// wantAwaitingRetry is set exactly once on each transition caused by an
	// expired reset. The next flush consumes it only after synchronous demotion
	// writeback, then arms the immediate passive fetch and bounded retries.
	wantAwaitingRetry bool
}

// stableKey is the deterministic tie-break key (spec section 12).
func (a *account) stableKey() string {
	if a.name != "" {
		return a.name
	}
	if a.id != "" {
		return a.id
	}
	return a.authIndex
}

// hasFutureReset reports whether the account's reset timestamp may be used
// for ranking right now.
func (a *account) hasFutureReset(now time.Time) bool {
	if a.resetState != ResetConfirmed && a.resetState != ResetStale {
		return false
	}
	return a.resetAt.After(now)
}

// cancelRetriesLocked invalidates and stops any scheduled bounded retries.
func (a *account) cancelRetriesLocked() {
	a.retrySeq++
	for _, t := range a.retryTimers {
		t.Stop()
	}
	a.retryTimers = nil
}

// quarantineReasonFor classifies definitive credential-health failures.
//
// Narrow by design (spec sections 6A.1, 6A.2, 6A.6):
//   - disabled credentials are quarantined;
//   - StatusError with an auth-specific message (unauthorized,
//     invalid_grant, reauth, revoked, 401/403) is quarantined;
//   - Unavailable alone, quota/cooldown/429 states, and generic transient
//     errors are NOT quarantined — CPA's native availability handling owns
//     those.
func quarantineReasonFor(entry hostapi.AuthEntry) (bool, string) {
	if entry.Disabled || strings.EqualFold(entry.Status, "disabled") {
		return true, "disabled"
	}
	if !strings.EqualFold(entry.Status, "error") {
		return false, ""
	}

	msg := strings.ToLower(entry.StatusMessage)
	// Definitive authentication markers take precedence over any generic quota
	// wording that may be included in the same CPA error message.
	for _, marker := range []string{
		"unauthorized", "invalid_grant", "invalid grant", "reauth",
		"re-auth", "authentication required", "invalid credential",
		"revoked", "forbidden",
	} {
		if strings.Contains(msg, marker) {
			return true, "reauth_required"
		}
	}
	if containsStandaloneCode(msg, "401") || containsStandaloneCode(msg, "403") {
		return true, "reauth_required"
	}

	// Quota/rate-limit/cooldown wording alone is never a credential-health
	// failure. CPA's native availability handling owns those transient states.
	for _, benign := range []string{"quota", "rate limit", "rate-limit", "429", "cooldown", "cool down", "overloaded"} {
		if strings.Contains(msg, benign) {
			return false, ""
		}
	}
	return false, ""
}

func containsStandaloneCode(message, code string) bool {
	for start := 0; ; {
		i := strings.Index(message[start:], code)
		if i < 0 {
			return false
		}
		i += start
		beforeDigit := i > 0 && message[i-1] >= '0' && message[i-1] <= '9'
		after := i + len(code)
		afterDigit := after < len(message) && message[after] >= '0' && message[after] <= '9'
		if !beforeDigit && !afterDigit {
			return true
		}
		start = i + len(code)
	}
}
