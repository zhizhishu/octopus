package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestApplyChannelWireHeadersReassertsCodexPairAfterCustomHeaders(t *testing.T) {
	promptCacheKey := "wire-contract-session"
	request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	channel := &dbmodel.Channel{
		Type: outbound.OutboundTypeOpenAIResponse,
		CustomHeader: []dbmodel.CustomHeader{
			{HeaderKey: "Originator", HeaderValue: "custom-originator"},
			{HeaderKey: "User-Agent", HeaderValue: "custom-user-agent"},
			{HeaderKey: "Accept-Encoding", HeaderValue: "identity"},
		},
	}

	ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
		Channel:     channel,
		InboundType: inbound.InboundTypeOpenAIResponse,
		InternalRequest: &transformermodel.InternalLLMRequest{
			Model:          "gpt-5.6-sol",
			PromptCacheKey: &promptCacheKey,
		},
	})

	if got := request.Header.Get("Originator"); got != defaultCodexOriginator {
		t.Fatalf("Originator = %q, want %q", got, defaultCodexOriginator)
	}
	if got := request.Header.Get("User-Agent"); got != defaultCodexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if _, exists := request.Header["Accept-Encoding"]; exists {
		t.Fatalf("Codex wire contract must remove Accept-Encoding, got %q", request.Header.Get("Accept-Encoding"))
	}
	if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "true" {
		t.Fatalf("Lite header = %q, want true", got)
	}
}

func TestApplyChannelWireHeadersCloakNeverUsesGenericIdentity(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("Originator", "leaked-codex")
	request.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
	channel := &dbmodel.Channel{
		Type:  outbound.OutboundTypeOpenAIResponse,
		Cloak: dbmodel.ChannelCloak{Mode: "never"},
		CustomHeader: []dbmodel.CustomHeader{
			{HeaderKey: "Originator", HeaderValue: "custom-codex"},
			{HeaderKey: "User-Agent", HeaderValue: "custom-agent"},
		},
	}

	ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
		Channel:     channel,
		InboundType: inbound.InboundTypeOpenAIResponse,
	})

	if got := request.Header.Get("User-Agent"); got != dbmodel.DefaultGenericUA {
		t.Fatalf("User-Agent = %q, want generic %q", got, dbmodel.DefaultGenericUA)
	}
	if got := request.Header.Get("Originator"); got != "" {
		t.Fatalf("cloak=never must strip Codex Originator, got %q", got)
	}
	if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "" {
		t.Fatalf("cloak=never must strip Lite header, got %q", got)
	}
}

func TestApplyChannelWireHeadersDropsLiteForGPT55AfterCustomHeaders(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{model: "gpt-5.5", want: ""},
		{model: "gpt-5.5-pro", want: ""},
		{model: "gpt-5.6-sol", want: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			request.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
			channel := &dbmodel.Channel{
				Type: outbound.OutboundTypeOpenAIResponse,
				CustomHeader: []dbmodel.CustomHeader{
					{HeaderKey: "X-Openai-Internal-Codex-Responses-Lite", HeaderValue: "true"},
				},
			}

			ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
				Channel:     channel,
				InboundType: inbound.InboundTypeOpenAIResponse,
				InternalRequest: &transformermodel.InternalLLMRequest{
					Model: tc.model,
				},
			})

			if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != tc.want {
				if tc.want == "" {
					t.Fatalf("%s must not send the incompatible Lite header, got %q", tc.model, got)
				}
				t.Fatalf("%s Lite header = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestApplyChannelWireHeadersChatCodexRepairsPairDropsLiteAndAcceptEncoding(t *testing.T) {
	cases := []struct {
		name string
		typ  outbound.OutboundType
	}{
		{"openai_chat", outbound.OutboundTypeOpenAIChat},
		{"custom_openai_chat", outbound.OutboundTypeCustomOpenAIChat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ := tc.typ
			request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			channel := &dbmodel.Channel{
				Type: typ,
				Cloak: dbmodel.ChannelCloak{
					Mode:  "always",
					Shape: "codex",
				},
				CustomHeader: []dbmodel.CustomHeader{
					{HeaderKey: "User-Agent", HeaderValue: "codex_exec/9.9.9"},
					{HeaderKey: "Originator", HeaderValue: "not-the-ua-token"},
					{HeaderKey: "Accept-Encoding", HeaderValue: "identity"},
					{HeaderKey: "X-Openai-Internal-Codex-Responses-Lite", HeaderValue: "true"},
				},
			}

			ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
				Channel:     channel,
				InboundType: inbound.InboundTypeOpenAIChat,
				InternalRequest: &transformermodel.InternalLLMRequest{
					Model: "gpt-5.6-sol",
				},
			})

			ua := request.Header.Get("User-Agent")
			originator := request.Header.Get("Originator")
			token, _, _ := strings.Cut(strings.ToLower(ua), "/")
			if originator == "" || originator != token {
				t.Fatalf("type %d Originator %q must equal UA leading token %q (ua %q)", typ, originator, token, ua)
			}
			if ua == "codex_exec/9.9.9" || originator == "not-the-ua-token" {
				t.Fatalf("type %d custom header split the codex pair: ua %q originator %q", typ, ua, originator)
			}
			if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "" {
				t.Fatalf("type %d chat-codex must not send Lite, got %q", typ, got)
			}
			if _, exists := request.Header["Accept-Encoding"]; exists {
				t.Fatalf("type %d chat-codex must drop Accept-Encoding, got %q", typ, request.Header.Get("Accept-Encoding"))
			}
		})
	}
}

