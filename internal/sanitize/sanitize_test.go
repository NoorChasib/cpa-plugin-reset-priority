package sanitize

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMessageRedactsBearerTokens(t *testing.T) {
	secret := "<TEST_BEARER_TOKEN>"
	got := Message("request failed: Authorization: Bearer " + secret)
	if strings.Contains(got, secret) {
		t.Errorf("bearer token survived: %q", got)
	}
}

func TestMessageRedactsJWTs(t *testing.T) {
	jwt := strings.Join([]string{"eyJdummy", "testpart", "signature"}, ".")
	got := Message("token " + jwt + " rejected")
	if strings.Contains(got, jwt) {
		t.Errorf("jwt survived: %q", got)
	}
}

func TestMessageRedactsAPIKeys(t *testing.T) {
	key := "sk-" + strings.Repeat("test", 3)
	got := Message("bad key " + key)
	if strings.Contains(got, key) {
		t.Errorf("api key survived: %q", got)
	}
}

func TestMessageStripsQueryStrings(t *testing.T) {
	secret := "<TEST_QUERY_VALUE>"
	got := Message("GET https://api.example.com/usage?access_token=" + secret + " failed")
	if strings.Contains(got, secret) {
		t.Errorf("query string survived: %q", got)
	}
	if !strings.Contains(got, "https://api.example.com/usage") {
		t.Errorf("base URL lost: %q", got)
	}
}

func TestMessageRedactsLongTokenRuns(t *testing.T) {
	long := strings.Repeat("Ab1", 20) // 60 chars of token-ish material
	got := Message("blob " + long + " end")
	if strings.Contains(got, long) {
		t.Errorf("long token run survived: %q", got)
	}
}

func TestMessageTruncates(t *testing.T) {
	got := Message(strings.Repeat("x ", 400))
	if len(got) > 250 {
		t.Errorf("message not truncated: %d chars", len(got))
	}
}

func TestMessageKeepsOrdinaryText(t *testing.T) {
	msg := "claude usage endpoint returned HTTP 429"
	if got := Message(msg); got != msg {
		t.Errorf("ordinary message mangled: %q", got)
	}
}

func TestErrorNil(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q", got)
	}
	if got := Error(errors.New("plain failure")); got != "plain failure" {
		t.Errorf("Error = %q", got)
	}
}

func TestMessageTruncationIsUTF8Safe(t *testing.T) {
	// The length cap is a byte budget, so a naive s[:max] slice can cut a
	// multi-byte rune in half and emit invalid UTF-8 into status pages,
	// management JSON responses, and host logs. The single leading ASCII
	// byte offsets the 3-byte runes so the cap lands inside a rune rather
	// than on a boundary.
	got := Message("x" + strings.Repeat("日", 200))
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncation marker missing: %q", got)
	}
	if len(got) > maxMessageLength+len("...") {
		t.Errorf("truncated message is %d bytes, want at most %d", len(got), maxMessageLength+len("..."))
	}
}

func TestMessageTruncationKeepsWholeRunes(t *testing.T) {
	// Every rune that survives truncation must be intact: decoding the
	// result must never yield the replacement character.
	for name, input := range map[string]string{
		"two-byte runes":   strings.Repeat("é", 400),
		"three-byte runes": "x" + strings.Repeat("日", 200),
		"four-byte runes":  "xy" + strings.Repeat("\U0001d11e", 200),
	} {
		t.Run(name, func(t *testing.T) {
			got := Message(input)
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("truncation split a rune: %q", got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncation produced invalid UTF-8: %q", got)
			}
		})
	}
}
