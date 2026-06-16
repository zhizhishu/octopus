package relay

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestDeriveClientSessionKeyPrefersStableSessionHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("AH-Trace-Id", "trace-1")
	headers.Set("Session_id", "codex-session-1")

	got := deriveClientSessionKey(headers, nil)
	want := hashRouteSessionKey("codex-session", "codex-session-1")
	if got != want {
		t.Fatalf("session key = %q, want %q", got, want)
	}
}

func TestDeriveClientSessionKeyPrefersTraceWhenThreadAndTracePresent(t *testing.T) {
	headers := http.Header{}
	headers.Set("AH-Thread-Id", "thread-1")
	headers.Set("AH-Trace-Id", "trace-1")

	got := deriveClientSessionKey(headers, nil)
	want := hashRouteSessionKey("trace", "trace-1")
	if got != want {
		t.Fatalf("session key = %q, want %q", got, want)
	}
}

func TestDeriveClientSessionKeyKeepsThreadWhenTraceMissing(t *testing.T) {
	headers := http.Header{}
	headers.Set("AH-Thread-Id", "thread-1")

	got := deriveClientSessionKey(headers, nil)
	want := hashRouteSessionKey("thread", "thread-1")
	if got != want {
		t.Fatalf("session key = %q, want %q", got, want)
	}
}

func TestDeriveClientSessionKeySupportsAmpThreadAndClientRequestID(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Amp-Thread-Id", "amp-thread-1")
	got := deriveClientSessionInfo(headers, nil)
	want := hashRouteSessionKey("thread", "amp-thread-1")
	if got.Key != want || got.Source != "header:X-Amp-Thread-Id" {
		t.Fatalf("amp thread session = %+v, want key %q", got, want)
	}

	headers = http.Header{}
	headers.Set("X-Client-Request-Id", "client-request-1")
	got = deriveClientSessionInfo(headers, nil)
	want = hashRouteSessionKey("trace", "client-request-1")
	if got.Key != want || got.Source != "header:X-Client-Request-Id" {
		t.Fatalf("client request session = %+v, want key %q", got, want)
	}
}

func TestDeriveClientSessionKeyPrefersClientRequestWhenAmpThreadPresent(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Amp-Thread-Id", "amp-thread-1")
	headers.Set("X-Client-Request-Id", "client-request-1")

	got := deriveClientSessionInfo(headers, nil)
	want := hashRouteSessionKey("trace", "client-request-1")
	if got.Key != want || got.Source != "header:X-Client-Request-Id" {
		t.Fatalf("client request should win over amp thread, got %+v want key %q", got, want)
	}
}

func TestDeriveClientSessionKeyPrefersMetadataTraceWhenThreadPresent(t *testing.T) {
	req := &model.InternalLLMRequest{Metadata: map[string]string{
		"amp_thread_id":     "amp-thread-meta",
		"client_request_id": "client-request-meta",
	}}

	got := deriveClientSessionInfo(nil, req)
	want := hashRouteSessionKey("trace", "client-request-meta")
	if got.Key != want || got.Source != "metadata:client_request_id" {
		t.Fatalf("metadata client request should win over thread, got %+v want key %q", got, want)
	}
}

func TestDeriveClientSessionKeyFallsBackToMetadataUserID(t *testing.T) {
	req := &model.InternalLLMRequest{Metadata: map[string]string{"user_id": "claude-user-1"}}

	got := deriveClientSessionKey(nil, req)
	want := hashRouteSessionKey("user", "claude-user-1")
	if got != want {
		t.Fatalf("metadata session key = %q, want %q", got, want)
	}
}

func TestDeriveClientSessionKeyExtractsClaudeMetadataJSONSession(t *testing.T) {
	req := &model.InternalLLMRequest{Metadata: map[string]string{
		"user_id": `{"device_id":"dev","account_uuid":"acct","session_id":"019ea3aa-0000-7000-8000-000000000001"}`,
	}}

	got := deriveClientSessionInfo(nil, req)
	want := hashRouteSessionKey("anthropic-session", "019ea3aa-0000-7000-8000-000000000001")
	if got.Key != want {
		t.Fatalf("metadata session key = %q, want %q", got.Key, want)
	}
	if got.Source != "metadata:user_id:claude-session" {
		t.Fatalf("metadata source = %q", got.Source)
	}
}

func TestDeriveClientSessionKeyExtractsClaudeLegacySession(t *testing.T) {
	req := &model.InternalLLMRequest{Metadata: map[string]string{
		"user_id": "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_019ea3aa-0000-7000-8000-000000000002",
	}}

	got := deriveClientSessionKey(nil, req)
	want := hashRouteSessionKey("anthropic-session", "019ea3aa-0000-7000-8000-000000000002")
	if got != want {
		t.Fatalf("metadata session key = %q, want %q", got, want)
	}
}

