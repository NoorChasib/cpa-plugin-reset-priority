// Package providers fetches and parses regular weekly quota reset timestamps
// for Claude and Codex OAuth credentials.
//
// Hard product policy (spec section 2): the ONLY value allowed to influence
// credential ordering is the exact reset deadline of the regular weekly
// quota window:
//
//   - Claude: seven_day.resets_at
//   - Codex:  the window whose duration is exactly 604800 seconds, via
//     reset_at/resetAt or reset_after_seconds/resetAfterSeconds
//
// Utilization percentages, five-hour windows, model-scoped windows, monthly
// windows, additional/code-review limits, reset credits, and plan types are
// never parsed into the normalized observation and can never reach the
// ranker.
package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

// HTTPDoer abstracts host.http.do for tests.
type HTTPDoer interface {
	HTTPDo(ctx context.Context, req hostapi.HTTPRequest) (hostapi.HTTPResponse, error)
}

// Credentials carries the minimum provider credentials extracted from the
// physical auth JSON. Values must never be logged.
type Credentials struct {
	AccessToken string
	// AccountID is the ChatGPT account ID (Codex only).
	AccountID string
}

// Observation is the normalized weekly-reset observation. It deliberately
// contains no utilization or non-weekly window data.
type Observation struct {
	// HasWeekly reports whether the provider returned a regular weekly
	// window with a usable reset timestamp.
	HasWeekly bool
	// ResetAt is the exact weekly reset deadline (zone preserved).
	ResetAt time.Time
	// ObservedAt is when the response was observed.
	ObservedAt time.Time
}

// Provider fetches the regular weekly reset for one credential.
type Provider interface {
	ID() string
	FetchWeeklyReset(ctx context.Context, creds Credentials) (Observation, error)
}

// ExtractCredentials pulls the provider credentials from physical auth JSON.
// It reads only the fields required to authenticate the quota request and
// never logs any of them (error messages are static).
//
// Supported shapes:
//   - Claude/Codex direct fields: access_token, account_id.
//   - Nested Codex CLI-style container: tokens.access_token,
//     tokens.account_id, tokens.id_token.
//   - chatgpt_account_id as an alternate account key (direct or nested).
//   - Codex fallback: when no account field is present, the id_token JWT
//     payload is decoded (WITHOUT signature validation — the claims are
//     used solely to recover the Chatgpt-Account-Id routing header, never
//     for identity) looking for the dotted
//     "https://api.openai.com/auth.chatgpt_account_id" claim, the
//     "https://api.openai.com/auth" claim object's chatgpt_account_id, or a
//     top-level chatgpt_account_id claim.
func ExtractCredentials(providerID string, doc json.RawMessage) (Credentials, error) {
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil || root == nil {
		return Credentials{}, fmt.Errorf("parse auth json: invalid document")
	}

	access := stringField(root, "access_token")
	account := firstStringField(root, "account_id", "chatgpt_account_id")
	idToken := stringField(root, "id_token")
	if nested, ok := asObject(root["tokens"]); ok {
		if access == "" {
			access = stringField(nested, "access_token")
		}
		if account == "" {
			account = firstStringField(nested, "account_id", "chatgpt_account_id")
		}
		if idToken == "" {
			idToken = stringField(nested, "id_token")
		}
	}
	if access == "" {
		return Credentials{}, fmt.Errorf("auth json has no access token")
	}

	if providerID == "codex" && account == "" && idToken != "" {
		account = accountIDFromIDToken(idToken)
	}
	return Credentials{AccessToken: access, AccountID: account}, nil
}

// accountIDFromIDToken best-effort decodes a JWT payload to recover the
// ChatGPT account ID. Malformed tokens simply yield "" — the Codex provider
// then fails closed before any HTTP is attempted. No claim is validated or
// trusted for anything beyond this single routing header.
func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if id := stringField(claims, "https://api.openai.com/auth.chatgpt_account_id"); id != "" {
		return id
	}
	if auth, ok := asObject(claims["https://api.openai.com/auth"]); ok {
		if id := stringField(auth, "chatgpt_account_id"); id != "" {
			return id
		}
	}
	return stringField(claims, "chatgpt_account_id")
}

// stringField returns a trimmed string value from a decoded JSON object.
func stringField(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// firstStringField returns the first non-empty string among keys.
func firstStringField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := stringField(obj, key); v != "" {
			return v
		}
	}
	return ""
}

// parseRFC3339Timestamp accepts RFC3339/RFC3339Nano strings ONLY, preserving
// sub-second precision and the zone offset. Reducing timestamps to day/minute
// precision is forbidden by spec section 8. Numeric epochs are deliberately
// not accepted here: providers that support them (Codex) parse them with
// their own exact, bounded integer parser.
func parseRFC3339Timestamp(raw any) (time.Time, bool) {
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// firstKey returns the first present key from a decoded JSON object,
// supporting snake_case and camelCase alternates.
func firstKey(obj map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := obj[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// asObject narrows a decoded JSON value to an object.
func asObject(v any) (map[string]any, bool) {
	obj, ok := v.(map[string]any)
	return obj, ok
}
