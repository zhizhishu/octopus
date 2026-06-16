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