func TestDeriveClientSessionKeyDoesNotExposeRawValue(t *testing.T) {
	got := deriveClientSessionKeyFromHeaders(http.Header{"Conversation_id": []string{"secret-conversation"}})
	if got == "" {
		t.Fatalf("expected derived key")
	}
	if got == "secret-conversation" {
		t.Fatalf("session key should be hashed, got raw value")
	}
}

func TestDeriveClientSessionInfoIncludesSafeSource(t *testing.T) {
	info := deriveClientSessionInfo(
		http.Header{"Session_id": []string{"secret-session"}},
		&model.InternalLLMRequest{Metadata: map[string]string{"user_id": "ignored"}},
	)
	if info.Key == "" {
		t.Fatalf("expected derived key")
	}
	if info.Source != "header:Session_id" {
		t.Fatalf("session source = %q, want header:Session_id", info.Source)
	}
	if info.Key == "secret-session" || info.Source == "secret-session" {
		t.Fatalf("session info must not expose raw value: %+v", info)
	}
}

func TestDeriveClientSessionInfoUsesCodexClientMetadata(t *testing.T) {
	req := &model.InternalLLMRequest{
		ClientMetadata: json.RawMessage(`{
			"x-codex-window-id":"codex-session-1:3",
			"x-codex-installation-id":"install-should-lose"
		}`),
	}

	got := deriveClientSessionInfo(nil, req)
	want := hashRouteSessionKey("codex-session", "codex-session-1")
	if got.Key != want || got.Source != "client_metadata:x-codex-window-id" {
		t.Fatalf("codex client metadata session = %+v, want key %q", got, want)
	}
}

func TestDeriveClientSessionInfoUsesCodexTurnMetadataBeforeInstallation(t *testing.T) {
	req := &model.InternalLLMRequest{
		ClientMetadata: json.RawMessage(`{
			"x-codex-turn-metadata":"{\"session_id\":\"turn-session-1\",\"thread_id\":\"thread-should-lose\"}",
			"x-codex-installation-id":"install-should-lose"
		}`),
	}

	got := deriveClientSessionInfo(nil, req)
	want := hashRouteSessionKey("codex-session", "turn-session-1")
	if got.Key != want || got.Source != "client_metadata:x-codex-turn-metadata:session_id" {
		t.Fatalf("codex turn metadata session = %+v, want key %q", got, want)
	}
}

func TestDeriveManagedClientSessionInfoAddsOctopusFallbacks(t *testing.T) {
	promptCacheKey := "project-alpha"
	previous := "resp_prev_alpha"
	user := "user-alpha"
	safety := "safety-alpha"

	tests := []struct {
		name   string
		req    *model.InternalLLMRequest
		scope  string
		value  string
		source string
	}{
		{
			name: "prompt cache",
			req: &model.InternalLLMRequest{
				PromptCacheKey: &promptCacheKey,
				Messages:       []model.Message{{Role: "user"}},
			},
			scope:  "prompt-cache",
			value:  promptCacheKey,
			source: "body:prompt_cache_key",
		},
		{
			name: "previous response",
			req: &model.InternalLLMRequest{
				PreviousResponseID: &previous,
				Messages:           []model.Message{{Role: "user"}},
			},
			scope:  "previous-response",
			value:  previous,
			source: "body:previous_response_id",
		},
		{
			name: "user",
			req: &model.InternalLLMRequest{
				User:     &user,
				Messages: []model.Message{{Role: "user"}},
			},
			scope:  "user",
			value:  user,
			source: "body:user",
		},
		{
			name: "safety identifier",
			req: &model.InternalLLMRequest{
				SafetyIdentifier: &safety,
				Messages:         []model.Message{{Role: "user"}},
			},
			scope:  "safety-identifier",
			value:  safety,
			source: "body:safety_identifier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveManagedClientSessionInfo(nil, tt.req)
			want := hashRouteSessionKey(tt.scope, tt.value)
			if got.Key != want || got.Source != tt.source {
				t.Fatalf("managed session = %+v, want key %q source %q", got, want, tt.source)
			}
		})
	}
}

func TestDeriveManagedClientSessionInfoRequestFingerprintIsOctopusOwned(t *testing.T) {
	req := &model.InternalLLMRequest{
		RawRequest: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`),
		Messages:   []model.Message{{Role: "user"}},
	}

	got := deriveManagedClientSessionInfo(nil, req)
	if got.Key == "" || got.Source != "octopus:request_fingerprint" {
		t.Fatalf("expected octopus request fingerprint fallback, got %+v", got)
	}
}
