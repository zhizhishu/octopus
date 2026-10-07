package model

import (
	"math"
	"strconv"
	"testing"
)

func TestRelayStreamKeepaliveIntervalValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     SettingKey
		value   string
		wantErr bool
	}{
		{name: "keepalive default positive", key: SettingKeyRelayStreamKeepaliveSec, value: "15"},
		{name: "keepalive disabled", key: SettingKeyRelayStreamKeepaliveSec, value: "0"},
		{name: "keepalive negative", key: SettingKeyRelayStreamKeepaliveSec, value: "-1", wantErr: true},
		{name: "keepalive not integer", key: SettingKeyRelayStreamKeepaliveSec, value: "1.5", wantErr: true},
		{name: "data timeout default positive", key: SettingKeyRelayStreamDataTimeoutSec, value: DefaultRelayStreamDataIntervalTimeoutSeconds},
		{name: "data timeout disabled", key: SettingKeyRelayStreamDataTimeoutSec, value: "0"},
		{name: "data timeout negative", key: SettingKeyRelayStreamDataTimeoutSec, value: "-1", wantErr: true},
		{name: "data timeout not integer", key: SettingKeyRelayStreamDataTimeoutSec, value: "1.5", wantErr: true},
		{name: "first token default positive", key: SettingKeyFirstTokenTimeOutDefault, value: "30"},
		{name: "first token default disabled", key: SettingKeyFirstTokenTimeOutDefault, value: "0"},
		{name: "first token default negative", key: SettingKeyFirstTokenTimeOutDefault, value: "-1", wantErr: true},
		{name: "first token default not integer", key: SettingKeyFirstTokenTimeOutDefault, value: "1.5", wantErr: true},
		// Upstream header wait: default-off, but bounded so an out-of-range value can
		// never overflow into a negative duration (which the relay reads as disabled).
		{name: "upstream header disabled", key: SettingKeyUpstreamHeaderTimeoutSec, value: "0"},
		{name: "upstream header positive", key: SettingKeyUpstreamHeaderTimeoutSec, value: "120"},
		{name: "upstream header at ceiling", key: SettingKeyUpstreamHeaderTimeoutSec, value: "86400"},
		{name: "upstream header above ceiling", key: SettingKeyUpstreamHeaderTimeoutSec, value: "86401", wantErr: true},
		{name: "upstream header overflow attempt", key: SettingKeyUpstreamHeaderTimeoutSec, value: "9223372036", wantErr: true},
		{name: "upstream header just past overflow boundary", key: SettingKeyUpstreamHeaderTimeoutSec, value: "9223372037", wantErr: true},
		{name: "upstream header platform int max", key: SettingKeyUpstreamHeaderTimeoutSec, value: strconv.Itoa(math.MaxInt), wantErr: true},
		{name: "upstream header negative", key: SettingKeyUpstreamHeaderTimeoutSec, value: "-1", wantErr: true},
		{name: "upstream header not integer", key: SettingKeyUpstreamHeaderTimeoutSec, value: "1.5", wantErr: true},
		{name: "upstream header not a number", key: SettingKeyUpstreamHeaderTimeoutSec, value: "abc", wantErr: true},
		{name: "upstream header empty", key: SettingKeyUpstreamHeaderTimeoutSec, value: "", wantErr: true},
		{name: "upstream header blank", key: SettingKeyUpstreamHeaderTimeoutSec, value: " ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setting := Setting{
				Key:   tt.key,
				Value: tt.value,
			}
			err := setting.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestHeaderDefaultsValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     SettingKey
		value   string
		wantErr bool
	}{
		{name: "claude ua", key: SettingKeyClaudeHeaderUserAgent, value: "claude-cli/2.1.126"},
		{name: "codex beta", key: SettingKeyCodexHeaderBetaFeatures, value: "multi_agent"},
		{name: "header newline", key: SettingKeyCodexHeaderUserAgent, value: "codex\nbad", wantErr: true},
		{name: "stabilize true", key: SettingKeyClaudeHeaderStabilize, value: "true"},
		{name: "stabilize invalid", key: SettingKeyClaudeHeaderStabilize, value: "yes", wantErr: true},
		{name: "claude auto compact true", key: SettingKeyClaudeCLIAutoCompact, value: "true"},
		{name: "claude auto compact invalid", key: SettingKeyClaudeCLIAutoCompact, value: "yes", wantErr: true},
		{name: "codex fast mode true", key: SettingKeyCodexFastMode, value: "true"},
		{name: "codex fast mode invalid", key: SettingKeyCodexFastMode, value: "yes", wantErr: true},
		{name: "claude reasoning high", key: SettingKeyClaudeCLIReasoningEffort, value: "high"},
		{name: "claude reasoning off", key: SettingKeyClaudeCLIReasoningEffort, value: "off"},
		{name: "claude reasoning invalid", key: SettingKeyClaudeCLIReasoningEffort, value: "ultra", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Setting{Key: tt.key, Value: tt.value}).Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestRouteModeOverrideValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "empty uses default", value: ""},
		{name: "blank uses default", value: " \t "},
		{name: "spread", value: "spread"},
		{name: "fill_first", value: "fill_first"},
		{name: "uppercase spread", value: "SPREAD"},
		{name: "padded fill_first", value: "  fill_first  "},
		{name: "unknown mode", value: "smart", wantErr: true},
		{name: "legacy alias rejected", value: "round_robin", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Setting{Key: SettingKeyRouteModeOverride, Value: tt.value}).Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestProxyURLValidationAcceptsHTTPAndSOCKS(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "empty disabled", value: ""},
		{name: "http proxy", value: "http://127.0.0.1:8080"},
		{name: "https proxy", value: "https://proxy.example:8443"},
		{name: "socks proxy", value: "socks://127.0.0.1:1080"},
		{name: "socks5 proxy", value: "socks5://127.0.0.1:1080"},
		{name: "unsupported scheme", value: "ftp://127.0.0.1:21", wantErr: true},
		{name: "missing host", value: "socks5://", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Setting{Key: SettingKeyProxyURL, Value: tt.value}).Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestRedactProxyURLPassword(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "no credentials", raw: "http://proxy.example.com:8080", want: "http://proxy.example.com:8080"},
		{name: "user only", raw: "http://user@proxy.example.com:8080", want: "http://user@proxy.example.com:8080"},
		{name: "user and password", raw: "http://user:secret@proxy.example.com:8080", want: "http://user:***@proxy.example.com:8080"},
		{name: "socks5 with password path query", raw: "socks5://u:p@10.0.0.1:1080/x?a=b", want: "socks5://u:***@10.0.0.1:1080/x?a=b"},
		{name: "unparseable unchanged", raw: "://not a url", want: "://not a url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactProxyURLPassword(tt.raw); got != tt.want {
				t.Fatalf("RedactProxyURLPassword(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestMergeProxyURLPassword(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		stored   string
		want     string
	}{
		{name: "round-trip restores stored password", incoming: "http://user:***@proxy.example.com:8080", stored: "http://user:secret@proxy.example.com:8080", want: "http://user:secret@proxy.example.com:8080"},
		{name: "edited host keeps stored password", incoming: "http://user:***@new.example.com:9090", stored: "http://user:secret@proxy.example.com:8080", want: "http://user:secret@new.example.com:9090"},
		{name: "placeholder but stored has no password is stripped", incoming: "http://user:***@proxy.example.com:8080", stored: "http://user@proxy.example.com:8080", want: "http://user@proxy.example.com:8080"},
		{name: "placeholder but stored empty is stripped", incoming: "http://user:***@proxy.example.com:8080", stored: "", want: "http://user@proxy.example.com:8080"},
		{name: "real incoming password unchanged", incoming: "http://user:newsecret@proxy.example.com:8080", stored: "http://user:secret@proxy.example.com:8080", want: "http://user:newsecret@proxy.example.com:8080"},
		{name: "no credentials unchanged", incoming: "http://proxy.example.com:8080", stored: "http://user:secret@proxy.example.com:8080", want: "http://proxy.example.com:8080"},
		{name: "empty incoming unchanged", incoming: "", stored: "http://user:secret@proxy.example.com:8080", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MergeProxyURLPassword(tt.incoming, tt.stored); got != tt.want {
				t.Fatalf("MergeProxyURLPassword(%q, %q) = %q, want %q", tt.incoming, tt.stored, got, tt.want)
			}
		})
	}
}

