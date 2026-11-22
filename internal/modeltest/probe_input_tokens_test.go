package modeltest

import (
	"encoding/json"
	"testing"

	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

func TestProbeBilledInputTokensFromCacheAliases(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"prompt_tokens":5033,"completion_tokens":5,"cache_read_input_tokens":4864}`, 5033},
		{`{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}`, 45},
	} {
		var usage transformermodel.Usage
		if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
			t.Fatal(err)
		}
		if got := probeBilledInputTokens(&usage); got != tc.want {
			t.Errorf("got %d, want %d for %s", got, tc.want, tc.body)
		}
	}
}

// probeBilledInputTokens 对应参考实现 lm-detector shared/tokenizer-usage.ts 的 inputTokens：
// Messages 口径是 input + cache_creation + cache_read（input_tokens 不含缓存）；
// OpenAI 口径的 prompt_tokens 已含缓存读，不能再加一遍。
func TestProbeBilledInputTokens(t *testing.T) {
	cases := []struct {
		name string
		u    *transformermodel.Usage
		want int
	}{
		{"nil", nil, 0},
		{"openai includes cached", &transformermodel.Usage{PromptTokens: 100,
			PromptTokensDetails: &transformermodel.PromptTokensDetails{CachedTokens: 40}}, 100},
		{"anthropic adds cache read and write", &transformermodel.Usage{PromptTokens: 10, AnthropicUsage: true,
			CacheCreationInputTokens: 5, PromptTokensDetails: &transformermodel.PromptTokensDetails{CachedTokens: 30}}, 45},
		{"separate-cache proxy", &transformermodel.Usage{PromptTokens: 10, SeparateCacheInputTokens: true,
			CacheCreationInputTokens: 2}, 12},
		{"anthropic no cache", &transformermodel.Usage{PromptTokens: 17, AnthropicUsage: true}, 17},
	}
	for _, tc := range cases {
		if got := probeBilledInputTokens(tc.u); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
