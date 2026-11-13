package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCatalogLookupPreservesRequestBytes(t *testing.T) {
	for _, tc := range []struct {
		id    string
		image bool
	}{
		{"claude-opus-4-6", false},
		{"gpt-5.3-codex", false},
		{"gemini-3.1-pro-preview", false},
		{"gemini-3-pro-image", true},
		{"gpt-image-1", true},
		{"google/gemini-4-argon-high", false},
		{"gemini-4-carbon-high-20261110", false},
		{"gemini-4-hydrogen-xhigh-20261112", false},
		{"openai/gpt-image-2.5", true},
		{"gpt-image-2.6", true},
		{"doubao-seedream-5-5-261028", true},
		{"imagen-4.0-generate-001", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			request := InternalLLMRequest{Model: tc.id}
			before, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if image := request.IsImageGenerationRequest(); image != tc.image {
				t.Errorf("image capability = %v, want %v", image, tc.image)
			}
			after, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || request.Model != tc.id {
				t.Fatalf("metadata lookup mutated the request: before=%s after=%s", before, after)
			}
		})
	}
}
