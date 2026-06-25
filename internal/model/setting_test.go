package model

import "testing"

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
