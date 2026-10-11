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
