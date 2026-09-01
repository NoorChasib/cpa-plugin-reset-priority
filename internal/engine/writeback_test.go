package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

// richDoc builds an auth JSON doc with many unrelated fields, including
// nested structures, that must survive priority writes untouched.
func richDoc(name string) map[string]any {
	return map[string]any{
		"type":          "claude",
		"access_token":  token(name),
		"refresh_token": "refresh-" + token(name),
		"email":         name + "@example.com",
		"expired":       "2026-09-08T00:00:00Z",
		"priority":      "250", // string form: host parses trimmed decimals
		"custom_note":   "do not touch",
		"nested":        map[string]any{"keep": []any{1, 2, 3}, "flag": true},
		"account_uuid":  "uuid-" + name,
	}
}

func TestWritebackMutatesOnlyPriority(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	original := richDoc("a")
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-a",
		Name:      "a.json",
		Provider:  "claude",
		Status:    "active",
		Source:    "file",
		Path:      "/auth/a.json",
		Priority:  250,
	}, original)
	env.claude.setReset(token("a"), day(1))
	env.reconcile()

	saves := env.host.savesFor("a.json")
	if len(saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(saves))
	}
	saved := saves[0].Doc
	var priority int
	if err := json.Unmarshal(saved["priority"], &priority); err != nil || priority != 100 {
		t.Errorf("saved priority = %s, want 100", saved["priority"])
	}
	for key, wantVal := range original {
		if key == "priority" {
			continue
		}
		var gotVal any
		if err := json.Unmarshal(saved[key], &gotVal); err != nil {
			t.Fatalf("saved doc missing field %q", key)
		}
		var wantNorm any
		wantRaw, _ := json.Marshal(wantVal)
		_ = json.Unmarshal(wantRaw, &wantNorm)
		if !reflect.DeepEqual(gotVal, wantNorm) {
			t.Errorf("field %q changed: got %v, want %v", key, gotVal, wantNorm)
		}
	}
	if len(saved) != len(original) {
		t.Errorf("saved doc has %d fields, want %d", len(saved), len(original))
	}
}

func TestWritebackNoWriteWhenPriorityAlreadyMatches(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	doc := richDoc("a")
	doc["priority"] = 100
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-a",
		Name:      "a.json",
		Provider:  "claude",
		Status:    "active",
		Source:    "file",
		Path:      "/auth/a.json",
		Priority:  100,
	}, doc)
	env.claude.setReset(token("a"), day(1))
	env.reconcile()
	if got := env.host.saveCount(); got != 0 {
		t.Errorf("saves = %d, want 0 (priority already matches)", got)
	}

	// Repeated reconciliations stay write-free.
	env.clk.Advance(time.Hour)
	if got := env.host.saveCount(); got != 0 {
		t.Errorf("saves after hourly reconcile = %d, want 0", got)
	}
}

func TestWritebackAmbiguousListPriorityVerifiedByGetWithoutSave(t *testing.T) {
	// The list value is ambiguous (omitempty zero) but the latest physical
	// JSON already matches the target: a read happens, no save.
	env := newTestEnv(t, defaultConfig())
	doc := richDoc("a")
	doc["priority"] = 100
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-a",
		Name:      "a.json",
		Provider:  "claude",
		Status:    "active",
		Source:    "file",
		Path:      "/auth/a.json",
		// Priority omitted (zero): ambiguous.
	}, doc)
	env.claude.setReset(token("a"), day(1))
	env.reconcile()
	if got := env.host.saveCount(); got != 0 {
		t.Errorf("saves = %d, want 0 (latest physical value already matches)", got)
	}
}

