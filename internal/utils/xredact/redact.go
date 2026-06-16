package xredact

import (
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+\-/=]{8,})`),
	regexp.MustCompile(`(?i)(x-api-key\s*[:=]\s*)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret)\s*[:=]\s*)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9._-]{3,}\b`),
}

// Secrets replaces provider/API credentials that might be echoed by upstream
// proxies. It is intentionally conservative enough for user-facing errors and
// audit summaries, not for preserving exact raw upstream text.
func Secrets(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, pattern := range secretPatterns {
		switch pattern.NumSubexp() {
		case 2:
			value = pattern.ReplaceAllString(value, `${1}[redacted]`)
		case 1:
			value = pattern.ReplaceAllString(value, `Bearer [redacted]`)
		default:
			value = pattern.ReplaceAllString(value, `[redacted]`)
		}
	}
	return value
}
