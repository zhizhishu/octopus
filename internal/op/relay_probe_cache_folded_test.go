package op

import (
	"testing"
)

// 真实 Anthropic 报文的缓存探针判定。
//
// 线上真实 usage（prompt cache 命中）：
//
//	{"input_tokens":12,"cache_read_input_tokens":40000,"cache_creation_input_tokens":3000}
//
// Anthropic 是**独占口径**：input_tokens 只算非缓存部分，缓存两栏另计，
// 真实输入总量是 43012。落库时 relay_logs.input_tokens 存的是原值 12，
// 折叠后的 43012 存在 cache_input_tokens 里。
func TestProbeCacheRealAnthropicPayload(t *testing.T) {
	cases := []struct {
		name        string
		in          RelayProbeInput
		wantFinding bool
		why         string
	}{
		{
			name:        "真实 Anthropic 缓存请求（12 + 40000 + 3000）",
			in:          RelayProbeInput{InputTokens: 12, CacheHitTokens: 40000, CacheWriteTokens: 3000, SeparateCacheInput: true, CacheFoldedInputTokens: 43012},
			wantFinding: false,
			why:         "43000 < 43012，计数正常，不该报警",
		},
		{
			name:        "同一报文但没传折叠值（退化到自行折叠）",
			in:          RelayProbeInput{InputTokens: 12, CacheHitTokens: 40000, CacheWriteTokens: 3000, SeparateCacheInput: true},
			wantFinding: false,
			why:         "SeparateCacheInput 时自己折成 43012，判定不变",
		},
		{
			name:        "OpenAI 口径（缓存已含在 prompt_tokens 内）",
			in:          RelayProbeInput{InputTokens: 43012, CacheHitTokens: 40000, CacheWriteTokens: 3000, CacheFoldedInputTokens: 43012},
			wantFinding: false,
			why:         "缓存是输入的一部分，43000 < 43012 正常",
		},
		{
			name:        "真的超了：缓存比折叠总量还多",
			in:          RelayProbeInput{InputTokens: 100, CacheHitTokens: 40000, CacheWriteTokens: 3000, SeparateCacheInput: true, CacheFoldedInputTokens: 43100},
			wantFinding: false,
			why:         "折叠后 43100 = 100+43000，缓存不超过总量，正常",
		},
		{
			name:        "口径错：缓存总量硬超过输入",
			in:          RelayProbeInput{InputTokens: 100, CacheHitTokens: 40000, CacheWriteTokens: 3000, CacheFoldedInputTokens: 100},
			wantFinding: true,
			why:         "分母被人为压到 100，43000 > 100，这才是真异常",
		},
	}
	for _, c := range cases {
		got := len(probeCacheExceedsPrompt(c.in)) > 0
		if got != c.wantFinding {
			t.Errorf("%s: finding=%v want=%v（%s）", c.name, got, c.wantFinding, c.why)
		}
	}
}
