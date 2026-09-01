package providers

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// fakeJWT builds an unsigned JWT-shaped token carrying the given claims.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesignature"
}

func TestExtractCredentialsNestedTokensContainer(t *testing.T) {
	// Codex CLI-style auth.json keeps credentials under "tokens".
	creds, err := ExtractCredentials("codex", []byte(`{
		"OPENAI_API_KEY": null,
		"tokens": {
			"access_token": "tok-nested",
			"refresh_token": "r",
			"account_id": "acct-nested",
			"id_token": "x.y.z"
		},
		"last_refresh": "2026-09-01T00:00:00Z"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccessToken != "tok-nested" || creds.AccountID != "acct-nested" {
		t.Errorf("creds = %+v", creds)
	}
}

func TestExtractCredentialsChatgptAccountIDKey(t *testing.T) {
	creds, err := ExtractCredentials("codex", []byte(`{
		"access_token": "tok",
		"chatgpt_account_id": "acct-direct-alt"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "acct-direct-alt" {
		t.Errorf("AccountID = %q, want acct-direct-alt", creds.AccountID)
	}

	nested, err := ExtractCredentials("codex", []byte(`{
		"tokens": {"access_token": "tok", "chatgpt_account_id": "acct-nested-alt"}
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if nested.AccountID != "acct-nested-alt" {
		t.Errorf("AccountID = %q, want acct-nested-alt", nested.AccountID)
	}
}

func TestExtractCredentialsAccountIDFromIDTokenAuthClaim(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{
		"sub": "user-1",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-from-claim",
			"chatgpt_plan_type":  "pro",
		},
	})
	creds, err := ExtractCredentials("codex", []byte(`{
		"access_token": "tok",
		"id_token": "`+idToken+`"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "acct-from-claim" {
		t.Errorf("AccountID = %q, want acct-from-claim", creds.AccountID)
	}
}

func TestExtractCredentialsAccountIDFromIDTokenDottedClaim(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{
		"sub": "user-1",
		"https://api.openai.com/auth.chatgpt_account_id": " acct-from-dotted-claim ",
	})
	creds, err := ExtractCredentials("codex", []byte(`{
		"tokens": {
			"access_token": "tok",
			"id_token": "`+idToken+`"
		}
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "acct-from-dotted-claim" {
		t.Errorf("AccountID = %q, want acct-from-dotted-claim", creds.AccountID)
	}
}

func TestExtractCredentialsAccountIDFromTopLevelClaim(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{"chatgpt_account_id": "acct-top-claim"})
	creds, err := ExtractCredentials("codex", []byte(`{
		"tokens": {"access_token": "tok", "id_token": "`+idToken+`"}
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "acct-top-claim" {
		t.Errorf("AccountID = %q, want acct-top-claim", creds.AccountID)
	}
}

func TestExtractCredentialsDirectAccountIDWinsOverIDToken(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{"chatgpt_account_id": "acct-from-jwt"})
	creds, err := ExtractCredentials("codex", []byte(`{
		"access_token": "tok",
		"account_id": "acct-direct",
		"id_token": "`+idToken+`"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "acct-direct" {
		t.Errorf("AccountID = %q, want the direct field", creds.AccountID)
	}
}

func TestExtractCredentialsMalformedIDTokenTolerated(t *testing.T) {
	for name, idToken := range map[string]string{
		"not a jwt":       "just-a-string",
		"bad base64":      "a.!!!!.c",
		"non-json claims": "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c",
	} {
		creds, err := ExtractCredentials("codex", []byte(`{
			"access_token": "tok",
			"id_token": "`+idToken+`"
		}`))
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if creds.AccountID != "" {
			t.Errorf("%s: AccountID = %q, want empty", name, creds.AccountID)
		}
	}
}

func TestExtractCredentialsClaudeIgnoresIDTokenClaims(t *testing.T) {
	// Claude auth files also carry an id_token; the ChatGPT claim fallback
	// is Codex-only.
	idToken := fakeJWT(t, map[string]any{"chatgpt_account_id": "should-not-appear"})
	creds, err := ExtractCredentials("claude", []byte(`{
		"access_token": "tok",
		"id_token": "`+idToken+`"
	}`))
	if err != nil {
		t.Fatalf("ExtractCredentials: %v", err)
	}
	if creds.AccountID != "" {
		t.Errorf("AccountID = %q, want empty for claude", creds.AccountID)
	}
}

func TestExtractCredentialsErrorsAreStatic(t *testing.T) {
	// Failure messages must never embed document contents.
	_, err := ExtractCredentials("codex", []byte(`{"refresh_token":"super-secret-refresh"}`))
	if err == nil {
		t.Fatalf("want error when access token missing")
	}
	if got := err.Error(); got != "auth json has no access token" {
		t.Errorf("error = %q, want a static message", got)
	}
}
