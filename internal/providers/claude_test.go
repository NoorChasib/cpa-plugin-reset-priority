package providers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// captureDoer records the request and returns a canned response.
type captureDoer struct {
	lastReq hostapi.HTTPRequest
	calls   int
	status  int
	body    string
	err     error
}

func (d *captureDoer) HTTPDo(ctx context.Context, req hostapi.HTTPRequest) (hostapi.HTTPResponse, error) {
	d.lastReq = req
	d.calls++
	if d.err != nil {
		return hostapi.HTTPResponse{}, d.err
	}
	status := d.status
	if status == 0 {
		status = 200
	}
	return hostapi.HTTPResponse{StatusCode: status, Body: []byte(d.body)}, nil
}

func nowFn() time.Time { return testNow }

func TestClaudeParsesSevenDayResetExactly(t *testing.T) {
	// Nanosecond-precision timestamp with a non-UTC offset must be
	// preserved exactly — no truncation to days/minutes/seconds.
	doer := &captureDoer{body: `{
		"five_hour": {"utilization": 88, "resets_at": "2026-09-01T13:00:00Z"},
		"seven_day": {"utilization": 42.5, "resets_at": "2026-09-02T03:00:00.123456789-07:00"},
		"seven_day_opus": {"utilization": 99, "resets_at": "2026-09-01T14:00:00Z"}
	}`}
	c := NewClaude(doer, nowFn)
	obs, err := c.FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("FetchWeeklyReset: %v", err)
	}
	if !obs.HasWeekly {
		t.Fatalf("HasWeekly = false, want true")
	}
	zone := time.FixedZone("", -7*3600)
	want := time.Date(2026, 9, 2, 3, 0, 0, 123456789, zone)
	if !obs.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %s, want %s", obs.ResetAt, want)
	}
	if obs.ResetAt.Nanosecond() != 123456789 {
		t.Errorf("nanoseconds truncated: %d", obs.ResetAt.Nanosecond())
	}
	if !obs.ObservedAt.Equal(testNow) {
		t.Errorf("ObservedAt = %s, want %s", obs.ObservedAt, testNow)
	}
}

func TestClaudeTimezoneOffsetsRepresentSameInstant(t *testing.T) {
	fetch := func(ts string) time.Time {
		doer := &captureDoer{body: `{"seven_day": {"resets_at": "` + ts + `"}}`}
		obs, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
		if err != nil || !obs.HasWeekly {
			t.Fatalf("fetch %s: %v", ts, err)
		}
		return obs.ResetAt
	}
	a := fetch("2026-09-02T03:00:00-07:00")
	b := fetch("2026-09-02T10:00:00Z")
	if !a.Equal(b) {
		t.Errorf("same instant in different zones compares unequal: %s vs %s", a, b)
	}
}

func TestClaudeRequestShape(t *testing.T) {
	doer := &captureDoer{body: `{"seven_day":{"resets_at":"2026-09-02T03:00:00Z"}}`}
	_, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "secret-token"})
	if err != nil {
		t.Fatalf("FetchWeeklyReset: %v", err)
	}
	req := doer.lastReq
	if req.URL != "https://api.anthropic.com/api/oauth/usage" {
		t.Errorf("URL = %s", req.URL)
	}
	if req.Method != "GET" {
		t.Errorf("Method = %s, want GET", req.Method)
	}
	if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer secret-token" {
		t.Errorf("Authorization = %v", got)
	}
	if got := req.Headers["anthropic-beta"]; len(got) != 1 || got[0] != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %v", got)
	}
}

func TestClaudeMissingSevenDayIsNotWeekly(t *testing.T) {
	for name, body := range map[string]string{
		"no seven_day":      `{"five_hour": {"resets_at": "2026-09-01T13:00:00Z"}}`,
		"null resets_at":    `{"seven_day": {"utilization": 10, "resets_at": null}}`,
		"missing resets_at": `{"seven_day": {"utilization": 10}}`,
		"unparsable":        `{"seven_day": {"resets_at": "Sep 2"}}`,
		"empty object":      `{}`,
	} {
		doer := &captureDoer{body: body}
		obs, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if obs.HasWeekly {
			t.Errorf("%s: HasWeekly = true, want false", name)
		}
	}
}

