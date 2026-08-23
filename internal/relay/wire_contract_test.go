package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
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

