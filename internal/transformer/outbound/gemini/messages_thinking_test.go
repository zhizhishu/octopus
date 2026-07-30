package gemini

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// functionCallThoughtSignature walks the converted request and returns the
// ThoughtSignature attached to the first functionCall part it finds.
func functionCallThoughtSignature(t *testing.T, req *model.GeminiGenerateContentRequest) string {
	t.Helper()
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				return part.ThoughtSignature
			}
		}
	}
	t.Fatalf("no functionCall part found in %#v", req.Contents)
	return ""
}

// Fix 4a: an assistant message with a ReasoningSignature re-emits it as the
// functionCall part's ThoughtSignature; absent a signature the documented
// skip sentinel is emitted so replayed calls still pass Gemini validation.
func TestGeminiOutboundReemitsThoughtSignature(t *testing.T) {
	buildAssistantToolCall := func(sig *string) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: "gemini-test",
			Messages: []model.Message{{
				Role:               "assistant",
				ReasoningSignature: sig,
				ToolCalls: []model.ToolCall{{
					ID:    "call_lookup_0",
					Type:  "function",
					Index: 0,
					Function: model.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"octopus"}`,
					},
				}},
			}},
		}
	}

	t.Run("real_signature_preserved", func(t *testing.T) {
		converted := convertLLMToGeminiRequest(buildAssistantToolCall(stringPtr("sig-xyz-789")))
		if got := functionCallThoughtSignature(t, converted); got != "sig-xyz-789" {
			t.Fatalf("expected real thoughtSignature re-emitted, got %q", got)
		}
	})

	t.Run("missing_signature_uses_sentinel", func(t *testing.T) {
		converted := convertLLMToGeminiRequest(buildAssistantToolCall(nil))
		if got := functionCallThoughtSignature(t, converted); got != "skip_thought_signature_validator" {
			t.Fatalf("expected skip sentinel thoughtSignature, got %q", got)
		}
	})
}

// Fix 4b: a request carrying ReasoningEffort re-emits generationConfig.thinkingConfig.thinkingLevel,
// and TransformerMetadata["gemini_include_thoughts"] drives thinkingConfig.includeThoughts
// (defaulting to true when the metadata is absent).
func TestGeminiOutboundReemitsThinkingLevelAndIncludeThoughts(t *testing.T) {
	user := "hello"
	baseMessages := []model.Message{{
		Role:    "user",
		Content: model.MessageContent{Content: &user},
	}}

	cases := []struct {
		name        string
		effort      string
		metadata    map[string]string
		wantLevel   string
		wantInclude bool
	}{
		{
			name:        "high_include_false",
			effort:      "high",
			metadata:    map[string]string{"gemini_include_thoughts": "false"},
			wantLevel:   "high",
			wantInclude: false,
		},
		{
			name:        "medium_include_true",
			effort:      "medium",
			metadata:    map[string]string{"gemini_include_thoughts": "true"},
			wantLevel:   "medium",
			wantInclude: true,
		},
		{
			name:        "low_include_defaults_true",
			effort:      "low",
			metadata:    nil,
			wantLevel:   "low",
			wantInclude: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &model.InternalLLMRequest{
				Model:               "gemini-test",
				Messages:            baseMessages,
				ReasoningEffort:     tc.effort,
				TransformerMetadata: tc.metadata,
			}
			converted := convertLLMToGeminiRequest(req)
			if converted.GenerationConfig == nil || converted.GenerationConfig.ThinkingConfig == nil {
				t.Fatalf("expected thinkingConfig in generation config, got %#v", converted.GenerationConfig)
			}
			tcfg := converted.GenerationConfig.ThinkingConfig
			if tcfg.ThinkingLevel != tc.wantLevel {
				t.Fatalf("expected thinkingLevel %q, got %q", tc.wantLevel, tcfg.ThinkingLevel)
			}
			if tcfg.IncludeThoughts != tc.wantInclude {
				t.Fatalf("expected includeThoughts %v, got %v", tc.wantInclude, tcfg.IncludeThoughts)
			}
		})
	}
}
