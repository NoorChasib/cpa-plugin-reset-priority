package providers

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

func codexFetch(t *testing.T, body string) (Observation, error) {
	t.Helper()
	doer := &captureDoer{body: body}
	return NewCodex(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok", AccountID: "acct"})
}

func TestCodexWeeklyResetAtRFC3339(t *testing.T) {
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"primary_window": {"used_percent": 3, "limit_window_seconds": 18000, "reset_after_seconds": 10148},
			"secondary_window": {"used_percent": 63, "limit_window_seconds": 604800, "reset_at": "2026-09-05T08:30:11.5Z"}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	want := time.Date(2026, 9, 5, 8, 30, 11, 500000000, time.UTC)
	if !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
}

func TestCodexWeeklyResetAtNumericEpoch(t *testing.T) {
	// 1788523200 = testNow + 3 days: a plausible weekly reset instant.
	obs, err := codexFetch(t, `{
		"rate_limits": {
			"primary": {"used_percent": 3, "window_minutes": 300, "reset_after_seconds": 10148},
			"secondary": {"used_percent": 63, "window_minutes": 10080, "reset_at": 1788523200}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	if got := obs.ResetAt.Unix(); got != 1788523200 {
		t.Errorf("ResetAt unix = %d, want 1788523200", got)
	}
}

func TestCodexNumericResetAtStrictness(t *testing.T) {
	// testNow = 2026-09-01T12:00:00Z = 1788264000. Numeric reset_at must be
	// exact integer Unix seconds within one weekly window (plus one hour of
	// clock-skew slack) of the observation time on either side.
	accept := map[string]string{
		"now":                        `1788264000`,
		"one week ahead":             `1788868800`,
		"future plausibility bound":  `1788872400`, // +7d +1h exactly
		"past plausibility bound":    `1787655600`, // -7d -1h exactly
		"quoted-free positive small": `1788264001`,
	}
	reject := map[string]string{
		"fractional":              `1788523200.5`,
		"exact fractional zero":   `1788523200.0`,
		"exponent form":           `1.7885232e9`,
		"millisecond shaped":      `1788523200000`,
		"huge but int64":          `9223372036854775807`,
		"int64 overflow":          `9223372036854775808`,
		"absurdly huge":           `99999999999999999999999999999999`,
		"beyond future bound":     `1788872401`, // +7d +1h +1s
		"beyond past bound":       `1787655599`, // -7d -1h -1s
		"zero":                    `0`,
		"negative":                `-1788523200`,
		"negative int64 overflow": `-9223372036854775809`,
	}
	body := func(resetAt string) string {
		return `{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_at":` + resetAt + `}}}`
	}
	for name, resetAt := range accept {
		t.Run("accept "+name, func(t *testing.T) {
			obs, err := codexFetch(t, body(resetAt))
			if err != nil || !obs.HasWeekly {
				t.Fatalf("err=%v HasWeekly=%t, want plausible epoch %s accepted", err, obs.HasWeekly, resetAt)
			}
			want, _ := new(big.Rat).SetString(resetAt)
			if got := big.NewRat(obs.ResetAt.Unix(), 1); got.Cmp(want) != 0 {
				t.Errorf("ResetAt unix = %s, want %s", got.RatString(), resetAt)
			}
		})
	}
	for name, resetAt := range reject {
		t.Run("reject "+name, func(t *testing.T) {
			obs, err := codexFetch(t, body(resetAt))
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if obs.HasWeekly {
				t.Errorf("implausible numeric reset_at %s accepted as %s", resetAt, obs.ResetAt)
			}
		})
	}
}

func TestCodexRejectedNumericResetAtFallsBackToResetAfterSeconds(t *testing.T) {
	// A millisecond-shaped reset_at must not poison the window: the bounded
	// relative field still yields the reset instant.
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"secondary_window": {
				"limit_window_seconds": 604800,
				"reset_at": 1788523200000,
				"reset_after_seconds": 3600
			}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	if want := testNow.Add(time.Hour); !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
}

func TestCodexResetAfterSecondsBounds(t *testing.T) {
	body := func(resetAfter string) string {
		return `{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_after_seconds":` + resetAfter + `}}}`
	}
	// Exactly one full window is the largest possible remaining time.
	obs, err := codexFetch(t, body(`604800`))
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	if want := testNow.Add(604800 * time.Second); !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
	for name, resetAfter := range map[string]string{
		"negative":             `-1`,
		"negative fraction":    `-0.5`,
		"beyond weekly window": `604801`,
		"fractionally beyond":  `604800.5`,
		"duration overflow ns": `9223372036854775807`,
		"huge float":           `1e300`,
		"absurd integer":       `99999999999999999999999999999999`,
		"non-numeric string":   `"3600"`,
		"boolean":              `true`,
		"object":               `{"seconds": 3600}`,
	} {
		t.Run(name, func(t *testing.T) {
			obs, err := codexFetch(t, body(resetAfter))
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if obs.HasWeekly {
				t.Errorf("out-of-bounds reset_after_seconds %s accepted as %s", resetAfter, obs.ResetAt)
			}
		})
	}
}

func TestCodexResetAfterSecondsRelativeToObservation(t *testing.T) {
	// Spec section 8: resetAt = responseObservationTime + resetAfterSeconds
	// with second-level or better precision (fractional seconds preserved).
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"secondary_window": {"limit_window_seconds": 604800, "reset_after_seconds": 75420.25}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	want := testNow.Add(75420*time.Second + 250*time.Millisecond)
	if !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
}

func TestCodexCamelCaseVariants(t *testing.T) {
	obs, err := codexFetch(t, `{
		"rateLimit": {
			"primaryWindow": {"usedPercent": 71, "limitWindowSeconds": 18000, "resetAfterSeconds": 60},
			"secondaryWindow": {"usedPercent": 51, "limitWindowSeconds": 604800, "resetAt": "2026-09-06T00:00:00Z"}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	want := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	if !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
}

func TestCodexResetAtPreferredOverResetAfterSeconds(t *testing.T) {
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"secondary_window": {
				"limit_window_seconds": 604800,
				"reset_at": "2026-09-06T00:00:00Z",
				"reset_after_seconds": 1
			}
		}
	}`)
	if err != nil || !obs.HasWeekly {
		t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
	}
	if !obs.ResetAt.Equal(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ResetAt = %s, want the absolute reset_at", obs.ResetAt)
	}
}

func TestCodexWindowIdentifiedByExactDuration(t *testing.T) {
	// 604801 seconds is NOT the weekly window.
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"secondary_window": {"limit_window_seconds": 604801, "reset_at": "2026-09-06T00:00:00Z"}
		}
	}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if obs.HasWeekly {
		t.Errorf("604801-second window misidentified as weekly")
	}
}

