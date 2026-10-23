package xredact

import (
	"strings"
	"testing"
)

const (
	fakeJWT  = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	fakeHex  = "0123456789abcdef0123456789abcdef"
	fakeSK   = "sk-test1234567890abcdefgh"
	fakeBear = "Bearer qW3ertyuiop1234567890"
)

// TestSecretsRedactsJWTPinnedMode pins that raw JWT tokens are redacted.
func TestSecretsRedactsJWTPinnedMode(t *testing.T) {
	got := Secrets("auth failed for " + fakeJWT)
	if strings.Contains(got, fakeJWT) {
		t.Fatalf("JWT leaked: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected [redacted] marker, got %s", got)
	}
}

// TestSecretsRedactsBareHex pins that bare 32+ hex strings are redacted.
func TestSecretsRedactsBareHex(t *testing.T) {
	got := Secrets("key= " + fakeHex + " rejected")
	if strings.Contains(got, fakeHex) {
		t.Fatalf("bare hex leaked: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected [redacted] marker, got %s", got)
	}
}

// TestSecretsRedactsSKAndBearer keeps the legacy forms covered.
func TestSecretsRedactsSKAndBearer(t *testing.T) {
	got := Secrets("Authorization: " + fakeBear + " key " + fakeSK)
	if strings.Contains(got, fakeSK) || strings.Contains(got, "qW3ertyuiop1234567890") {
		t.Fatalf("secret leaked: %s", got)
	}
}

// TestSecretsKeepsShortHex pins that short hex ids are not over-redacted.
func TestSecretsKeepsShortHex(t *testing.T) {
	in := "request id abcdef0123456789 done"
	if got := Secrets(in); got != in {
		t.Fatalf("over-redacted short id: %s", got)
	}
}

// TestURLSecretsRedactsQueryCredentials pins that credential-carrying URL query
// parameters (the Gemini ?key=<channel key> shape) are replaced with ***, while
// non-sensitive parameters, the path, and surrounding text stay intact.
func TestURLSecretsRedactsQueryCredentials(t *testing.T) {
	in := `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=AIzaSecret123&api_key=k2&api-key=k3&token=t4&access_token=t5&sig=s6&signature=s7&safe=keep": dial tcp: connection refused`
	want := `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=***&api_key=***&api-key=***&token=***&access_token=***&sig=***&signature=***&safe=keep": dial tcp: connection refused`
	if got := URLSecrets(in); got != want {
		t.Fatalf("URLSecrets mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestSecretsCoversURLQueryCredentials pins that the existing Secrets scrubber
// (used for per-attempt audit messages) inherits the URL query rule, so a Gemini
// key in an echoed request URL cannot ride through that path either.
func TestSecretsCoversURLQueryCredentials(t *testing.T) {
	got := Secrets(`Post "https://x/v1?alt=sse&key=AIzaSecret123": boom`)
	if strings.Contains(got, "AIzaSecret123") {
		t.Fatalf("Secrets leaked URL query key: %s", got)
	}
	if !strings.Contains(got, "alt=sse&key=***") {
		t.Fatalf("expected key=*** with alt=sse preserved, got: %s", got)
	}
}
