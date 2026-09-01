package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// fakeHTTP serves canned provider payloads keyed by bearer token, so policy
// tests can drive the REAL Claude/Codex parsers end-to-end through the
// engine and prove which response fields can and cannot influence ranking.
type fakeHTTP struct {
	bodies map[string]string // token -> response body
}

func (f *fakeHTTP) HTTPDo(ctx context.Context, req hostapi.HTTPRequest) (hostapi.HTTPResponse, error) {
	auth := ""
	for key, values := range req.Headers {
		if strings.EqualFold(key, "Authorization") && len(values) > 0 {
			auth = strings.TrimPrefix(values[0], "Bearer ")
		}
	}
	body, ok := f.bodies[auth]
	if !ok {
		return hostapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":"unknown token"}`)}, nil
	}
	return hostapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
}

// newPolicyEnv builds an engine over the real provider parsers.
func newPolicyEnv(t *testing.T, bodies map[string]string) *testEnv {
	t.Helper()
	env := newTestEnv(t, defaultConfig())
	http := &fakeHTTP{bodies: bodies}
	env.eng.providers = map[string]providers.Provider{
		"claude": providers.NewClaude(http, env.clk.Now),
		"codex":  providers.NewCodex(http, env.clk.Now),
	}
	return env
}

func rfc(t time.Time) string { return t.Format(time.RFC3339Nano) }

// claudeBody builds a full Claude usage payload with distracting windows.
func claudeBody(sevenDayResetsAt string, sevenDayUtilization int, distractors string) string {
	return fmt.Sprintf(`{
		%s
		"seven_day": {"utilization": %d, "resets_at": %s}
	}`, distractors, sevenDayUtilization, sevenDayResetsAt)
}

func TestPolicyClaudeIgnoredSignals(t *testing.T) {
	// A: 99%% weekly used, resets tomorrow. Also carries a five-hour window
	// resetting very soon, model-scoped weekly windows resetting sooner,
	// and high utilization everywhere.
	// B: 1%% weekly used, resets in three days, with "better" utilization
	// and earlier five-hour/model windows.
	// Expected: A higher priority than B — only seven_day.resets_at counts.
	aReset := baseTime.Add(24 * time.Hour)
	bReset := baseTime.Add(72 * time.Hour)

	aBody := claudeBody(`"`+rfc(aReset)+`"`, 99, `
		"five_hour": {"utilization": 100, "resets_at": "`+rfc(baseTime.Add(5*time.Minute))+`"},
		"seven_day_opus": {"utilization": 100, "resets_at": "`+rfc(baseTime.Add(10*time.Minute))+`"},
		"seven_day_sonnet": {"utilization": 100, "resets_at": "`+rfc(baseTime.Add(15*time.Minute))+`"},`)
	bBody := claudeBody(`"`+rfc(bReset)+`"`, 1, `
		"five_hour": {"utilization": 1, "resets_at": "`+rfc(baseTime.Add(2*time.Minute))+`"},
		"seven_day_opus": {"utilization": 1, "resets_at": "`+rfc(baseTime.Add(3*time.Minute))+`"},`)

	env := newPolicyEnv(t, map[string]string{token("a"): aBody, token("b"): bBody})
	env.addAccount("claude", "a", time.Time{})
	env.addAccount("claude", "b", time.Time{})
	env.reconcile()

	env.assertDesired(map[string]int{"a": 200, "b": 100})
	rowA, _ := env.statusRow("a")
	if rowA.ResetAtUTC != aReset.UTC().Format(time.RFC3339Nano) {
		t.Errorf("account a reset = %s, want %s (seven_day only)", rowA.ResetAtUTC, rfc(aReset.UTC()))
	}
}