func TestClaudeModelScopedWeeklyOnlyIsNotWeekly(t *testing.T) {
	// Model-scoped weekly windows (seven_day_opus, seven_day_sonnet, ...) must
	// never substitute for the regular account-wide seven_day window, even
	// when they carry perfectly parseable future reset timestamps.
	doer := &captureDoer{body: `{
		"five_hour": {"utilization": 12, "resets_at": "2026-09-01T13:00:00Z"},
		"seven_day_opus": {"utilization": 55, "resets_at": "2026-09-03T00:00:00Z"},
		"seven_day_sonnet": {"utilization": 31, "resets_at": "2026-09-04T00:00:00Z"}
	}`}
	obs, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("FetchWeeklyReset: %v", err)
	}
	if obs.HasWeekly {
		t.Errorf("model-scoped weekly window accepted as the regular weekly window: HasWeekly = true")
	}
	if !obs.ResetAt.IsZero() {
		t.Errorf("ResetAt = %s, want zero (no regular weekly window)", obs.ResetAt)
	}
	if !obs.ObservedAt.Equal(testNow) {
		t.Errorf("ObservedAt = %s, want %s", obs.ObservedAt, testNow)
	}
}

func TestClaudeNonStringResetsAtRejected(t *testing.T) {
	// seven_day.resets_at must be an RFC3339/RFC3339Nano string. Numeric
	// epochs have ambiguous units (seconds vs milliseconds) and are not what
	// Anthropic documents, so they are rejected rather than guessed at.
	for name, body := range map[string]string{
		"integer epoch seconds": `{"seven_day": {"resets_at": 1788648000}}`,
		"fractional epoch":      `{"seven_day": {"resets_at": 1788648000.25}}`,
		"millisecond epoch":     `{"seven_day": {"resets_at": 1788648000000}}`,
		"boolean":               `{"seven_day": {"resets_at": true}}`,
		"object":                `{"seven_day": {"resets_at": {"unix": 1788648000}}}`,
		"array":                 `{"seven_day": {"resets_at": ["2026-09-05T08:30:11Z"]}}`,
		"numeric string":        `{"seven_day": {"resets_at": "1788648000"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			doer := &captureDoer{body: body}
			obs, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
			if err != nil {
				t.Fatalf("FetchWeeklyReset: %v", err)
			}
			if obs.HasWeekly {
				t.Errorf("non-RFC3339 resets_at accepted: HasWeekly = true")
			}
		})
	}
}

func TestClaudeHTTPErrorIsSanitized(t *testing.T) {
	doer := &captureDoer{status: 429, body: `{"secret":"sk-ant-super-secret-response"}`}
	_, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"})
	if err == nil {
		t.Fatalf("want error on HTTP 429")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks response body: %v", err)
	}
}

func TestClaudeInvalidJSONError(t *testing.T) {
	doer := &captureDoer{body: `not json`}
	if _, err := NewClaude(doer, nowFn).FetchWeeklyReset(context.Background(), Credentials{AccessToken: "tok"}); err == nil {
		t.Fatalf("want error on invalid JSON")
	}
}

func TestExtractCredentials(t *testing.T) {
	creds, err := ExtractCredentials("claude", []byte(`{
		"type": "claude", "access_token": " tok-123 ", "refresh_token": "r", "email": "a@b.c"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccessToken != "tok-123" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}

	codexCreds, err := ExtractCredentials("codex", []byte(`{
		"access_token": "tok", "account_id": "acct-9"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials codex: %v", err)
	}
	if codexCreds.AccountID != "acct-9" {
		t.Errorf("AccountID = %q", codexCreds.AccountID)
	}

	if _, err := ExtractCredentials("claude", []byte(`{"type":"claude"}`)); err == nil {
		t.Errorf("want error when access token missing")
	}
	if _, err := ExtractCredentials("claude", []byte(`[]`)); err == nil {
		t.Errorf("want error on non-object auth json")
	}
}
