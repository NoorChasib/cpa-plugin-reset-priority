package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/sanitize"
)

// writeItem is one planned priority write.
type writeItem struct {
	authIndex string
	name      string
	provider  string
	desired   int
	health    Health
	disabled  bool
}

// writePlanLocked selects accounts whose physical priority may need to
// change. An account is skipped when its best-known current priority already
// equals the desired priority; ambiguity (unknown current) forces a
// read-and-verify pass, which still results in no save when the latest
// physical value already matches.
func (e *Engine) writePlanLocked() []writeItem {
	var plan []writeItem
	for _, acct := range e.accounts {
		// Any definitive quarantine status makes host.auth.save unsafe because
		// CPA's save/upsert path can reactivate the credential.
		if acct.health == HealthQuarantined {
			continue
		}
		if acct.currentKnown && acct.currentPriority == acct.desired {
			continue
		}
		plan = append(plan, writeItem{
			authIndex: acct.authIndex,
			name:      acct.name,
			provider:  acct.provider,
			desired:   acct.desired,
			health:    acct.health,
			disabled:  acct.disabled,
		})
	}
	return plan
}

// performWrites applies the plan through host callbacks.
//
// Safety rules (spec section 13, plus the audited host.auth.save hazards of
// CLIProxyAPI commit 81e1b5374f99c212f196f34956eeed964a46b8fa):
//
//   - host.auth.save is WHOLE-DOCUMENT replacement with no compare-and-swap
//     or field patch, so the latest physical JSON is re-read via
//     host.auth.get immediately before writing and ONLY the top-level
//     "priority" field is mutated; every other field is preserved verbatim
//     (values byte-for-byte via json.RawMessage).
//   - The save-side upsert path can synthesize an active runtime record, so
//     saving any credential CPA definitively reports as quarantined (disabled,
//     unauthorized, revoked, reauth-required) could silently RE-ACTIVATE it.
//     Quarantined credentials are therefore never written. A recovering auth
//     becomes writable only after a later roster explicitly reports it healthy.
//   - A failed save for one account never aborts the rest of the pass.
//   - Dry-run performs zero host.auth.save calls (spec section 16).
//   - Auth files are never read or written directly; only host callbacks.
//   - Write passes are serialized (writeMu) and every planned write is
//     revalidated against live state immediately before executing: if the
//     account disappeared or a newer pass (e.g. an exact-deadline demotion)
//     changed the desired priority while this plan waited, the stale entry
//     is skipped instead of overwriting the newer value. Should a change
//     land during the save itself, the newer pass's own serialized write
//     runs afterwards and converges the physical state.
func (e *Engine) performWrites(ctx context.Context, plan []writeItem) {
	if len(plan) == 0 {
		return
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	for _, item := range plan {
		// Revalidate against live state under the state lock.
		e.mu.Lock()
		acct := e.accounts[item.authIndex]
		if e.stopped || acct == nil || acct.desired != item.desired || acct.health != item.health || acct.health == HealthQuarantined {
			e.mu.Unlock()
			continue
		}
		item.name = acct.name
		item.disabled = acct.disabled
		dryRun := e.cfg.DryRun
		e.mu.Unlock()

		if dryRun {
			e.logf("info", fmt.Sprintf(
				"reset-priority dry-run: would set priority of %s auth %s to %d",
				item.provider, item.name, item.desired))
			continue
		}
		if item.disabled {
			// Defensive backstop for inconsistent roster health fields.
			e.logf("info", fmt.Sprintf(
				"reset-priority: skipping unsafe priority write for disabled %s auth %s",
				item.provider, item.name))
			continue
		}
		e.writeOne(ctx, item)
	}
}

// writeOne performs the read-latest / mutate-only-priority / no-op-skip /
// save flow for a single account.
func (e *Engine) writeOne(ctx context.Context, item writeItem) {
	got, errGet := e.host.AuthGet(ctx, item.authIndex)
	if errGet != nil {
		// The auth may have disappeared between discovery and save; treat
		// as a normal concurrent roster change (spec section 14).
		e.recordWriteError(item.authIndex, "read-before-write failed: "+shortErr(errGet))
		return
	}
	if got.AuthIndex != "" && got.AuthIndex != item.authIndex {
		e.recordWriteError(item.authIndex, "read-before-write returned a different auth index")
		return
	}
	if got.Name != "" && got.Name != item.name {
		e.recordWriteError(item.authIndex, "read-before-write returned a different physical auth name")
		return
	}
	// Revalidate the latest document as the managed provider's OAuth shape.
	// Discovery can race an operator replacing a file with a non-OAuth auth;
	// never mutate that replacement merely because the prior roster was valid.
	if _, errCreds := providers.ExtractCredentials(item.provider, got.JSON); errCreds != nil {
		e.recordWriteError(item.authIndex, "read-before-write no longer has a managed OAuth credential shape")
		return
	}

	// Decode into RawMessage values so unrelated fields survive verbatim.
	var doc map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(got.JSON, &doc); errUnmarshal != nil || doc == nil {
		e.recordWriteError(item.authIndex, "auth json is not an object; refusing to write")
		return
	}

	if existing, ok := parsePriorityRaw(doc["priority"]); ok && existing == item.desired {
		// Latest physical priority already matches: no write occurs.
		e.updateCurrentPriority(item.authIndex, item.desired)
		return
	}

	doc["priority"] = json.RawMessage(strconv.Itoa(item.desired))
	raw, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		e.recordWriteError(item.authIndex, "re-encode auth json failed")
		return
	}

	if !e.writeItemCurrent(item) {
		return
	}
	name := got.Name
	if name == "" {
		name = item.name
	}
	if errSave := e.host.AuthSave(ctx, name, raw); errSave != nil {
		e.recordWriteError(item.authIndex, "save failed: "+shortErr(errSave))
		return
	}
	e.updateCurrentPriority(item.authIndex, item.desired)
	e.logf("info", fmt.Sprintf(
		"reset-priority: set priority of %s auth %s to %d", item.provider, name, item.desired))
}

func (e *Engine) writeItemCurrent(item writeItem) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	acct := e.accounts[item.authIndex]
	return !e.stopped && acct != nil && acct.desired == item.desired && acct.health == item.health && acct.health != HealthQuarantined
}

func (e *Engine) updateCurrentPriority(authIndex string, priority int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if acct := e.accounts[authIndex]; acct != nil {
		acct.currentPriority = priority
		acct.currentKnown = true
		if acct.health == HealthRecovering && priority == e.cfg.QuarantinePriority {
			acct.recoverySentinelReady = true
		}
		acct.writeError = ""
	}
}

func (e *Engine) recordWriteError(authIndex, message string) {
	e.mu.Lock()
	if acct := e.accounts[authIndex]; acct != nil {
		acct.writeError = message
	}
	e.mu.Unlock()
	// Cross-ABI host logging must never run under Engine.mu (see Logger).
	e.logf("warn", "reset-priority: "+message)
}

// parsePriorityRaw reads an existing top-level priority as a JSON number or
// a numeric string, mirroring the host's tolerant parsing.
func parsePriorityRaw(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var asFloat float64
	if err := json.Unmarshal(raw, &asFloat); err == nil {
		return int(asFloat), true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if v, errConv := strconv.Atoi(strings.TrimSpace(asString)); errConv == nil {
			return v, true
		}
	}
	return 0, false
}

func shortErr(err error) string {
	return sanitize.Error(err)
}