func TestCodexWeeklyDurationExactIntegerForms(t *testing.T) {
	for name, field := range map[string]string{
		"seconds integer":  `"limit_window_seconds": 604800`,
		"seconds decimal":  `"limit_window_seconds": 604800.0`,
		"seconds exponent": `"limit_window_seconds": 6.048e5`,
		"minutes integer":  `"window_minutes": 10080`,
		"minutes decimal":  `"window_minutes": 10080.000`,
	} {
		t.Run(name, func(t *testing.T) {
			obs, err := codexFetch(t, `{"rate_limit":{"secondary_window":{`+field+`,"reset_at":"2026-09-06T00:00:00Z"}}}`)
			if err != nil || !obs.HasWeekly {
				t.Fatalf("err=%v HasWeekly=%t", err, obs.HasWeekly)
			}
		})
	}
}

func TestCodexWeeklyDurationRejectsFractionalValuesWithoutRounding(t *testing.T) {
	for name, field := range map[string]string{
		"seconds fraction":       `"limit_window_seconds": 604800.5`,
		"seconds tiny fraction":  `"limit_window_seconds": 604800.00000000001`,
		"seconds below boundary": `"limit_window_seconds": 604799.99999999999`,
		"seconds above boundary": `"limit_window_seconds": 604801`,
		"minutes fraction":       `"window_minutes": 10080.5`,
		"minutes below boundary": `"window_minutes": 10079.999999999999`,
		"minutes above boundary": `"window_minutes": 10081`,
	} {
		t.Run(name, func(t *testing.T) {
			obs, err := codexFetch(t, `{"rate_limit":{"secondary_window":{`+field+`,"reset_at":"2026-09-06T00:00:00Z"}}}`)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if obs.HasWeekly {
				t.Fatalf("fractional/non-weekly duration %s was accepted", field)
			}
		})
	}
}

func TestCodexWeeklyDurationRejectsNonFiniteValues(t *testing.T) {
	for name, raw := range map[string]any{
		"float NaN":           math.NaN(),
		"float positive Inf":  math.Inf(1),
		"float negative Inf":  math.Inf(-1),
		"number NaN":          json.Number("NaN"),
		"number positive Inf": json.Number("Inf"),
		"number negative Inf": json.Number("-Inf"),
	} {
		t.Run(name, func(t *testing.T) {
			if isWeeklyWindow(map[string]any{"limit_window_seconds": raw}) {
				t.Fatalf("non-finite duration %#v was accepted", raw)
			}
		})
	}
}

