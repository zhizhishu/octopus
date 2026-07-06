package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestApplyDeepSeekParamCompat verifies that DeepSeek chat outbound strips the
// OpenAI Responses / newer-Chat residue params it rejects (prompt_cache_key /
// prompt_cache_retention / safety_identifier / store), keeps a valid tool_choice,
// drops a malformed one, and leaves non-DeepSeek models untouched.
func TestApplyDeepSeekParamCompat(t *testing.T) {
	store := false
	pck := "cache-key-123"
	ret := "in_memory"
	sid := "safety-1"
	auto := "auto"

	// DeepSeek: residue params stripped, valid tool_choice="auto" kept.
	body := chatRequestBody(t, &model.InternalLLMRequest{
		Model:                "deepseek-v4-pro",
		Messages:             userMessages(),
		Store:                &store,
		PromptCacheKey:       &pck,
		PromptCacheRetention: &ret,
		SafetyIdentifier:     &sid,
		ToolChoice:           &model.ToolChoice{ToolChoice: &auto},
	})
	for _, k := range []string{"store", "prompt_cache_key", "prompt_cache_retention", "safety_identifier"} {
		if _, ok := body[k]; ok {
			t.Errorf("DeepSeek outbound must not carry residue field %q, body=%v", k, body)
		}
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("valid tool_choice=auto must be preserved, got %v", body["tool_choice"])
	}

	// DeepSeek: malformed tool_choice variants are dropped (would marshal to
	// null / non-function named / empty name and be rejected by the upstream).
	malformed := map[string]*model.ToolChoice{
		"empty":           {},
		"nonFunctionType": {NamedToolChoice: &model.NamedToolChoice{Type: "allowed_tools"}},
		"emptyName":       {NamedToolChoice: &model.NamedToolChoice{Type: "function"}},
	}
	for name, tc := range malformed {
		b := chatRequestBody(t, &model.InternalLLMRequest{
			Model: "deepseek-v4-pro", Messages: userMessages(), ToolChoice: tc,
		})
		if _, ok := b["tool_choice"]; ok {
			t.Errorf("malformed tool_choice(%s) must be dropped, body=%v", name, b)
		}
	}

	// Non-DeepSeek models keep the params (OpenAI-official upstreams support them).
	body2 := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "gpt-4o", Messages: userMessages(),
		PromptCacheKey: &pck, ToolChoice: &model.ToolChoice{ToolChoice: &auto},
	})
	if _, ok := body2["prompt_cache_key"]; !ok {
		t.Errorf("non-DeepSeek model must keep prompt_cache_key, body=%v", body2)
	}
}
