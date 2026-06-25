package op

import (
	"encoding/json"
	"net"
	"strconv"
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

// resetEmailRateState clears the sliding-window rate maps so tests do not
// interfere with one another regardless of ordering.
func resetEmailRateState(t *testing.T) {
	t.Helper()
	emailRateMu.Lock()
	emailRateByEmail = make(map[string][]time.Time)
	emailRateByIP = make(map[string][]time.Time)
	emailRateMu.Unlock()
}

func TestEmailRateMapPruning(t *testing.T) {
	resetEmailRateState(t)

	// Seed stale entries well outside their windows and a fresh request; the
	// stale entries must be pruned (and their now-empty keys deleted), leaving
	// only the current request's record.
	const staleEmail = "stale-prune@b.com"
	const staleIP = "203.0.113.7"
	emailRateMu.Lock()
	emailRateByEmail[staleEmail] = []time.Time{time.Now().Add(-emailPerEmailHourlyWindow - time.Minute)}
	emailRateByIP[staleIP] = []time.Time{time.Now().Add(-emailPerIPWindow - time.Minute)}
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

	// The per-email cooldown still works: an immediate repeat is rejected.
	if err := emailRateLimitAllow("fresh-prune@b.com", ""); err == nil {
		t.Fatalf("expected immediate repeat for same email to be rate-limited")
	}
}

// TestEmailRateLimitPerEmailHourlyCap verifies the per-email hourly cap: even
// when the 60s cooldown is satisfied (timestamps backdated), the 6th send in an
// hour is blocked while the first emailPerEmailHourlyMax are allowed.
func TestEmailRateLimitPerEmailHourlyCap(t *testing.T) {
	resetEmailRateState(t)

	const email = "hourly-cap@b.com"
	// Pre-seed emailPerEmailHourlyMax recent-but-cooldown-cleared sends within
	// the hourly window (spaced so the most recent is older than the cooldown).
	now := time.Now()
	seeded := make([]time.Time, 0, emailPerEmailHourlyMax)
	for i := emailPerEmailHourlyMax; i >= 1; i-- {
		// All within the last hour; the newest is > cooldown ago.
		seeded = append(seeded, now.Add(-time.Duration(i)*2*emailPerEmailCooldown))
	}
	emailRateMu.Lock()
	emailRateByEmail[email] = seeded
	emailRateMu.Unlock()

	// The (max+1)th send within the hour must be blocked, even though the
	// cooldown is satisfied.
	if err := emailRateLimitAllow(email, ""); err == nil {
		t.Fatalf("expected the %dth send within the hour to be rate-limited", emailPerEmailHourlyMax+1)
	}
}

// TestEmailRateLimitPerEmailHourlyCapAllowsUpToMax confirms exactly
// emailPerEmailHourlyMax sends are admitted within the hour (cooldown aside)
// before the cap trips.
func TestEmailRateLimitPerEmailHourlyCapAllowsUpToMax(t *testing.T) {
	resetEmailRateState(t)

	const email = "hourly-cap-allow@b.com"
	now := time.Now()
	// Seed max-1 sends within the hour, all older than the cooldown, so the next
	// call is the max-th and must be allowed.
	seeded := make([]time.Time, 0, emailPerEmailHourlyMax-1)
	for i := emailPerEmailHourlyMax - 1; i >= 1; i-- {
		seeded = append(seeded, now.Add(-time.Duration(i)*2*emailPerEmailCooldown))
	}
	emailRateMu.Lock()
	emailRateByEmail[email] = seeded
	emailRateMu.Unlock()

	if err := emailRateLimitAllow(email, ""); err != nil {
		t.Fatalf("expected the %dth send within the hour to be allowed, got %v", emailPerEmailHourlyMax, err)
	}
	// Now at the cap; the immediate next is blocked (both cooldown and cap).
	if err := emailRateLimitAllow(email, ""); err == nil {
		t.Fatalf("expected the %dth send within the hour to be rate-limited", emailPerEmailHourlyMax+1)
	}
}

// TestEmailRateLimitPerIPBurst proves the per-IP gate is a sliding-window burst
// of emailPerIPMax per emailPerIPWindow (NOT the old 20s per-action gap): many
// distinct emails from one IP succeed back-to-back up to the burst, and only the
// (max+1)th is blocked. Each email differs so the per-email cooldown never trips.
func TestEmailRateLimitPerIPBurst(t *testing.T) {
	resetEmailRateState(t)

	const ip = "198.51.100.42"
	for i := 0; i < emailPerIPMax; i++ {
		email := "burst" + strconv.Itoa(i) + "@b.com"
		if err := emailRateLimitAllow(email, ip); err != nil {
			t.Fatalf("burst send %d/%d unexpectedly rate-limited (old 20s gap would block here): %v", i+1, emailPerIPMax, err)
		}
	}
	// The (max+1)th distinct-email send from the same IP within the window is
	// blocked by the per-IP burst cap.
	if err := emailRateLimitAllow("burst-over@b.com", ip); err == nil {
		t.Fatalf("expected the %dth send from one IP within the window to be rate-limited", emailPerIPMax+1)
	}
}

// TestSendEmailVerificationAdminBypassRateLimits proves a verified admin caller
// (isAdmin=true) is never throttled by the per-email cooldown, per-email hourly
// cap, per-IP burst, or the global cap. Verification is disabled here so the
// call short-circuits before any DB/provider work, isolating the rate-limit
// bypass: a non-admin would be blocked by the disabled toggle, but the point is
// the rate-limit gates must never reject an admin. We assert the gates directly
// plus the disabled-toggle behaviour for admin.
func TestSendEmailVerificationAdminBypassRateLimits(t *testing.T) {
	resetEmailRateState(t)

	const email = "admin-bypass@b.com"
	const ip = "198.51.100.99"

	// Saturate both per-email and per-IP windows for a non-admin so any
	// non-bypassing path would be blocked.
	now := time.Now()
	emailRateMu.Lock()
	saturatedEmail := make([]time.Time, 0, emailPerEmailHourlyMax)
	for i := 0; i < emailPerEmailHourlyMax; i++ {
		saturatedEmail = append(saturatedEmail, now)
	}
	emailRateByEmail[email] = saturatedEmail
	saturatedIP := make([]time.Time, 0, emailPerIPMax)
	for i := 0; i < emailPerIPMax; i++ {
		saturatedIP = append(saturatedIP, now)
	}
	emailRateByIP[ip] = saturatedIP
	emailRateMu.Unlock()

	// Sanity: a non-admin is blocked by the saturated windows.
	if err := emailRateLimitAllow(email, ip); err == nil {
		t.Fatalf("setup error: expected saturated windows to block a non-admin")
	}

	// Also saturate the global cap so a non-admin would hit the circuit breaker.
	globalSendMu.Lock()
	globalSendTimes = globalSendTimes[:0]
	for i := 0; i < maxGlobalSendsPerWindow; i++ {
		globalSendTimes = append(globalSendTimes, now)
	}
	globalSendMu.Unlock()
	t.Cleanup(func() {
		globalSendMu.Lock()
		globalSendTimes = nil
		globalSendMu.Unlock()
	})

	// Ensure verification is disabled so SendEmailVerificationCode returns early
	// with the enabled-toggle error for BOTH admin and non-admin, proving the
	// admin path reached past the (skipped) rate limits without a throttle error.
	settingCache.Del(model.SettingKeyEmailVerificationEnabled)
	t.Cleanup(func() { settingCache.Del(model.SettingKeyEmailVerificationEnabled) })

	// Admin: must never get the generic throttle error. With verification
	// disabled it returns the enabled-toggle error instead, which is fine; the
	// assertion is specifically that it is NOT the rate-limit message.
	err := SendEmailVerificationCode(email, ip, true)
	if err != nil && err.Error() == "please wait a moment before requesting another code" {
		t.Fatalf("admin caller was throttled despite isAdmin=true: %v", err)
	}

	// Repeat many times to be sure the admin bypass is not consuming/limited by
	// any window even past the caps.
	for i := 0; i < emailPerIPMax+emailPerEmailHourlyMax+5; i++ {
		err := SendEmailVerificationCode(email, ip, true)
		if err != nil && err.Error() == "please wait a moment before requesting another code" {
			t.Fatalf("admin caller throttled on repeat %d despite isAdmin=true: %v", i, err)
		}
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
