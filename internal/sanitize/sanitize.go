// Package sanitize scrubs error messages and log text before they reach
// status pages, management responses, or host logs. Provider errors may embed
// URLs, headers, or token fragments; nothing token-shaped may leave the
// plugin.
package sanitize

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxMessageLength = 240

var (
	// bearerPattern redacts explicit bearer credentials.
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"']+`)
	// jwtPattern redacts JWT-shaped blobs (three dot-joined base64url parts).
	jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}(?:\.[A-Za-z0-9_.-]{4,}){0,2}`)
	// keyPattern redacts common API-key shapes.
	keyPattern = regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}\b`)
	// queryPattern strips query strings from URLs (may carry tokens).
	queryPattern = regexp.MustCompile(`\?[^\s"']*`)
	// longTokenPattern redacts very long unbroken secret-shaped runs.
	longTokenPattern = regexp.MustCompile(`[A-Za-z0-9+/_=-]{48,}`)
)

// Message scrubs a free-form message for safe display and logging.
func Message(s string) string {
	if s == "" {
		return ""
	}
	s = bearerPattern.ReplaceAllString(s, "bearer [redacted]")
	s = jwtPattern.ReplaceAllString(s, "[redacted]")
	s = keyPattern.ReplaceAllString(s, "[redacted]")
	s = queryPattern.ReplaceAllString(s, "?[redacted]")
	s = longTokenPattern.ReplaceAllString(s, "[redacted]")
	s = strings.TrimSpace(s)
	if len(s) > maxMessageLength {
		s = truncateToRuneBoundary(s, maxMessageLength) + "..."
	}
	return s
}

// truncateToRuneBoundary caps s at maxBytes bytes without splitting a
// multi-byte rune. maxMessageLength is a byte budget, so a plain slice can cut
// a UTF-8 sequence in half and emit invalid bytes into status pages,
// management JSON responses, and host logs; backing up to the start of the
// straddling rune drops at most three bytes instead.
func truncateToRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is a continuation byte exactly while the cut lands inside a
	// rune, so this walks back to the boundary that starts it.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Error scrubs an error for safe display and logging.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return Message(err.Error())
}
