package xredact

import (
	"regexp"
	"strings"
)

// credentialQueryParams are the exact query-parameter names (matched
// case-insensitively) whose values URLSecrets replaces with ***.
//
// A name is recognized only as a whole parameter, introduced by "?", "&", or
// the percent-encoded separators %3F / %26. "=" may be written %3D. "_" and "-"
// inside a name may be written %5F / %2D, and the name itself is percent-decoded
// once before the lookup, so %6B%65%79 matches "key". A second encoding layer
// (%253D) is not decoded.
//
// The value runs until "&", an unescaped quote, "#", ASCII whitespace, an
// encoded fragment (%23), or an encoded "&" (%26) that introduces another
// name= parameter. A backslash-quote (\" or \') ends the value too and is kept,
// so a Go error quoted inside JSON stays valid. A credential that itself
// contains %26name= is truncated there; that sequence is indistinguishable
// from the next parameter.
//
// Redacted names:
//
//	key
//	apikey, api_key, api-key, x-api-key, x-goog-api-key
//	token, access_token, access-token, refresh_token, refresh-token, id_token, id-token
//	sig, signature, x-amz-signature, x-amz-credential, x-amz-security-token
//	x-goog-signature, x-goog-credential
//	auth, password, passwd, pwd, client_secret, client-secret, secret, security-token
//
// Deliberately not redacted (the match is exact, so these stay intact):
//
//	keyword, keywords          — search text, not a key
//	monkey                      — unrelated
//	tokens, max_tokens,         — counts
//	token_type, token_count
//	author, authority,          — not the "auth" credential
//	authentication, auth_type
//	api-version, api_version    — provider switches
//	key_id                      — identifier, not the secret
//	client_id                   — OAuth public client id
//	signature_method, sig_alg   — algorithm names
//	code, state, session, sid   — too ambiguous (locale, OAuth state, labels)
//	expires, X-Amz-Date,        — signing metadata, not the signature
//	X-Amz-Algorithm, X-Amz-SignedHeaders,
//	X-Goog-Date, X-Goog-Expires, X-Goog-Algorithm, X-Goog-SignedHeaders
//	GoogleAccessId,             — key identifiers
//	OSSAccessKeyId, Key-Pair-Id
//	user_password and any other compound name that only contains a secret word
//
// URL userinfo (user:password@host) is not a query parameter and is left alone.
var credentialQueryParams = map[string]struct{}{
	"access-token":         {},
	"access_token":         {},
	"api-key":              {},
	"api_key":              {},
	"apikey":               {},
	"auth":                 {},
	"client-secret":        {},
	"client_secret":        {},
	"id-token":             {},
	"id_token":             {},
	"key":                  {},
	"passwd":               {},
	"password":             {},
	"pwd":                  {},
	"refresh-token":        {},
	"refresh_token":        {},
	"secret":               {},
	"security-token":       {},
	"sig":                  {},
	"signature":            {},
	"token":                {},
	"x-amz-credential":     {},
	"x-amz-security-token": {},
	"x-amz-signature":      {},
	"x-api-key":            {},
	"x-goog-api-key":       {},
	"x-goog-credential":    {},
	"x-goog-signature":     {},
}

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

// URLSecrets replaces the value of credential-bearing query parameters with ***
// so an upstream URL echoed by an HTTP transport error cannot leak a channel key
// into relay_logs.error, per-attempt audit messages, stdout logs, or API error
// text. See credentialQueryParams for the name list and the names left alone.
// Non-sensitive query parameters and the rest of the text are left untouched.
func URLSecrets(value string) string {
	if value == "" || !strings.ContainsAny(value, "?&%") {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		sep := querySepLen(value, i)
		if sep == 0 {
			b.WriteByte(value[i])
			i++
			continue
		}
		nameEnd := scanParamName(value, i+sep)
		eq := 0
		if nameEnd > 0 {
			eq = queryEqLen(value, nameEnd)
		}
		if nameEnd < 0 || eq == 0 || !isCredentialQueryParam(value[i+sep:nameEnd]) {
			b.WriteString(value[i : i+sep])
			i += sep
			continue
		}
		valStart := nameEnd + eq
		valEnd := scanQueryValueEnd(value, valStart)
		b.WriteString(value[i:valStart])
		if valEnd > valStart {
			b.WriteString("***")
		}
		i = valEnd
	}
	return b.String()
}

func isCredentialQueryParam(raw string) bool {
	_, ok := credentialQueryParams[decodeParamName(raw)]
	return ok
}

// decodeParamName percent-decodes a query name once and case-folds it.
func decodeParamName(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		if decoded, ok := pctByte(raw, i); ok {
			b.WriteByte(decoded)
			i += 3
			continue
		}
		b.WriteByte(raw[i])
		i++
	}
	return strings.ToLower(b.String())
}

func querySepLen(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	switch s[i] {
	case '?', '&':
		return 1
	}
	if decoded, ok := pctByte(s, i); ok && (decoded == '?' || decoded == '&') {
		return 3
	}
	return 0
}

func queryEqLen(s string, i int) int {
	if i < len(s) && s[i] == '=' {
		return 1
	}
	if decoded, ok := pctByte(s, i); ok && decoded == '=' {
		return 3
	}
	return 0
}

func scanParamName(s string, i int) int {
	start := i
	for i < len(s) {
		if isParamNameByte(s[i]) {
			i++
			continue
		}
		// Percent-encoded name bytes (_ - and letters) stay inside the name.
		// Encoded separators must stop the name so "%3D" can still be the equals.
		decoded, ok := pctByte(s, i)
		if !ok || isQueryDelimByte(decoded) {
			break
		}
		i += 3
	}
	if i == start {
		return -1
	}
	return i
}

func isQueryDelimByte(c byte) bool {
	switch c {
	case '=', '&', '?', '#', ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

func isParamNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '_' || c == '-' || c == '.' || c == '~'
}

// scanQueryValueEnd returns the index where a query value ends. See
// credentialQueryParams for which bytes terminate a value.
func scanQueryValueEnd(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case '&', '"', '\'', '#', ' ', '\t', '\n', '\r':
			return i
		}
		// Keep \" and \' in place. They close a URL that was quoted inside JSON
		// (Post \"https://host?apikey=SECRET\") and are not part of the secret.
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\'') {
			return i
		}
		if decoded, ok := pctByte(s, i); ok {
			if decoded == '#' || (decoded == '&' && separatesParam(s, i+3)) {
				return i
			}
			i += 3
			continue
		}
		i++
	}
	return len(s)
}

// separatesParam reports whether s[i:] begins with a query name and "=" / "%3D".
func separatesParam(s string, i int) bool {
	end := scanParamName(s, i)
	if end < 0 {
		return false
	}
	return queryEqLen(s, end) > 0
}

func pctByte(s string, i int) (byte, bool) {
	if i+2 >= len(s) || s[i] != '%' || !isHex(s[i+1]) || !isHex(s[i+2]) {
		return 0, false
	}
	return unhex(s[i+1])<<4 | unhex(s[i+2]), true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
