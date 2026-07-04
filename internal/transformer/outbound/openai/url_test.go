package openai

import (
	"context"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func testChatRequest() *transformerModel.InternalLLMRequest {
	content := "ping"
	return &transformerModel.InternalLLMRequest{
		Model: "gpt-4o",
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
	}
}

func TestChatOutboundTransformRequestNormalizesOpenAIPath(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "base without v1",
			baseURL: "https://api.openai.com",
			want:    "/v1/chat/completions",
		},
		{
			name:    "base with v1",
			baseURL: "https://api.openai.com/v1",
			want:    "/v1/chat/completions",
		},
		{
			name:    "base accidentally includes responses endpoint",
			baseURL: "https://api.openai.com/v1/responses",
			want:    "/v1/chat/completions",
		},
		{
			name:    "cliproxy provider base",
			baseURL: "https://cliproxy.example/api/provider/openai",
			want:    "/api/provider/openai/v1/chat/completions",
		},
		{
			name:    "cliproxy provider base with v1 suffix",
			baseURL: "https://cliproxy.example/api/provider/openai/v1",
			want:    "/api/provider/openai/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpReq, err := (&ChatOutbound{}).TransformRequest(context.Background(), testChatRequest(), tt.baseURL, "test-key")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			if httpReq.URL.Path != tt.want {
				t.Fatalf("URL path = %q, want %q", httpReq.URL.Path, tt.want)
			}
		})
	}
}

func TestCustomChatOutboundTransformRequestPreservesFullChatEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "domestic provider full chat endpoint without v1",
			baseURL: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			want:    "/api/coding/paas/v4/chat/completions",
		},
		{
			name:    "openai compatible full v1 chat endpoint",
			baseURL: "https://proxy.example/custom/v1/chat/completions",
			want:    "/custom/v1/chat/completions",
		},
		{
			name:    "base without chat endpoint still uses standard openai chat path",
			baseURL: "https://proxy.example/custom",
			want:    "/custom/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpReq, err := (&CustomChatOutbound{}).TransformRequest(context.Background(), testChatRequest(), tt.baseURL, "test-key")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			if httpReq.URL.Path != tt.want {
				t.Fatalf("URL path = %q, want %q", httpReq.URL.Path, tt.want)
			}
		})
	}
}

// TestResponseOutboundTransformRequestOmitsAcceptEncoding locks the codex byte-exact
// shape: the genuine codex CLI (Rust/reqwest) sends NO Accept-Encoding header, so
// ResponseOutbound must not set one. Auto-injection of "gzip" / "gzip, deflate, br" is
// suppressed at the transport layer (DisableCompression, see internal/client), so a
// request that omits the header here reaches the wire with none.
func TestResponseOutboundTransformRequestOmitsAcceptEncoding(t *testing.T) {
	httpReq, err := (&ResponseOutbound{}).TransformRequest(context.Background(), testChatRequest(), "https://anyrouter.top", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if _, ok := httpReq.Header["Accept-Encoding"]; ok {
		t.Fatalf("codex Responses request must not set Accept-Encoding, got %q", httpReq.Header.Get("Accept-Encoding"))
	}
}

func TestResponseOutboundTransformRequestNormalizesOpenAIPath(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "base without v1",
			baseURL: "https://api.openai.com",
			want:    "/v1/responses",
		},
		{
			name:    "base with v1",
			baseURL: "https://api.openai.com/v1",
			want:    "/v1/responses",
		},
		{
			name:    "base accidentally includes chat endpoint",
			baseURL: "https://api.openai.com/v1/chat/completions",
			want:    "/v1/responses",
		},
		{
			name:    "cliproxy provider base",
			baseURL: "https://cliproxy.example/api/provider/openai",
			want:    "/api/provider/openai/v1/responses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpReq, err := (&ResponseOutbound{}).TransformRequest(context.Background(), testChatRequest(), tt.baseURL, "test-key")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			if httpReq.URL.Path != tt.want {
				t.Fatalf("URL path = %q, want %q", httpReq.URL.Path, tt.want)
			}
		})
	}
}
