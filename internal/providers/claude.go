package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

const (
	// claudeUsageURL is Anthropic's fixed OAuth usage endpoint.
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// claudeOAuthBeta is the anthropic-beta value required for OAuth
	// credentials (matches the value CLIProxyAPI itself sends).
	claudeOAuthBeta = "oauth-2025-04-20"
)

// Claude fetches the regular account-wide seven-day reset timestamp.
type Claude struct {
	http HTTPDoer
	now  func() time.Time
}

// NewClaude builds the Claude provider. now supplies observation timestamps.
func NewClaude(http HTTPDoer, now func() time.Time) *Claude {
	return &Claude{http: http, now: now}
}

// ID implements Provider.
func (c *Claude) ID() string { return "claude" }

// claudeUsage decodes only the regular seven-day window. Every other field
// in the response (five_hour, model-scoped weekly windows such as
// seven_day_opus, utilization percentages, token counts, plan type, ...) is
// intentionally not modeled so it cannot leak into ranking.
type claudeUsage struct {
	SevenDay *claudeWindow `json:"seven_day"`
}

type claudeWindow struct {
	// ResetsAt is the exact reset timestamp. Only RFC3339/RFC3339Nano
	// strings are accepted; Anthropic documents resets_at as a timestamp
	// string, so numeric shapes are rejected rather than guessed at (a bare
	// number has ambiguous units — seconds vs milliseconds). Utilization is
	// deliberately not decoded: percent used must never affect ordering.
	ResetsAt any `json:"resets_at"`
}

// FetchWeeklyReset implements Provider.
func (c *Claude) FetchWeeklyReset(ctx context.Context, creds Credentials) (Observation, error) {
	body, err := fetchUsageBody(ctx, c.http, claudeUsageRequest(creds), c.ID())
	if err != nil {
		return Observation{}, err
	}
	observedAt := c.now()
	var usage claudeUsage
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return Observation{}, fmt.Errorf("claude usage response is not valid JSON")
	}
	if usage.SevenDay == nil {
		return Observation{ObservedAt: observedAt}, nil
	}
	resetAt, ok := parseRFC3339Timestamp(usage.SevenDay.ResetsAt)
	if !ok {
		return Observation{ObservedAt: observedAt}, nil
	}
	return Observation{HasWeekly: true, ResetAt: resetAt, ObservedAt: observedAt}, nil
}

func claudeUsageRequest(creds Credentials) hostapi.HTTPRequest {
	return hostapi.HTTPRequest{
		Method: "GET",
		URL:    claudeUsageURL,
		Headers: map[string][]string{
			"Authorization":  {"Bearer " + creds.AccessToken},
			"Accept":         {"application/json"},
			"anthropic-beta": {claudeOAuthBeta},
		},
	}
}