func TestWritebackReReadsLatestAndPreservesConcurrentChanges(t *testing.T) {
	// A concurrent token refresh lands between discovery and the write; the
	// saved document must carry the refreshed value (read latest, mutate
	// only priority).
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.host.beforeGet = func(authIndex string) {
		env.host.mu.Lock()
		defer env.host.mu.Unlock()
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(env.host.docs["idx-a"], &doc)
		doc["refresh_token"] = json.RawMessage(`"rotated-by-concurrent-refresh"`)
		raw, _ := json.Marshal(doc)
		env.host.docs["idx-a"] = raw
	}
	env.reconcile()

	saves := env.host.savesFor("a.json")
	if len(saves) == 0 {
		t.Fatalf("no save recorded")
	}
	last := saves[len(saves)-1].Doc
	var refresh string
	if err := json.Unmarshal(last["refresh_token"], &refresh); err != nil || refresh != "rotated-by-concurrent-refresh" {
		t.Errorf("saved refresh_token = %s, want the concurrently rotated value", last["refresh_token"])
	}
	var priority int
	if err := json.Unmarshal(last["priority"], &priority); err != nil || priority != 100 {
		t.Errorf("saved priority = %s, want 100", last["priority"])
	}
}

func TestWritebackRefusesDocumentReplacedWithNonOAuthAuth(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	getCalls := 0
	env.host.beforeGet = func(authIndex string) {
		if authIndex != "idx-a" {
			return
		}
		getCalls++
		if getCalls != 3 { // validation, provider fetch, then read-before-write
			return
		}
		env.host.mu.Lock()
		defer env.host.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{
			"type":        "claude",
			"api_key":     "<API_KEY_PLACEHOLDER>",
			"custom_note": "replacement must not be mutated",
		})
		env.host.docs[authIndex] = raw
	}

	env.reconcile()
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("non-OAuth replacement was saved %d times, want 0", got)
	}
	row, _ := env.statusRow("a")
	if !strings.Contains(row.WriteError, "OAuth credential shape") {
		t.Fatalf("write refusal was not surfaced: %q", row.WriteError)
	}
	if _, ok := env.host.docPriority(t, "idx-a"); ok {
		t.Fatalf("non-OAuth replacement unexpectedly gained a priority field")
	}
}

func TestWritebackOneFailedSaveDoesNotAbortOthers(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.addAccount("claude", "c", day(3))
	env.host.saveErr["b.json"] = errFake("disk full")
	env.reconcile()

	// a and c were written despite b's failure.
	env.assertPhysical(map[string]int{"a": 300, "c": 100})
	row, _ := env.statusRow("b")
	if row.WriteError == "" {
		t.Errorf("failed save not surfaced in status for b")
	}

	// The failure clears and the write is retried on the next pass.
	delete(env.host.saveErr, "b.json")
	env.clk.Advance(time.Hour)
	env.assertPhysical(map[string]int{"a": 300, "b": 200, "c": 100})
}

func TestWritebackAuthDisappearsBetweenDiscoveryAndSave(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.host.getErr["idx-a"] = errFake("auth not found")
	env.reconcile()

	// a could not be validated from its physical OAuth JSON and therefore
	// never enters ranking/writeback. b is the only managed account.
	env.assertPhysical(map[string]int{"b": 100})
	if saves := env.host.savesFor("a.json"); len(saves) != 0 {
		t.Errorf("unreadable auth a was saved")
	}
}

func TestDryRunPerformsZeroSaves(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()

	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("dry-run performed %d saves, want 0", got)
	}
	// Desired priorities are still computed, exposed, and scheduled.
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	if !env.eng.Status().DryRun {
		t.Errorf("status does not report dry-run")
	}
	if env.eng.Status().NextDeadlineAt == nil {
		t.Errorf("dry-run did not schedule the deadline timer")
	}

	// Deadline demotions also stay write-free in dry-run.
	env.clk.AdvanceTo(day(1))
	env.async.drain()
	if got := env.host.saveCount(); got != 0 {
		t.Errorf("dry-run deadline handling performed %d saves, want 0", got)
	}
}

func TestWritebackStringPriorityComparedNumerically(t *testing.T) {
	// A physical string priority equal to the target is a no-op.
	env := newTestEnv(t, defaultConfig())
	doc := richDoc("a")
	doc["priority"] = "100"
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-a",
		Name:      "a.json",
		Provider:  "claude",
		Status:    "active",
		Source:    "file",
		Path:      "/auth/a.json",
	}, doc)
	env.claude.setReset(token("a"), day(1))
	env.reconcile()
	if got := env.host.saveCount(); got != 0 {
		t.Errorf("saves = %d, want 0 (string \"100\" == 100)", got)
	}
}
