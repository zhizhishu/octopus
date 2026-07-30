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

	t.Run("gemini_tagged_signature_preserved", func(t *testing.T) {
		// Only a Gemini-tagged signature is re-emitted (as its raw value); a bare or
		// foreign signature falls back to the sentinel (see foreign_signature_uses_sentinel).
		converted := convertLLMToGeminiRequest(buildAssistantToolCall(stringPtr(model.TagGeminiThoughtSignature("sig-xyz-789"))))
		if got := functionCallThoughtSignature(t, converted); got != "sig-xyz-789" {
			t.Fatalf("expected raw Gemini thoughtSignature re-emitted, got %q", got)
		}
	})

	t.Run("missing_signature_uses_sentinel", func(t *testing.T) {
		converted := convertLLMToGeminiRequest(buildAssistantToolCall(nil))
		if got := functionCallThoughtSignature(t, converted); got != "skip_thought_signature_validator" {
			t.Fatalf("expected skip sentinel thoughtSignature, got %q", got)
		}
	})
}

// FIX A4: thoughtSignatureForMessage returns the raw ONLY for a Gemini-tagged
// signature; a foreign (redacted-tagged) or bare one falls back to the sentinel,
// and more than one functionCall always uses the sentinel (a single message-level
// signature cannot be attributed to a specific parallel call).
func TestGeminiOutboundThoughtSignatureSourceTagging(t *testing.T) {
	toolCall := func(id string) model.ToolCall {
		return model.ToolCall{ID: id, Type: "function", Function: model.FunctionCall{Name: "lookup", Arguments: "{}"}}
	}
	build := func(sig *string, calls ...model.ToolCall) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: "gemini-test",
			Messages: []model.Message{{
				Role:               "assistant",
				ReasoningSignature: sig,
				ToolCalls:          calls,
			}},
		}
	}
	const sentinel = "skip_thought_signature_validator"

	t.Run("foreign_redacted_uses_sentinel", func(t *testing.T) {
		sig := model.EncodeRedactedThinkingSignature("REDACTED")
		if got := functionCallThoughtSignature(t, convertLLMToGeminiRequest(build(&sig, toolCall("c0")))); got != sentinel {
			t.Fatalf("redacted-tagged signature must use sentinel, got %q", got)
		}
	})
	t.Run("bare_uses_sentinel", func(t *testing.T) {
		sig := "bare-thinking-sig"
		if got := functionCallThoughtSignature(t, convertLLMToGeminiRequest(build(&sig, toolCall("c0")))); got != sentinel {
			t.Fatalf("bare signature must use sentinel, got %q", got)
		}
	})
	t.Run("gemini_tagged_single_call_returns_raw", func(t *testing.T) {
		sig := model.TagGeminiThoughtSignature("g-raw")
		if got := functionCallThoughtSignature(t, convertLLMToGeminiRequest(build(&sig, toolCall("c0")))); got != "g-raw" {
			t.Fatalf("gemini-tagged single call must return raw, got %q", got)
		}
	})
	t.Run("gemini_tagged_multiple_calls_uses_sentinel", func(t *testing.T) {
		sig := model.TagGeminiThoughtSignature("g-raw")
		converted := convertLLMToGeminiRequest(build(&sig, toolCall("c0"), toolCall("c1")))
		seen := 0
		for _, content := range converted.Contents {
			for _, part := range content.Parts {
				if part.FunctionCall == nil {
					continue
				}
				seen++
				if part.ThoughtSignature != sentinel {
					t.Fatalf("multiple tool calls must use sentinel, got %q", part.ThoughtSignature)
				}
			}
		}
		if seen != 2 {
			t.Fatalf("expected 2 functionCall parts, got %d", seen)
		}
	})
}

// FIX C: thinkingLevel is only set from ReasoningEffort when it is a valid Gemini
// enum (low/medium/high). An out-of-range effort (xhigh) leaves it empty so an
// invalid enum is never sent, while the thinkingConfig itself is still produced.
func TestGeminiOutboundThinkingLevelAllowlist(t *testing.T) {
	user := "hi"
	thinkingConfig := func(effort string) *model.GeminiThinkingConfig {
		converted := convertLLMToGeminiRequest(&model.InternalLLMRequest{
			Model:           "gemini-test",
			Messages:        []model.Message{{Role: "user", Content: model.MessageContent{Content: &user}}},
			ReasoningEffort: effort,
		})
		if converted.GenerationConfig == nil {
			return nil
		}
		return converted.GenerationConfig.ThinkingConfig
	}

	t.Run("high_sets_level", func(t *testing.T) {
		tc := thinkingConfig("high")
		if tc == nil || tc.ThinkingLevel != "high" {
			t.Fatalf("expected thinkingLevel high, got %#v", tc)
		}
	})
	t.Run("xhigh_omits_level", func(t *testing.T) {
		tc := thinkingConfig("xhigh")
		if tc == nil {
			t.Fatalf("expected a thinkingConfig for effort xhigh")
		}
		if tc.ThinkingLevel != "" {
			t.Fatalf("expected empty thinkingLevel for xhigh, got %q", tc.ThinkingLevel)
		}
	})
}

// FIX D: an assistant turn carrying only reasoning text re-emits it as a leading
// thought:true part so a thought-only turn is not dropped on replay.
func TestGeminiOutboundReemitsThoughtOnlyAssistant(t *testing.T) {
	reasoning := "let me think"
	converted := convertLLMToGeminiRequest(&model.InternalLLMRequest{
		Model: "gemini-test",
		Messages: []model.Message{{
			Role:             "assistant",
			ReasoningContent: &reasoning,
		}},
	})
	var modelContent *model.GeminiContent
	for _, c := range converted.Contents {
		if c.Role == "model" {
			modelContent = c
			break
		}
	}
	if modelContent == nil || len(modelContent.Parts) == 0 {
		t.Fatalf("expected a model content with a thought part, got %#v", converted.Contents)
	}
	first := modelContent.Parts[0]
	if !first.Thought || first.Text != reasoning {
		t.Fatalf("expected a leading thought:true part carrying the reasoning text, got %#v", first)
	}
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
