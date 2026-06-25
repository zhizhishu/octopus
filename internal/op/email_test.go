package op

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
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

func TestEmailProviderDefault(t *testing.T) {
	// With no value cached, SettingGetString errors and emailProvider defaults
	// to "smtp". Ensure a clean state regardless of test ordering.
	settingCache.Del(model.SettingKeyEmailProvider)
	if got := emailProvider(); got != "smtp" {
		t.Fatalf("expected default provider %q, got %q", "smtp", got)
	}

	t.Cleanup(func() { settingCache.Del(model.SettingKeyEmailProvider) })

	cases := map[string]string{
		"":       "smtp",
		"smtp":   "smtp",
		"bogus":  "smtp",
		"http":   "http",
		"HTTP":   "http",
		" http ": "http",
	}
	for value, want := range cases {
		settingCache.Set(model.SettingKeyEmailProvider, value)
		if got := emailProvider(); got != want {
			t.Fatalf("emailProvider() with %q = %q, want %q", value, got, want)
		}
	}
}

func TestBuildHTTPEmailPayload(t *testing.T) {
	raw, err := buildHTTPEmailPayload("agent@edu.002836.xyz", "to@example.com", "Subj", "<b>hi</b>")
	if err != nil {
		t.Fatalf("buildHTTPEmailPayload returned error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got["from"] != "agent@edu.002836.xyz" {
		t.Fatalf("from = %v, want agent@edu.002836.xyz", got["from"])
	}
	if got["to_mail"] != "to@example.com" {
		t.Fatalf("to_mail = %v, want to@example.com", got["to_mail"])
	}
	if got["subject"] != "Subj" {
		t.Fatalf("subject = %v, want Subj", got["subject"])
	}
	if got["content"] != "<b>hi</b>" {
		t.Fatalf("content = %v, want <b>hi</b>", got["content"])
	}
	if got["is_html"] != true {
		t.Fatalf("is_html = %v, want true", got["is_html"])
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
