package xredact

import (
	"regexp"
	"strings"
)

// urlSecretQuery matches credential-bearing URL query parameters. Go's HTTP
// client echoes the full request URL in transport errors, so a Gemini-style
// ?key=<channel key> would otherwise survive into logs verbatim.
var urlSecretQuery = regexp.MustCompile(`(?i)([?&](?:key|api_key|api-key|token|access_token|sig|signature)=)([^&"'#\s]+)`)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+\-/=]{8,})`),
	regexp.MustCompile(`(?i)(x-api-key\s*[:=]\s*)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret)\s*[:=]\s*)([^\s"',;{}\[\]]+)`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9._-]{3,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
}

// Secrets replaces provider/API credentials that might be echoed by upstream
// proxies. It is intentionally conservative enough for user-facing errors and
// audit summaries, not for preserving exact raw upstream text.
func Secrets(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = URLSecrets(value)
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

// URLSecrets replaces the value of credential-bearing query parameters
// (?key=, ?api_key=, ?token=, ?sig=, ...) with *** so an upstream URL echoed by
// an HTTP transport error cannot leak a Gemini channel key into relay_logs.error,
// per-attempt audit messages, or stdout logs. Non-sensitive query parameters and
// the rest of the text are left untouched.
func URLSecrets(value string) string {
	if value == "" {
		return ""
	}
	return urlSecretQuery.ReplaceAllString(value, `${1}***`)
}