func TestApplyChannelWireHeadersResponsesStillSendsLite(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	channel := &dbmodel.Channel{
		Type: outbound.OutboundTypeOpenAIResponse,
		CustomHeader: []dbmodel.CustomHeader{
			{HeaderKey: "User-Agent", HeaderValue: "custom-user-agent"},
			{HeaderKey: "Originator", HeaderValue: "custom-originator"},
			{HeaderKey: "Accept-Encoding", HeaderValue: "identity"},
		},
	}

	ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
		Channel:     channel,
		InboundType: inbound.InboundTypeOpenAIResponse,
		InternalRequest: &transformermodel.InternalLLMRequest{
			Model: "gpt-5.6-sol",
		},
	})

	if got := request.Header.Get("Originator"); got != defaultCodexOriginator {
		t.Fatalf("Originator = %q, want %q", got, defaultCodexOriginator)
	}
	if got := request.Header.Get("User-Agent"); got != defaultCodexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if _, exists := request.Header["Accept-Encoding"]; exists {
		t.Fatalf("Responses codex must still drop Accept-Encoding, got %q", request.Header.Get("Accept-Encoding"))
	}
	if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "true" {
		t.Fatalf("Responses Lite header = %q, want true", got)
	}
}

func TestApplyChannelWireHeadersChatClaudeAndGenericStayUntouched(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		channel := &dbmodel.Channel{
			Type: outbound.OutboundTypeOpenAIChat,
			Cloak: dbmodel.ChannelCloak{
				Mode:  "always",
				Shape: "claude",
			},
			CustomHeader: []dbmodel.CustomHeader{
				{HeaderKey: "Accept-Encoding", HeaderValue: "gzip, deflate, br, zstd"},
			},
		}
		ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
			Channel:     channel,
			InboundType: inbound.InboundTypeOpenAIChat,
			InternalRequest: &transformermodel.InternalLLMRequest{
				Model: "claude-opus-4-8",
			},
		})
		if got := request.Header.Get("User-Agent"); got != defaultClaudeUserAgent {
			t.Fatalf("claude-shape UA = %q, want %q", got, defaultClaudeUserAgent)
		}
		if got := request.Header.Get("Originator"); got != "" {
			t.Fatalf("claude shape must not gain a codex Originator, got %q", got)
		}
		if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "" {
			t.Fatalf("claude shape must not send Lite, got %q", got)
		}
		if got := request.Header.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
			t.Fatalf("claude shape Accept-Encoding = %q, want it preserved", got)
		}
	})

	t.Run("generic", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		channel := &dbmodel.Channel{
			Type:  outbound.OutboundTypeOpenAIChat,
			Cloak: dbmodel.ChannelCloak{Mode: "always", Shape: ""},
			CustomHeader: []dbmodel.CustomHeader{
				{HeaderKey: "User-Agent", HeaderValue: "CustomAgent/1.0"},
				{HeaderKey: "Originator", HeaderValue: "leaked-originator"},
				{HeaderKey: "Accept-Encoding", HeaderValue: "gzip"},
			},
		}
		ApplyChannelWireHeaders(request, ChannelWireHeaderOptions{
			Channel:     channel,
			InboundType: inbound.InboundTypeOpenAIChat,
			InternalRequest: &transformermodel.InternalLLMRequest{
				Model: "gpt-4o",
			},
		})
		if got := request.Header.Get("User-Agent"); got != "CustomAgent/1.0" {
			t.Fatalf("generic chat must keep the custom UA, got %q", got)
		}
		if got := request.Header.Get("Originator"); got != "leaked-originator" {
			t.Fatalf("generic chat must not re-pair Originator, got %q", got)
		}
		if got := request.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Fatalf("generic chat must keep Accept-Encoding, got %q", got)
		}
		if got := request.Header.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "" {
			t.Fatalf("generic chat must not send Lite, got %q", got)
		}
	})
}

func TestApplyParamOverrideUsesRelayNullDeletionSemantics(t *testing.T) {
	requestBody := `{"model":"gpt-5.6-sol","temperature":0.7,"store":true}`
	request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	overrideJSON := `{"temperature":null,"store":false}`

	if err := ApplyParamOverride(request, &overrideJSON); err != nil {
		t.Fatalf("apply param override: %v", err)
	}
	updatedBody, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read updated body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(updatedBody, &payload); err != nil {
		t.Fatalf("decode updated body: %v", err)
	}
	if _, exists := payload["temperature"]; exists {
		t.Fatalf("null override must delete temperature, got %#v", payload["temperature"])
	}
	if got := payload["store"]; got != false {
		t.Fatalf("store = %#v, want false", got)
	}
	if got := payload["model"]; got != "gpt-5.6-sol" {
		t.Fatalf("unrelated model changed to %#v", got)
	}
}
