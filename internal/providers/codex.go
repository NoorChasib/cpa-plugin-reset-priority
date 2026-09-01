package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

// codexUsageURL is the ChatGPT/Codex usage probe endpoint.
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// codexUserAgent is the CLI-shaped User-Agent sent with wham/usage requests.
// The ChatGPT backend expects Codex CLI-style clients on this endpoint;
// requests without a CLI-shaped User-Agent may be rejected by edge filtering.
// The comment segment honestly identifies this plugin as the actual caller.
const codexUserAgent = "codex_cli_rs/0.0.0 (cpa-plugin-reset-priority)"

// weeklyWindowSeconds identifies the regular weekly quota window. The window
// is identified strictly by its duration semantics, never by array/object
// position (spec section 2.2).
const (
	weeklyWindowSeconds = 604800
	weeklyWindowMinutes = 10080 // exactly 604800 seconds
)

// Codex fetches the regular weekly quota window reset timestamp.
type Codex struct {
	http HTTPDoer
	now  func() time.Time
}

// NewCodex builds the Codex provider. now supplies observation timestamps.
func NewCodex(http HTTPDoer, now func() time.Time) *Codex {
	return &Codex{http: http, now: now}
}

// ID implements Provider.
func (c *Codex) ID() string { return "codex" }

// FetchWeeklyReset implements Provider.
func (c *Codex) FetchWeeklyReset(ctx context.Context, creds Credentials) (Observation, error) {
	// Fail closed before any HTTP: the wham/usage endpoint requires the
	// Chatgpt-Account-Id routing header, so an unresolvable account ID means
	// the probe cannot succeed. The error is static and never echoes any
	// credential material.
	if creds.AccountID == "" {
		return Observation{}, fmt.Errorf("codex account id could not be resolved from credentials")
	}
	body, err := fetchUsageBody(ctx, c.http, hostapi.HTTPRequest{
		Method: "GET",
		URL:    codexUsageURL,
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + creds.AccessToken},
			"Accept":             {"application/json"},
			"User-Agent":         {codexUserAgent},
			"Chatgpt-Account-Id": {creds.AccountID},
		},
	}, c.ID())
	if err != nil {
		return Observation{}, err
	}
	observedAt := c.now()
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return Observation{}, fmt.Errorf("codex usage response is not valid JSON")
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return Observation{}, fmt.Errorf("codex usage response is not valid JSON")
	}
	resetAt, ok := findWeeklyReset(root, observedAt)
	if !ok {
		return Observation{ObservedAt: observedAt}, nil
	}
	return Observation{HasWeekly: true, ResetAt: resetAt, ObservedAt: observedAt}, nil
}

// findWeeklyReset locates the regular weekly window and extracts its exact
// reset timestamp.
//
// Search space, deliberately narrow:
//
//   - rate_limit / rateLimit  -> primary_window / secondary_window
//   - rate_limits / rateLimits -> primary / secondary (and window alternates)
//
// The following are intentionally NEVER inspected, so they cannot influence
// ordering (spec section 2.2): additional_rate_limits, code_review limits,
// rate_limit_reset_credits, credits, plan_type, used_percent, monthly
// windows, and the five-hour (18000 s) window.
func findWeeklyReset(root map[string]any, observedAt time.Time) (time.Time, bool) {
	for _, window := range candidateWindows(root) {
		if !isWeeklyWindow(window) {
			continue
		}
		if resetAt, ok := windowResetAt(window, observedAt); ok {
			return resetAt, true
		}
	}
	return time.Time{}, false
}

// candidateWindows collects rate-limit window objects in a fixed,
// deterministic order.
func candidateWindows(root map[string]any) []map[string]any {
	var out []map[string]any
	appendWindows := func(envelope map[string]any) {
		for _, key := range []string{
			"primary_window", "primaryWindow",
			"secondary_window", "secondaryWindow",
			"primary", "secondary",
		} {
			if v, ok := envelope[key]; ok {
				if window, okObj := asObject(v); okObj {
					out = append(out, window)
				}
			}
		}
	}
	for _, key := range []string{"rate_limit", "rateLimit", "rate_limits", "rateLimits"} {
		if v, ok := root[key]; ok {
			if envelope, okObj := asObject(v); okObj {
				appendWindows(envelope)
			}
		}
	}
	// Some payload variants place the windows at the top level.
	appendWindows(root)
	return out
}

// windowDurationAliases lists the duration declarations that identify a
// window, in preference order. Each family groups the snake_case and camelCase
// spellings of one declaration with the weekly duration expressed in that
// family's own unit, so the comparison never needs a unit conversion.
var windowDurationAliases = []struct {
	keys []string
	week int64
}{
	{keys: []string{"limit_window_seconds", "limitWindowSeconds"}, week: weeklyWindowSeconds},
	{keys: []string{"window_minutes", "windowMinutes"}, week: weeklyWindowMinutes},
}

