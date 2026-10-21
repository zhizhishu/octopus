package authropic

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// budget_tokens 有两条官方约束，缺一不可：
//  1. 必须小于 max_tokens（思考 token 计入 max_tokens 总额）
//  2. 最小 1024，低于它就 400
//
// 只做第 1 条会漏掉一种必然失败的情况：max_tokens 本身就小到装不下 1024。
// 那时「压到 max_tokens-1」压出来的 1023/99/0 全是非法值。
func TestThinkingBudgetNeverBelowOfficialFloor(t *testing.T) {
	cases := []struct {
		name       string
		maxTokens  int64
		wantBudget int64 // 0 表示"不该发 thinking"
	}{
		{"默认 8192，压到 8191", 8192, 8191},
		{"充足，原样", 200000, 64000},
		{"2000 压到 1999，仍合法", 2000, 1999},
		{"1025 压到 1024，正好合法", 1025, 1024},
		{"1024 会压成 1023，非法", 1024, 0},
		{"100 会压成 99，非法", 100, 0},
		// maxTokens<=0 在这段代码里有专门语义＝未知/不封顶，保持原样不压低。
		// 生产路径上不会出现：resolveMaxTokens 最低也会给到 1。
		{"0 表示不封顶，不动", 0, 64000},
		{"1 会压成 0，非法", 1, 0},
	}
	for _, c := range cases {
		got := getThinkingBudget("max", nil, c.maxTokens)
		switch {
		case c.wantBudget == 0:
			if got != nil {
				t.Errorf("%s: maxTokens=%d 应当不发 thinking，实际给了 budget=%d", c.name, c.maxTokens, *got)
			}
		case got == nil:
			t.Errorf("%s: maxTokens=%d 应当给 budget=%d，实际没发", c.name, c.maxTokens, c.wantBudget)
		case *got != c.wantBudget:
			t.Errorf("%s: maxTokens=%d 得到 budget=%d，应为 %d", c.name, c.maxTokens, *got, c.wantBudget)
		default:
			// 只要发出去了，就必须 ≥1024，否则上游 400。
			if *got < anthropicThinkingBudgetMin {
				t.Errorf("%s: 发出的 budget=%d 低于官方下限 %d，必然 400", c.name, *got, anthropicThinkingBudgetMin)
			}
		}
	}
}

// 装不下合法预算时，整个 thinking 都不能出现。
//
// 只把 budget_tokens 置空会发出 {"type":"enabled"}——「开了思考但没给预算」
// 同样是非法请求，等于把一个 400 换成另一个 400。
func TestThinkingConfigOmittedWhenBudgetTooSmall(t *testing.T) {
	user := "hi"
	maxTok := int64(100)
	req := &model.InternalLLMRequest{
		Model:           "claude-test",
		MaxTokens:       &maxTok,
		ReasoningEffort: "max",
		Messages:        []model.Message{{Role: "user", Content: model.MessageContent{Content: &user}}},
	}
	converted := convertToAnthropicRequest(req)
	if converted.Thinking != nil {
		t.Fatalf("max_tokens=100 装不下 1024 的思考，不该发 thinking，实际 %#v", converted.Thinking)
	}

	// 充足时应照常发，别把功能整个关掉。
	big := int64(200000)
	req2 := &model.InternalLLMRequest{
		Model:           "claude-test",
		MaxTokens:       &big,
		ReasoningEffort: "max",
		Messages:        []model.Message{{Role: "user", Content: model.MessageContent{Content: &user}}},
	}
	converted2 := convertToAnthropicRequest(req2)
	if converted2.Thinking == nil || converted2.Thinking.BudgetTokens == nil {
		t.Fatalf("max_tokens 充足时应当发 thinking，实际 %#v", converted2.Thinking)
	}
	if got := *converted2.Thinking.BudgetTokens; got != 64000 {
		t.Fatalf("max 档预算应为 64000，实际 %d", got)
	}
}
