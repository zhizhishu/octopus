package authropic

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestMessageOutboundTransformRequestNormalizesAnthropicV1Path(t *testing.T) {
	content := "ping"
	maxTokens := int64(16)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4.7",
		MaxTokens: &maxTokens,
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
	}

	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "base without v1",
			baseURL: "https://api.anthropic.com",
			want:    "/v1/messages",
		},
		{
			name:    "base with v1",
			baseURL: "https://api.anthropic.com/v1",
			want:    "/v1/messages",
		},
		{
			name:    "base accidentally includes v1 messages",
			baseURL: "https://api.anthropic.com/v1/messages",
			want:    "/v1/messages",
		},
		{
			name:    "cliproxy provider base",
			baseURL: "https://cliproxy.example/api/provider/anthropic",
			want:    "/api/provider/anthropic/v1/messages",
		},
		{
			name:    "cliproxy provider base with v1 suffix",
			baseURL: "https://cliproxy.example/api/provider/anthropic/v1",
			want:    "/api/provider/anthropic/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, tt.baseURL, "test-key")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			if httpReq.URL.Path != tt.want {
				t.Fatalf("URL path = %q, want %q", httpReq.URL.Path, tt.want)
			}
		})
	}
}

func TestMessageOutboundTransformRequestMergesBaseAndInboundQuery(t *testing.T) {
	content := "ping"
	maxTokens := int64(16)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4.7",
		MaxTokens: &maxTokens,
		Query: url.Values{
			"client": []string{"mobile"},
		},
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
	}

	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://cpa.example/v1/messages?beta=true", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if got := httpReq.URL.Query().Get("beta"); got != "true" {
		t.Fatalf("base query beta = %q, want true", got)
	}
	if got := httpReq.URL.Query().Get("client"); got != "mobile" {
		t.Fatalf("inbound query client = %q, want mobile", got)
	}
}

func TestMessageOutboundTransformRequestKeepsCPAOneMillionMessagesShape(t *testing.T) {
	content := "ping"
	maxTokens := int64(32000)
	stream := true
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4-7[1m]",
		MaxTokens: &maxTokens,
		Stream:    &stream,
		Query: url.Values{
			"beta": []string{"true"},
		},
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
	}

	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://cpa.example/v1/messages?provider=cpa", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if httpReq.URL.Path != "/v1/messages" {
		t.Fatalf("URL path = %q, want /v1/messages", httpReq.URL.Path)
	}
	if got := httpReq.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("streaming Claude request Accept = %q, want Claude CLI-like application/json", got)
	}
	if got := httpReq.URL.Query().Get("provider"); got != "cpa" {
		t.Fatalf("base query provider = %q, want cpa", got)
	}
	if got := httpReq.URL.Query().Get("beta"); got != "true" {
		t.Fatalf("inbound query beta = %q, want true", got)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	bodyText := string(body)
	for _, want := range []string{
		`"model":"claude-opus-4-8"`,
		`"max_tokens":32000`,
		`"stream":true`,
		`"messages":[`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected body to contain %s, got %s", want, bodyText)
		}
	}
}

func TestMessageOutboundTransformRequestPreservesClaudeContextManagement(t *testing.T) {
	content := "ping"
	maxTokens := int64(64000)
	req := &transformerModel.InternalLLMRequest{
		Model:                      "claude-opus-4-8[1m]",
		MaxTokens:                  &maxTokens,
		ReasoningEffort:            "high",
		AdaptiveThinking:           true,
		ServiceTier:                ptrString("auto"),
		Stream:                     ptrBool(true),
		AnthropicContextManagement: []byte(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`),
		TransformOptions: transformerModel.TransformOptions{
			AnthropicBetas: []string{"custom-beta-2026-06-08"},
		},
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://anyrouter.top", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if got := httpReq.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("streaming Anthropic request Accept-Encoding = %q, want identity", got)
	}
	beta := httpReq.Header.Get("Anthropic-Beta")
	for _, want := range []string{"custom-beta-2026-06-08", transformerModel.AnthropicOneMillionBeta} {
		if !strings.Contains(beta, want) {
			t.Fatalf("expected beta %q in %q", want, beta)
		}
	}
	bodyText := string(body)
	for _, want := range []string{
		`"model":"claude-opus-4-8"`,
		`"service_tier":"auto"`,
		`"thinking":{"type":"adaptive"}`,
		`"output_config":{"effort":"high"}`,
		`"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected body to contain %s, got %s", want, bodyText)
		}
	}
}

