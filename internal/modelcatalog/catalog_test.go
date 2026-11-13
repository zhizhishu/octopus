package modelcatalog

import "testing"

func TestRegisteredCapabilities(t *testing.T) {
	cases := []struct {
		id, family       string
		image, reasoning bool
	}{
		{"gemini-4-argon", "google", false, true},
		{"gemini-4-carbon", "google", false, true},
		{"gemini-4-hydrogen", "google", false, true},
		{"gemini-4-argon-high", "google", false, true},
		{"gemini-4-carbon-high", "google", false, true},
		{"gemini-4-hydrogen-xhigh", "google", false, true},
		{"gemini-4-argon-b", "google", false, true},
		{"gemini-4-carbon-cyber-permissive", "google", false, true},
		{"gemini-4-argon-20261013", "google", false, true},
		{"gemini-4-carbon-20261110", "google", false, true},
		{"google/gemini-4-hydrogen-xhigh-20261112", "google", false, true},
		{"gpt-image-2", "openai", true, false},
		{"openai/gpt-image-2.5", "openai", true, false},
		{"gpt-image-2.6", "openai", true, false},
		{"doubao-seedream-5-0-pro-260628", "bytedance", true, false},
		{"doubao-seedream-5-0-lite-260128", "bytedance", true, false},
		{"doubao-seedream-5-0-flash-260915", "bytedance", true, false},
		{"doubao-seedream-5-0-260128", "bytedance", true, false},
		{"doubao-seedream-5-5-261028", "bytedance", true, false},
		{"imagen-4.0-generate-001", "google", true, false},
		{"imagen-4.0-ultra-generate-001", "google", true, false},
		{"imagen-4.0-fast-generate-001", "google", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			metadata, ok := Lookup(tc.id)
			if !ok || metadata.Family != tc.family || metadata.ImageGeneration != tc.image || metadata.Reasoning != tc.reasoning {
				t.Fatalf("Lookup(%q) = %+v, %v", tc.id, metadata, ok)
			}
			if metadata.Tokenizer != "" {
				t.Fatalf("unverified tokenizer must remain unknown: %+v", metadata)
			}
		})
	}
}

func TestUnknownModelsStayUnknown(t *testing.T) {
	for _, id := range []string{"", "my-custom-model", "gemini-4-argon-unknown", "gemini-4-argon-20269999", "gemini-4-argonaut", "gpt-image-27", "claude-opus-4-6", "gpt-5.3-codex", "gemini-3.1-pro-preview"} {
		if metadata, ok := Lookup(id); ok || metadata != (Metadata{}) {
			t.Errorf("unregistered ID %q acquired capabilities: %+v", id, metadata)
		}
	}
}

func TestReferenceFamilyLabels(t *testing.T) {
	for _, pair := range [][2]string{{"claude", "anthropic"}, {"gpt", "openai"}, {"gemini", "google"}, {"deepseek", "deepseek"}} {
		if !SameFamily(pair[0], pair[1]) || !SameFamily(pair[1], pair[0]) {
			t.Errorf("same-family labels failed: %v", pair)
		}
	}
	for _, pair := range [][2]string{{"claude", "openai"}, {"gpt", "google"}, {"gemini", "anthropic"}, {"", ""}, {"unknown", "openai"}} {
		if SameFamily(pair[0], pair[1]) {
			t.Errorf("unrelated labels became equivalent: %v", pair)
		}
	}
}
