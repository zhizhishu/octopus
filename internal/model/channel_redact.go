package model

import (
	"strings"
)

// RedactFlagLetters is the CosyRedactGateway detector alphabet. Order matches
// upstream ALL_FLAG_LETTERS: H high-entropy credentials, P phone, S sk- keys,
// I ID card, B bank card, E email, G gitleaks.
const RedactFlagLetters = "HPSIBEG"

// NormalizeRedactFlags validates and canonicalizes a channel's detector flag
// string: uppercase, drop unknown letters and duplicates, preserve the canonical
// order. Empty input stays empty (= all detectors, upstream's default).
// An input containing only unknown letters is rejected as empty-with-error by
// the caller-facing validation; here it collapses to "" only when nothing valid
// remains AND the input was empty to begin with — otherwise the first return is
// the cleaned subset. Callers that need strict rejection (API validation) check
// HasInvalidRedactFlag first.
func NormalizeRedactFlags(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	if upper == "" {
		return ""
	}
	seen := make(map[rune]bool)
	var out strings.Builder
	for _, c := range RedactFlagLetters {
		if strings.ContainsRune(upper, c) && !seen[c] {
			seen[c] = true
			out.WriteRune(c)
		}
	}
	return out.String()
}

// HasInvalidRedactFlag reports whether raw contains letters outside the
// detector alphabet (case-insensitive). Use this for API-level validation so a
// typo like "EX" is rejected instead of silently dropping the E.
func HasInvalidRedactFlag(raw string) bool {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	if upper == "" {
		return false
	}
	for _, c := range upper {
		if !strings.ContainsRune(RedactFlagLetters, c) {
			return true
		}
	}
	return false
}
