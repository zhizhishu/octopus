package model

import "testing"

func TestInternalRequestDetectsImageGenerationRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *InternalLLMRequest
	}{
		{
			name: "image modality",
			req:  &InternalLLMRequest{Modalities: []string{"text", "Image"}},
		},
		{
			name: "responses image tool",
			req:  &InternalLLMRequest{Tools: []Tool{{Type: "image_generation"}}},
		},
		{
			name: "image tool params",
			req:  &InternalLLMRequest{Tools: []Tool{{Type: "function", ImageGeneration: &ImageGeneration{Size: "1024x1024"}}}},
		},
		{
			name: "gpt image model",
			req:  &InternalLLMRequest{Model: "gpt-image-2"},
		},
		{
			name: "gpt 4o image model",
			req:  &InternalLLMRequest{Model: "gpt-4o-image"},
		},
		{
			name: "image2 alias",
			req:  &InternalLLMRequest{Model: "image2"},
		},
		{
			name: "gemini image model",
			req:  &InternalLLMRequest{Model: "google/gemini-2.5-flash-image-preview"},
		},
		{
			name: "imagen model",
			req:  &InternalLLMRequest{Model: "imagen-4.0-generate-001"},
		},
		{
			name: "grok image model",
			req:  &InternalLLMRequest{Model: "grok-imagine-image-pro"},
		},
		{
			name: "flux image model",
			req:  &InternalLLMRequest{Model: "flux.1-kontext-pro"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.req.IsImageGenerationRequest() {
				t.Fatalf("expected image generation request")
			}
		})
	}

	if (&InternalLLMRequest{Model: "gpt-5.5", Modalities: []string{"text"}, Tools: []Tool{{Type: "function"}}}).IsImageGenerationRequest() {
		t.Fatalf("plain text/function request must not be treated as image generation")
	}
	if (&InternalLLMRequest{Model: "grok-4-fast-reasoning"}).IsImageGenerationRequest() {
		t.Fatalf("plain Grok text model must not be treated as image generation")
	}
	if (&InternalLLMRequest{Model: "gemini-2.5-pro"}).IsImageGenerationRequest() {
		t.Fatalf("plain Gemini text model must not be treated as image generation")
	}
}
