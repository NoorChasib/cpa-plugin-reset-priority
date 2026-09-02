package engine

import (
	"sort"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/sanitize"
)

// Snapshot is the published, sanitized status. It never contains tokens,
// auth JSON, or raw provider responses.
type Snapshot struct {
	GeneratedAt time.Time `json:"generated_at"`
	DryRun      bool      `json:"dry_run"`
	Enabled     bool      `json:"enabled"`
	Stopped     bool      `json:"stopped"`
	// DisplayTimezone is the operator-chosen presentation zone for the HTML
	// view. All timestamps in this JSON stay RFC3339 UTC regardless.
	DisplayTimezone string          `json:"display_timezone,omitempty"`
	Warnings        []string        `json:"warnings,omitempty"`
	RosterError     string          `json:"roster_error,omitempty"`
	NextReconcileAt *time.Time      `json:"next_reconcile_at,omitempty"`
	NextDeadlineAt  *time.Time      `json:"next_deadline_at,omitempty"`
	Providers       []ProviderGroup `json:"providers"`
}

// ProviderGroup is one independently ranked provider section.
type ProviderGroup struct {
	Provider string          `json:"provider"`
	Accounts []AccountStatus `json:"accounts"`
}

// AccountStatus is the per-account status row (spec section 15).
type AccountStatus struct {
	AuthIndex        string `json:"auth_index"`
	Name             string `json:"name"`
	Label            string `json:"label"`
	Provider         string `json:"provider"`
	Health           string `json:"health"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
	ResetState       string `json:"reset_state"`
	// ResetAt preserves the provider timezone; ResetAtUTC normalizes it.
	// Both use RFC3339Nano so second-or-better precision is visible.
	ResetAt         string `json:"reset_at,omitempty"`
	ResetAtUTC      string `json:"reset_at_utc,omitempty"`
	CurrentPriority *int   `json:"current_priority,omitempty"`
	DesiredPriority int    `json:"desired_priority"`
	LastSuccessAt   string `json:"last_quota_refresh,omitempty"`
	ObservedAt      string `json:"observed_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	WriteError      string `json:"write_error,omitempty"`
}

// publishStatusLocked rebuilds the published snapshot. Caller holds e.mu.
func (e *Engine) publishStatusLocked(now time.Time) {
	snap := Snapshot{
		GeneratedAt:     now,
		DryRun:          e.cfg.DryRun,
		Enabled:         e.cfg.Enabled,
		Stopped:         e.stopped,
		DisplayTimezone: e.cfg.DisplayTimezone,
		Warnings:        append([]string(nil), e.cfg.Warnings...),
		RosterError:     e.lastRosterError,
	}
	if !e.nextReconcileAt.IsZero() {
		t := e.nextReconcileAt
		snap.NextReconcileAt = &t
	}
	if !e.nextDeadlineAt.IsZero() {
		t := e.nextDeadlineAt
		snap.NextDeadlineAt = &t
	}

	groups := make(map[string][]AccountStatus)
	for _, acct := range e.accounts {
		row := AccountStatus{
			AuthIndex:        acct.authIndex,
			Name:             acct.name,
			Label:            acct.label,
			Provider:         acct.provider,
			Health:           string(acct.health),
			QuarantineReason: acct.quarantineReason,
			ResetState:       string(acct.resetState),
			DesiredPriority:  acct.desired,
			LastError:        sanitize.Message(acct.lastError),
			WriteError:       sanitize.Message(acct.writeError),
		}
		if !acct.resetAt.IsZero() {
			row.ResetAt = acct.resetAt.Format(time.RFC3339Nano)
			row.ResetAtUTC = acct.resetAt.UTC().Format(time.RFC3339Nano)
		}
		if acct.currentKnown {
			p := acct.currentPriority
			row.CurrentPriority = &p
		}
		if !acct.lastSuccessAt.IsZero() {
			row.LastSuccessAt = acct.lastSuccessAt.UTC().Format(time.RFC3339Nano)
		}
		if !acct.observedAt.IsZero() {
			row.ObservedAt = acct.observedAt.UTC().Format(time.RFC3339Nano)
		}
		groups[acct.provider] = append(groups[acct.provider], row)
	}

	for _, provider := range []string{config.ProviderClaude, config.ProviderCodex} {
		rows, ok := groups[provider]
		if !ok {
			if e.cfg.Manages(provider) {
				snap.Providers = append(snap.Providers, ProviderGroup{Provider: provider, Accounts: []AccountStatus{}})
			}
			continue
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].DesiredPriority != rows[j].DesiredPriority {
				return rows[i].DesiredPriority > rows[j].DesiredPriority
			}
			return rows[i].Name < rows[j].Name
		})
		snap.Providers = append(snap.Providers, ProviderGroup{Provider: provider, Accounts: rows})
	}
	e.status = snap
}
