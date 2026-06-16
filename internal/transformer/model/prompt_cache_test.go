package model

import (
	"encoding/json"
	"testing"
)

func TestInternalLLMRequestPromptCacheFieldsAreStrings(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.1",
		"prompt_cache_key":"project-alpha",
		"prompt_cache_retention":"24h",
		"messages":[{"role":"user","content":"hello"}]
	}`)

	var req InternalLLMRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if req.PromptCacheKey == nil || *req.PromptCacheKey != "project-alpha" {
		t.Fatalf("prompt_cache_key was not preserved: %#v", req.PromptCacheKey)
	}
	if req.PromptCacheRetention == nil || *req.PromptCacheRetention != "24h" {
		t.Fatalf("prompt_cache_retention was not preserved: %#v", req.PromptCacheRetention)
	}

	encoded, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("encoded request is not valid JSON: %s", encoded)
	}
	if !containsJSONField(encoded, "prompt_cache_key", "project-alpha") {
		t.Fatalf("encoded request dropped prompt_cache_key: %s", encoded)
	}
	if !containsJSONField(encoded, "prompt_cache_retention", "24h") {
		t.Fatalf("encoded request dropped prompt_cache_retention: %s", encoded)
	}
}

func TestUsageUnmarshalOpenAIAliasesPreservesCacheTokens(t *testing.T) {
	body := []byte(`{
		"input_tokens":120,
		"output_tokens":30,
		"input_tokens_details":{"cached_tokens":72},
		"cache_creation_input_tokens":16
	}`)

	var usage Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if usage.PromptTokens != 120 || usage.CompletionTokens != 30 || usage.TotalTokens != 150 {
		t.Fatalf("unexpected token counts: prompt=%d completion=%d total=%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 72 {
		t.Fatalf("cached tokens were not preserved: %#v", usage.PromptTokensDetails)
	}
	if usage.CacheCreationInputTokens != 16 {
		t.Fatalf("cache creation tokens were not preserved: %d", usage.CacheCreationInputTokens)
	}
	if !usage.SeparateCacheInputTokens {
		t.Fatalf("cache creation alias should mark separate cache input tokens")
	}
}

func TestUsageUnmarshalAnthropicStyleAliasesMarkSeparateCacheTokens(t *testing.T) {
	body := []byte(`{
		"input_tokens":60,
		"output_tokens":20,
		"cache_read_input_tokens":30,
		"cache_creation_input_tokens":10
	}`)

	var usage Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if usage.PromptTokens != 60 || usage.CompletionTokens != 20 {
		t.Fatalf("unexpected token counts: prompt=%d completion=%d", usage.PromptTokens, usage.CompletionTokens)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 30 {
		t.Fatalf("cache read tokens were not preserved: %#v", usage.PromptTokensDetails)
	}
	if usage.CacheCreationInputTokens != 10 {
		t.Fatalf("cache creation tokens were not preserved: %d", usage.CacheCreationInputTokens)
	}
	if !usage.SeparateCacheInputTokens {
		t.Fatalf("anthropic-style cache aliases should mark separate cache input tokens")
	}
}

func TestUsageUnmarshalOpenAIChatDetailsPreservesCacheCreationTokens(t *testing.T) {
	body := []byte(`{
		"prompt_tokens":80,
		"completion_tokens":20,
		"total_tokens":100,
		"prompt_tokens_details":{"cached_tokens":25},
		"cache_creation_input_tokens":5
	}`)

	var usage Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if usage.PromptTokens != 80 || usage.CompletionTokens != 20 || usage.TotalTokens != 100 {
		t.Fatalf("unexpected token counts: prompt=%d completion=%d total=%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 25 {
		t.Fatalf("cached tokens were not preserved: %#v", usage.PromptTokensDetails)
	}
	if usage.CacheCreationInputTokens != 5 {
		t.Fatalf("cache creation tokens were not preserved: %d", usage.CacheCreationInputTokens)
	}
}

func TestUsageUnmarshalOpenAICompatibleCacheTokenAliases(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{
			name: "cached_tokens",
			body: `{"prompt_tokens":80,"completion_tokens":20,"cached_tokens":31}`,
			want: 31,
		},
		{
			name: "prompt_cache_hit_tokens",
			body: `{"prompt_tokens":80,"completion_tokens":20,"prompt_cache_hit_tokens":42}`,
			want: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var usage Usage
			if err := json.Unmarshal([]byte(tt.body), &usage); err != nil {
				t.Fatalf("unexpected unmarshal error: %v", err)
			}
			if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != tt.want {
				t.Fatalf("cached token alias was not preserved: %#v", usage.PromptTokensDetails)
			}
		})
	}
}

func containsJSONField(body []byte, key string, want string) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	got, ok := obj[key].(string)
	return ok && got == want
}
