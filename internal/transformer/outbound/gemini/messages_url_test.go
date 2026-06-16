package gemini

import (
	"context"
	"encoding/json"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func testGeminiRequest(stream bool) *transformerModel.InternalLLMRequest {
	content := "ping"
	return &transformerModel.InternalLLMRequest{
		Model: "gemini-pro",
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
		Stream: &stream,
	}
}

func TestMessagesOutboundImageGenerationSynthesizesGeminiModalities(t *testing.T) {
	content := "draw a tiny octopus sticker"
	stream := false
	req := &transformerModel.InternalLLMRequest{
		Model: "gemini-2.5-flash-image",
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				Content: &content,
			},
		}},
		Stream: &stream,
		Tools: []transformerModel.Tool{{
			Type: "image_generation",
			ImageGeneration: &transformerModel.ImageGeneration{
				Size:    "1792x1024",
				Quality: "high",
			},
		}},
	}

	httpReq, err := (&MessagesOutbound{}).TransformRequest(context.Background(), req, "https://generativelanguage.googleapis.com/v1beta/models", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	var body transformerModel.GeminiGenerateContentRequest
	if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
		t.Fatalf("decode gemini request: %v", err)
	}
	if body.GenerationConfig == nil {
		t.Fatalf("expected generationConfig")
	}
	if got := body.GenerationConfig.ResponseModalities; len(got) != 2 || got[0] != "TEXT" || got[1] != "IMAGE" {
		t.Fatalf("unexpected responseModalities: %#v", got)
	}
	if body.GenerationConfig.ImageConfig == nil ||
		body.GenerationConfig.ImageConfig.AspectRatio != "16:9" ||
		body.GenerationConfig.ImageConfig.ImageSize != "2K" {
		t.Fatalf("unexpected imageConfig: %#v", body.GenerationConfig.ImageConfig)
	}
}

func TestMessagesOutboundTransformRequestNormalizesGeminiPath(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		stream  bool
		want    string
	}{
		{
			name:    "base without v1beta",
			baseURL: "https://generativelanguage.googleapis.com",
			want:    "/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "base with v1beta",
			baseURL: "https://generativelanguage.googleapis.com/v1beta",
			want:    "/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "base accidentally includes models suffix",
			baseURL: "https://generativelanguage.googleapis.com/v1beta/models",
			want:    "/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "base accidentally includes concrete generate endpoint",
			baseURL: "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
			want:    "/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "cliproxy provider base",
			baseURL: "https://cliproxy.example/api/provider/gemini",
			want:    "/api/provider/gemini/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "cliproxy provider base with v1beta suffix",
			baseURL: "https://cliproxy.example/api/provider/gemini/v1beta",
			want:    "/api/provider/gemini/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:    "stream endpoint strips accidental generate endpoint",
			baseURL: "https://cliproxy.example/api/provider/gemini/v1beta/models/gemini-pro:generateContent",
			stream:  true,
			want:    "/api/provider/gemini/v1beta/models/gemini-pro:streamGenerateContent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpReq, err := (&MessagesOutbound{}).TransformRequest(context.Background(), testGeminiRequest(tt.stream), tt.baseURL, "test-key")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			if httpReq.URL.Path != tt.want {
				t.Fatalf("URL path = %q, want %q", httpReq.URL.Path, tt.want)
			}
		})
	}
}