func TestPolicyCodexIgnoredSignals(t *testing.T) {
	// A: weekly window (secondary, 604800s) resets tomorrow at 99% used;
	// five-hour window resets in minutes; monthly window, additional rate
	// limits, code-review limits, and reset credits all present.
	// B: weekly resets in three days at 1% used.
	aWeekly := baseTime.Add(24 * time.Hour)
	bWeekly := baseTime.Add(72 * time.Hour)

	aBody := `{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": true,
			"primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 120},
			"secondary_window": {"used_percent": 99, "limit_window_seconds": 604800, "reset_at": "` + rfc(aWeekly) + `"}
		},
		"additional_rate_limits": [
			{"limit_name": "GPT-5.3-Codex-Spark", "rate_limit": {"primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_at": "` + rfc(baseTime.Add(time.Minute)) + `"}}}
		],
		"code_review_rate_limit": {"primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_at": "` + rfc(baseTime.Add(2*time.Minute)) + `"}},
		"rate_limit_reset_credits": {"available_count": 5, "total_earned_count": 9}
	}`
	bBody := `{
		"rate_limit": {
			"primary_window": {"used_percent": 1, "limit_window_seconds": 18000, "reset_after_seconds": 60},
			"secondary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_at": "` + rfc(bWeekly) + `"}
		}
	}`

	env := newPolicyEnv(t, map[string]string{token("a"): aBody, token("b"): bBody})
	env.addAccount("codex", "a", time.Time{})
	env.addAccount("codex", "b", time.Time{})
	env.reconcile()

	// Only the regular weekly window ordered them: A first despite 99%
	// usage, limit_reached, and "earlier" distractor windows.
	env.assertDesired(map[string]int{"a": 200, "b": 100})
	rowA, _ := env.statusRow("a")
	if rowA.ResetAtUTC != aWeekly.UTC().Format(time.RFC3339Nano) {
		t.Errorf("account a reset = %s, want %s (weekly window only)", rowA.ResetAtUTC, rfc(aWeekly.UTC()))
	}
}

func TestPolicyCodexWeeklyWindowIdentifiedByDurationNotPosition(t *testing.T) {
	// The weekly window sits in primary_window here (positions swapped);
	// identification must follow the 604800-second duration.
	weekly := baseTime.Add(36 * time.Hour)
	body := `{
		"rate_limit": {
			"primary_window": {"used_percent": 42, "limit_window_seconds": 604800, "reset_at": "` + rfc(weekly) + `"},
			"secondary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_after_seconds": 900}
		}
	}`
	env := newPolicyEnv(t, map[string]string{token("a"): body})
	env.addAccount("codex", "a", time.Time{})
	env.reconcile()

	row, _ := env.statusRow("a")
	if row.ResetState != "confirmed" || row.ResetAtUTC != weekly.UTC().Format(time.RFC3339Nano) {
		t.Errorf("weekly window not identified by duration: state=%s reset=%s", row.ResetState, row.ResetAtUTC)
	}
}

func TestPolicyCodexMonthlyOnlyYieldsUnknown(t *testing.T) {
	// A payload with only five-hour and monthly windows must NOT produce a
	// ranking timestamp: no substitution of other windows.
	body := `{
		"rate_limit": {
			"primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 60},
			"secondary_window": {"used_percent": 10, "limit_window_seconds": 2592000, "reset_at": "` + rfc(baseTime.Add(400*time.Hour)) + `"}
		}
	}`
	env := newPolicyEnv(t, map[string]string{token("a"): body})
	env.addAccount("codex", "a", time.Time{})
	env.reconcile()

	row, _ := env.statusRow("a")
	if row.ResetState != "unknown" {
		t.Errorf("reset state = %s, want unknown (no weekly window)", row.ResetState)
	}
	if row.ResetAt != "" {
		t.Errorf("reset timestamp %q substituted from a non-weekly window", row.ResetAt)
	}
	env.assertDesired(map[string]int{"a": 100})
}

func TestPolicyClaudeFiveHourOnlyYieldsUnknown(t *testing.T) {
	body := `{
		"five_hour": {"utilization": 3, "resets_at": "` + rfc(baseTime.Add(time.Hour)) + `"}
	}`
	env := newPolicyEnv(t, map[string]string{token("a"): body})
	env.addAccount("claude", "a", time.Time{})
	env.reconcile()

	row, _ := env.statusRow("a")
	if row.ResetState != "unknown" || row.ResetAt != "" {
		t.Errorf("five-hour window leaked into ranking: state=%s reset=%q", row.ResetState, row.ResetAt)
	}
}

func TestPolicyUtilizationCannotReorderEqualResets(t *testing.T) {
	// Identical weekly resets with wildly different utilization must fall
	// back to the deterministic stable key, proving utilization is not even
	// a tie-breaker.
	reset := baseTime.Add(48 * time.Hour)
	aBody := claudeBody(`"`+rfc(reset)+`"`, 99, "")
	bBody := claudeBody(`"`+rfc(reset)+`"`, 1, "")
	env := newPolicyEnv(t, map[string]string{token("a"): aBody, token("b"): bBody})
	env.addAccount("claude", "a", time.Time{})
	env.addAccount("claude", "b", time.Time{})
	env.reconcile()
	// Stable key order: a.json < b.json.
	env.assertDesired(map[string]int{"a": 200, "b": 100})
}