func TestMessageOutboundTransformRequestPreservesNativeClaudeTitleShape(t *testing.T) {
	content := "Reply with exactly OK. Do not use tools."
	maxTokens := int64(64000)
	req := &transformerModel.InternalLLMRequest{
		Model:                 "claude-opus-4-8",
		MaxTokens:             &maxTokens,
		Stream:                ptrBool(true),
		AnthropicThinking:     []byte(`{"type":"disabled"}`),
		AnthropicOutputConfig: []byte(`{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}`),
		AnthropicToolsPresent: true,
		TransformOptions: transformerModel.TransformOptions{
			AnthropicOneMillionBeta: true,
			AnthropicBetas:          []string{"structured-outputs-2025-12-15"},
		},
		Metadata: map[string]string{
			"user_id": `{"device_id":"device","account_uuid":"","session_id":"session"}`,
		},
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://anyrouter.top", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	beta := httpReq.Header.Get("Anthropic-Beta")
	for _, want := range []string{"structured-outputs-2025-12-15", transformerModel.AnthropicOneMillionBeta} {
		if !strings.Contains(beta, want) {
			t.Fatalf("expected beta %q in %q", want, beta)
		}
	}
	bodyText := string(body)
	for _, want := range []string{
		`"model":"claude-opus-4-8"`,
		`"thinking":{"type":"disabled"}`,
		`"format":`,
		`"json_schema"`,
		`"additionalProperties":false`,
		`"tools":[]`,
		`"metadata":{"user_id":"{\"device_id\":\"device\"`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected body to contain %s, got %s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, "context_management") {
		t.Fatalf("native Claude title request must not synthesize context_management: %s", bodyText)
	}
}

func ptrString(value string) *string {
	return &value
}

func ptrBool(value bool) *bool {
	return &value
}

func TestMessageOutboundTransformRequestMapsOpusOneMillionShortcut(t *testing.T) {
	content := "ping"
	maxTokens := int64(32000)
	stream := true
	req := &transformerModel.InternalLLMRequest{
		Model:     "opus[1m]",
		MaxTokens: &maxTokens,
		Stream:    &stream,
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://anyrouter.top", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if bodyText := string(body); !strings.Contains(bodyText, `"model":"claude-opus-4-8"`) {
		t.Fatalf("expected opus[1m] to map to claude-opus-4-8, got %s", bodyText)
	}
}

func TestMessageOutboundTransformRequestAuthHeadersByHost(t *testing.T) {
	content := "ping"
	maxTokens := int64(16)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4-8[1m]",
		MaxTokens: &maxTokens,
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	officialReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://api.anthropic.com", "test-key")
	if err != nil {
		t.Fatalf("official TransformRequest returned error: %v", err)
	}
	if got := officialReq.Header.Get("X-API-Key"); got != "test-key" {
		t.Fatalf("official x-api-key = %q, want test-key", got)
	}
	if got := officialReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("official authorization should be empty, got %q", got)
	}

	routerReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://anyrouter.top", "test-key")
	if err != nil {
		t.Fatalf("router TransformRequest returned error: %v", err)
	}
	if got := routerReq.Header.Get("X-API-Key"); got != "test-key" {
		t.Fatalf("router x-api-key = %q, want test-key", got)
	}
	if got := routerReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("router authorization = %q, want bearer", got)
	}
}
