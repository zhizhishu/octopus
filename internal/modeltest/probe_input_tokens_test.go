package modeltest

import (
	"testing"

	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

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
