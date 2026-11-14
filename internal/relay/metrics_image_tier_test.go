package relay

import (
	"bytes"
	"encoding/json"
	"testing"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

func TestChatImageDefaultTierObservation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, model, metadata, quality string
		want                                     bool
	}{
		{"chat-default", "chat", "gemini-3-pro-image", "", "", true},
		{"aspect-only", "chat", "gemini-3-pro-image", `{"aspect_ratio":"16:9"}`, "", true},
		{"explicit-2k", "chat", "gemini-3-pro-image", `{"image_size":"2k"}`, "", false},
		{"explicit-4k", "chat", "gemini-3-pro-image", `{"image_size":"4K"}`, "", false},
		{"invalid-tier", "chat", "gemini-3-pro-image", `{"image_size":"unexpected"}`, "", true},
		{"invalid-json", "chat", "gemini-3-pro-image", "{", "", true},
		{"tool-quality", "chat", "gemini-3-pro-image", "", "high", false},
		{"explicit-auto", "chat", "gemini-3-pro-image", "", "auto", false},
		{"images-endpoint", "images_generations", "gemini-3-pro-image", "", "", false},
		{"responses-endpoint", "responses", "gemini-3-pro-image", "", "", false},
		{"ordinary-chat", "chat", "gemini-3.1-pro-preview", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &transformerModel.InternalLLMRequest{Model: tc.model}
			if tc.metadata != "" {
				request.TransformerMetadata = map[string]string{"gemini_image_config": tc.metadata}
			}
			if tc.quality != "" {
				request.Tools = []transformerModel.Tool{{Type: "image_generation", ImageGeneration: &transformerModel.ImageGeneration{Quality: tc.quality}}}
			}
			before, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			metrics := &RelayMetrics{RequestEndpoint: tc.endpoint, InternalRequest: request}
			if got := metrics.chatImageUsesUpstreamDefaultTier(); got != tc.want {
				t.Errorf("default-tier observation=%v, want %v", got, tc.want)
			}
			after, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("observation changed request bytes: %s -> %s", before, after)
			}
		})
	}
	if (&RelayMetrics{RequestEndpoint: "chat"}).chatImageUsesUpstreamDefaultTier() {
		t.Fatal("missing request must not claim an image tier")
	}
}
