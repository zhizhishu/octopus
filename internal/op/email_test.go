package op

import (
	"encoding/json"
	"net"
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

func TestEmailCodeAttemptCap(t *testing.T) {
	const email = "attempts@b.com"
	storeEmailCode(email, "123456")

	// A freshly stored code starts with zero attempts.
	emailCodeMu.Lock()
	if got := emailCodeStore[email].attempts; got != 0 {
		emailCodeMu.Unlock()
		t.Fatalf("expected freshly stored code to have 0 attempts, got %d", got)
	}
	emailCodeMu.Unlock()

	// maxEmailCodeAttempts wrong guesses must invalidate the entry. The final
	// guess (the one that reaches the cap) deletes the entry.
	for i := 0; i < maxEmailCodeAttempts; i++ {
		if VerifyEmailCode(email, "000000") {
			t.Fatalf("wrong guess %d unexpectedly succeeded", i+1)
		}
	}

	emailCodeMu.Lock()
	_, ok := emailCodeStore[email]
	emailCodeMu.Unlock()
	if ok {
		t.Fatalf("expected entry to be invalidated after %d wrong guesses", maxEmailCodeAttempts)
	}

	// Even the correct code now misses, because the entry is gone.
	if VerifyEmailCode(email, "123456") {
		t.Fatalf("expected correct code to miss after entry was invalidated")
	}
}

func TestEmailCodeAttemptResetOnCorrect(t *testing.T) {
	const email = "attempts-ok@b.com"
	storeEmailCode(email, "123456")

	// A few wrong guesses below the cap leave the entry intact.
	for i := 0; i < maxEmailCodeAttempts-1; i++ {
		if VerifyEmailCode(email, "000000") {
			t.Fatalf("wrong guess %d unexpectedly succeeded", i+1)
		}
	}
	// The correct code still verifies and does not consume the entry.
	if !VerifyEmailCode(email, "123456") {
		t.Fatalf("expected correct code to verify before the cap")
	}
	emailCodeMu.Lock()
	_, ok := emailCodeStore[email]
	emailCodeMu.Unlock()
	if !ok {
		t.Fatalf("expected entry to survive a correct verification (no consume)")
	}
	ConsumeEmailCode(email)
}

func TestEmailRateMapPruning(t *testing.T) {
	// Seed a stale entry well outside its window and a fresh request; the stale
	// entry must be pruned, leaving only the current request's record.
	const staleEmail = "stale-prune@b.com"
	const staleIP = "203.0.113.7"
	emailRateMu.Lock()
	emailRateByEmail[staleEmail] = time.Now().Add(-emailRateLimitPerEmail - time.Minute)
	emailRateByIP[staleIP] = time.Now().Add(-emailRateLimitPerIP - time.Minute)
	emailRateMu.Unlock()

	if err := emailRateLimitAllow("fresh-prune@b.com", "198.51.100.9"); err != nil {
		t.Fatalf("fresh request unexpectedly rate-limited: %v", err)
	}

	emailRateMu.Lock()
	_, staleEmailKept := emailRateByEmail[staleEmail]
	_, staleIPKept := emailRateByIP[staleIP]
	emailRateMu.Unlock()
	if staleEmailKept {
		t.Fatalf("expected stale per-email entry to be pruned")
	}
	if staleIPKept {
		t.Fatalf("expected stale per-IP entry to be pruned")
	}

	// The per-email gate still works: an immediate repeat is rejected.
	if err := emailRateLimitAllow("fresh-prune@b.com", ""); err == nil {
		t.Fatalf("expected immediate repeat for same email to be rate-limited")
	}
}

func TestEmailSSRFHostCheck(t *testing.T) {
	disallowed := []net.IP{
		net.ParseIP("127.0.0.1"),       // loopback
		net.ParseIP("::1"),             // loopback v6
		net.ParseIP("169.254.169.254"), // link-local (cloud metadata)
		net.ParseIP("fe80::1"),         // link-local v6
		net.ParseIP("0.0.0.0"),         // unspecified
		net.ParseIP("::"),              // unspecified v6
		net.ParseIP("224.0.0.1"),       // multicast
	}
	for _, ip := range disallowed {
		if !isDisallowedEmailHostIP(ip) {
			t.Fatalf("expected %v to be disallowed", ip)
		}
	}

	allowed := []net.IP{
		net.ParseIP("8.8.8.8"),      // public
		net.ParseIP("10.0.0.5"),     // private LAN 10/8
		net.ParseIP("172.16.0.5"),   // private LAN 172.16/12
		net.ParseIP("192.168.1.10"), // private LAN 192.168/16
		net.ParseIP("2606:4700::1"), // public v6
	}
	for _, ip := range allowed {
		if isDisallowedEmailHostIP(ip) {
			t.Fatalf("expected %v to be allowed", ip)
		}
	}

	// checkEmailHTTPTarget rejects loopback/metadata literals without DNS, and
	// admits a public IP literal. Hostnames are not resolved here to keep the
	// test network-free.
	if err := checkEmailHTTPTarget("http://127.0.0.1:8080"); err == nil {
		t.Fatalf("expected loopback base URL to be rejected")
	}
	if err := checkEmailHTTPTarget("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatalf("expected cloud-metadata base URL to be rejected")
	}
	if err := checkEmailHTTPTarget("https://8.8.8.8/admin"); err != nil {
		t.Fatalf("expected public IP base URL to be allowed, got %v", err)
	}
	if err := checkEmailHTTPTarget("https://192.168.1.10:9000"); err != nil {
		t.Fatalf("expected private-LAN IP base URL to be allowed, got %v", err)
	}
	if err := checkEmailHTTPTarget("::: not a url"); err == nil {
		t.Fatalf("expected malformed base URL to be rejected")
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
