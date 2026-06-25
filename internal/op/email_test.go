package op

import (
	"testing"
	"time"
)

func TestGenerateEmailCode(t *testing.T) {
	for i := 0; i < 100; i++ {
		code := generateEmailCode()
		if len(code) != 6 {
			t.Fatalf("expected 6-digit code, got %q (len %d)", code, len(code))
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("expected only digits, got %q", code)
			}
		}
	}
}

func TestVerifyAndConsumeEmailCode(t *testing.T) {
	const email = "a@b.com"
	storeEmailCode(email, "123456")

	if !VerifyEmailCode(email, "123456") {
		t.Fatalf("expected VerifyEmailCode to return true for correct code")
	}
	if VerifyEmailCode(email, "654321") {
		t.Fatalf("expected VerifyEmailCode to return false for wrong code")
	}

	ConsumeEmailCode(email)
	if VerifyEmailCode(email, "123456") {
		t.Fatalf("expected VerifyEmailCode to return false after ConsumeEmailCode")
	}

	// Expiry: set an entry directly in the package-private store with a past expiry.
	const expiredEmail = "expired@b.com"
	emailCodeMu.Lock()
	emailCodeStore[expiredEmail] = emailCodeEntry{
		code:      "111111",
		expiresAt: time.Now().Add(-time.Minute),
	}
	emailCodeMu.Unlock()
	if VerifyEmailCode(expiredEmail, "111111") {
		t.Fatalf("expected VerifyEmailCode to return false for expired code")
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"first.last@sub.example.co",
	}
	for _, email := range valid {
		if !validateEmail(email) {
			t.Fatalf("expected %q to be valid", email)
		}
	}

	invalid := []string{
		"",
		"not-an-email",
		"user@",
		"@example.com",
		"user @example.com",
		"user@example.com\nBcc: evil@example.com",
	}
	for _, email := range invalid {
		if validateEmail(email) {
			t.Fatalf("expected %q to be invalid", email)
		}
	}
}