func TestCodexNoWeeklyWindowVariants(t *testing.T) {
	for name, body := range map[string]string{
		"five hour only":            `{"rate_limit": {"primary_window": {"limit_window_seconds": 18000, "reset_after_seconds": 60}}}`,
		"monthly only":              `{"rate_limit": {"secondary_window": {"limit_window_seconds": 2592000, "reset_at": "2026-10-01T00:00:00Z"}}}`,
		"no duration":               `{"rate_limit": {"secondary_window": {"reset_at": "2026-09-06T00:00:00Z"}}}`,
		"weekly without timestamps": `{"rate_limit": {"secondary_window": {"limit_window_seconds": 604800}}}`,
		"empty":                     `{}`,
	} {
		obs, err := codexFetch(t, body)
		if err != nil {
			t.Errorf("%s: err=%v", name, err)
			continue
		}
		if obs.HasWeekly {
			t.Errorf("%s: HasWeekly = true, want false", name)
		}
	}
}

func TestCodexIgnoresAdditionalAndCodeReviewLimits(t *testing.T) {
	// Weekly-duration windows inside additional_rate_limits or code-review
	// limits must never be picked up, even when the regular envelope has no
	// weekly window at all.
	obs, err := codexFetch(t, `{
		"rate_limit": {
			"primary_window": {"limit_window_seconds": 18000, "reset_after_seconds": 60}
		},
		"additional_rate_limits": [
			{"limit_name": "Spark", "rate_limit": {"primary_window": {"limit_window_seconds": 604800, "reset_at": "2026-09-02T00:00:00Z"}}}
		],
		"code_review_rate_limit": {"primary_window": {"limit_window_seconds": 604800, "reset_at": "2026-09-02T00:00:00Z"}},
		"rate_limit_reset_credits": {"available_count": 3}
	}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if obs.HasWeekly {
		t.Errorf("additional/code-review weekly window leaked into the observation")
	}
}

func TestCodexRequestShape(t *testing.T) {
	doer := &captureDoer{body: `{}`}
	_, err := NewCodex(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "secret", AccountID: "acct-1"})
	if err != nil {
		t.Fatalf("FetchWeeklyReset: %v", err)
	}
	req := doer.lastReq
	if req.URL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Errorf("URL = %s", req.URL)
	}
	if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer secret" {
		t.Errorf("Authorization = %v", got)
	}
	if got := req.Headers["Chatgpt-Account-Id"]; len(got) != 1 || got[0] != "acct-1" {
		t.Errorf("Chatgpt-Account-Id = %v", got)
	}
}

func TestCodexSendsCLIShapedUserAgent(t *testing.T) {
	doer := &captureDoer{body: `{}`}
	_, err := NewCodex(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok", AccountID: "acct-1"})
	if err != nil {
		t.Fatalf("FetchWeeklyReset: %v", err)
	}
	got := doer.lastReq.Headers["User-Agent"]
	if len(got) != 1 {
		t.Fatalf("User-Agent = %v, want exactly one value", got)
	}
	if got[0] != codexUserAgent {
		t.Errorf("User-Agent = %q, want %q", got[0], codexUserAgent)
	}
	if !strings.HasPrefix(got[0], "codex_cli_rs/") {
		t.Errorf("User-Agent %q is not CLI-shaped (want codex_cli_rs/ prefix)", got[0])
	}
}

func TestCodexEmptyAccountIDFailsClosedBeforeHTTP(t *testing.T) {
	doer := &captureDoer{body: `{}`}
	_, err := NewCodex(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "super-secret-token"})
	if err == nil {
		t.Fatalf("want error when the account id cannot be resolved")
	}
	if doer.calls != 0 {
		t.Errorf("HTTPDo called %d times, want 0 (must fail closed before HTTP)", doer.calls)
	}
	// The error must be static and non-secret: no token material, no
	// account data, only a description of the missing account id.
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("error leaks the access token: %v", err)
	}
	if want := "codex account id could not be resolved from credentials"; err.Error() != want {
		t.Errorf("err = %q, want static message %q", err.Error(), want)
	}
}

func TestCodexHTTPErrorIsSanitized(t *testing.T) {
	doer := &captureDoer{status: 500, body: `{"detail":"internal, includes account data"}`}
	_, err := NewCodex(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok", AccountID: "acct"})
	if err == nil {
		t.Fatalf("want error on HTTP 500")
	}
	if strings.Contains(err.Error(), "account data") {
		t.Errorf("error leaks response body: %v", err)
	}
}