// isWeeklyWindow reports whether a window's declared duration is exactly one
// week.
//
// Families are consulted in order and, within a family, every spelling is
// tried until one yields a usable number: a wrong-typed or non-integral value
// declares nothing and must not shadow the sibling spelling or the next
// family. The first usable value then decides outright, so a window that
// genuinely declares a non-weekly duration still fails closed and the
// five-hour window can never be promoted by a contradicting alias. A window
// with no usable duration at all is never assumed to be weekly.
func isWeeklyWindow(window map[string]any) bool {
	for _, family := range windowDurationAliases {
		if declared, ok := firstParsed(window, exactInteger, family.keys...); ok {
			return declared.Cmp(big.NewRat(family.week, 1)) == 0
		}
	}
	return false
}

// exactInteger narrows a decoded JSON number to an exact integer without
// rounding. Codex duration declarations must be finite and integral;
// truncating a fractional value is forbidden. The result stays a big.Rat so
// that an integral value too large for int64 remains a usable (merely
// non-weekly) declaration rather than being mistaken for a missing one.
func exactInteger(raw any) (*big.Rat, bool) {
	v, ok := raw.(json.Number)
	if !ok {
		return nil, false
	}
	n, ok := new(big.Rat).SetString(v.String())
	if !ok || !n.IsInt() {
		return nil, false
	}
	return n, true
}

// resetAtPlausibilitySlack absorbs modest provider/host clock disagreement
// when bounding absolute reset_at values around the observation time.
const resetAtPlausibilitySlack = time.Hour

// windowResetAt extracts the exact reset instant, preferring an absolute
// reset_at timestamp and falling back to reset_after_seconds relative to the
// observation time (spec section 8).
//
// Both alias families are scanned to exhaustion, so an unparseable or
// implausible reset_at never suppresses a valid resetAt (nor a valid relative
// offset); only the absence of any usable value in a family moves on to the
// next one.
func windowResetAt(window map[string]any, observedAt time.Time) (time.Time, bool) {
	parseAbsolute := func(raw any) (time.Time, bool) {
		switch value := raw.(type) {
		case string:
			t, ok := parseRFC3339Timestamp(value)
			return t, ok && plausibleResetAt(t, observedAt)
		case json.Number:
			return parseUnixSecondsResetAt(value, observedAt)
		}
		return time.Time{}, false
	}
	if t, ok := firstParsed(window, parseAbsolute, "reset_at", "resetAt"); ok {
		return t, true
	}
	if d, ok := firstParsed(window, parseResetAfterSeconds, "reset_after_seconds", "resetAfterSeconds"); ok {
		return observedAt.Add(d), true
	}
	return time.Time{}, false
}

// parseUnixSecondsResetAt parses a numeric reset_at as EXACT integer Unix
// seconds. The response is decoded with json.Decoder.UseNumber, so the raw
// digits reach strconv.ParseInt untouched: fractional values, exponent
// notation, and anything that overflows int64 are rejected outright, never
// rounded or truncated.
//
// The instant must also be plausible for a weekly window: within one weekly
// window (plus clock-skew slack) of the observation time on either side.
// This rejects millisecond-shaped epochs (~1000x too large), other
// wrong-unit values, and implausibly distant instants that cannot be the
// regular weekly reset, instead of silently producing a deadline centuries
// away (or in 1970) that would dominate or vanish from the ranking.
func parseUnixSecondsResetAt(num json.Number, observedAt time.Time) (time.Time, bool) {
	sec, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	t := time.Unix(sec, 0).UTC()
	if !plausibleResetAt(t, observedAt) {
		return time.Time{}, false
	}
	return t, true
}

func plausibleResetAt(resetAt, observedAt time.Time) bool {
	const window = weeklyWindowSeconds*time.Second + resetAtPlausibilitySlack
	return !resetAt.Before(observedAt.Add(-window)) && !resetAt.After(observedAt.Add(window))
}

// parseResetAfterSeconds validates a relative reset offset. Fractional
// seconds are allowed and preserved (spec section 8: second-level or better
// precision), but the offset must be finite and within
// [0, weeklyWindowSeconds]: the regular weekly window can never be further
// than one full window away from resetting, and the bound also keeps the
// float64 -> time.Duration conversion far from overflow (604800 s is
// ~6.05e14 ns, versus math.MaxInt64 ~9.22e18 ns).
func parseResetAfterSeconds(raw any) (time.Duration, bool) {
	v, ok := raw.(json.Number)
	if !ok {
		return 0, false
	}
	seconds, err := v.Float64()
	if err != nil {
		return 0, false
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > weeklyWindowSeconds {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}