// TestProxyURLRedactMergeRoundTrip exercises the full settings-list -> save loop:
// a stored URL with a password is redacted for display, and saving that redacted
// value back restores the original password and still passes Validate().
func TestProxyURLRedactMergeRoundTrip(t *testing.T) {
	stored := "socks5://user:s3cr3t@10.0.0.1:1080"
	redacted := RedactProxyURLPassword(stored)
	if redacted == stored {
		t.Fatalf("expected redaction to change the URL, got %q", redacted)
	}
	merged := MergeProxyURLPassword(redacted, stored)
	if merged != stored {
		t.Fatalf("round-trip MergeProxyURLPassword(%q, %q) = %q, want %q", redacted, stored, merged, stored)
	}
	if err := (&Setting{Key: SettingKeyProxyURL, Value: merged}).Validate(); err != nil {
		t.Fatalf("merged value failed Validate: %v", err)
	}
}

// The upstream header budget is default-off, and the environment variable is the
// *initial* value only: it seeds DefaultSettings, after which the stored setting (what
// the Settings page writes) wins. This pins the seeding half; the relay-side reader
// pins that a stored value is what takes effect.
func TestUpstreamHeaderTimeoutDefaultSeededFromEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{name: "unset stays disabled", env: "", want: "0"},
		{name: "valid seconds", env: "120", want: "120"},
		{name: "negative falls back to disabled", env: "-5", want: "0"},
		{name: "garbage falls back to disabled", env: "abc", want: "0"},
		{name: "fractional falls back to disabled", env: "1.5", want: "0"},
		{name: "above ceiling is capped", env: "999999", want: "86400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OCTOPUS_UPSTREAM_HEADER_TIMEOUT_SECONDS", tc.env)
			if got := defaultUpstreamHeaderTimeoutSeconds(); got != tc.want {
				t.Fatalf("seeded default = %q, want %q", got, tc.want)
			}
			var seeded string
			found := false
			for _, s := range DefaultSettings() {
				if s.Key == SettingKeyUpstreamHeaderTimeoutSec {
					seeded, found = s.Value, true
					break
				}
			}
			if !found {
				t.Fatalf("DefaultSettings must register %s", SettingKeyUpstreamHeaderTimeoutSec)
			}
			if seeded != tc.want {
				t.Fatalf("DefaultSettings value = %q, want %q", seeded, tc.want)
			}
		})
	}
}

// The no-breaker rescue budget is capped at MaxRelayNoBreakerRetryBudgetSeconds (300s),
// which is also the hard ceiling on the unified automatic-recovery window. The old 600s
// ceiling must now be rejected.
func TestNoBreakerRetryBudgetValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "disabled", value: "0"},
		{name: "at ceiling", value: "300"},
		{name: "tightened", value: "120"},
		{name: "above ceiling", value: "301", wantErr: true},
		{name: "old ceiling now rejected", value: "600", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "not an integer", value: "1.5", wantErr: true},
		{name: "not a number", value: "abc", wantErr: true},
		{name: "empty", value: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Setting{Key: SettingKeyRelayNoBreakerRetryBudgetSec, Value: tt.value}).Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
