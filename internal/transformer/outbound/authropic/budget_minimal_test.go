package authropic

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// minimal 必须拿到**最小合法预算**，而不是顶格默认值。
//
// 曾经 minimal 在 EffortBudget 表里查不到，于是落进 EffortBudgetFor 的未知分支
// 拿到 EffortBudgetDefault(32768)：客户端要「尽可能少的思考」，出站发的是
// {"type":"enabled","budget_tokens":32768}，与 high 一模一样，按顶格计费。
//
// minimal ≠ 关闭（关闭是 none/off，走 disabled 分支），所以它既不能 disabled，
// 也不能吃到默认的 32768 —— 只能是 Anthropic 允许的最小预算 1024。
func TestMinimalEffortGetsSmallestLegalBudget(t *testing.T) {
	const maxTokens = int64(200000) // 足够大，避免被 max_tokens 夹紧后看不出差异

	for _, in := range []string{"minimal", "MINIMAL", " Minimal ", " minimal ", "\tMINIMAL\n"} {
		got := getThinkingBudget(in, nil, maxTokens)
		if got == nil {
			t.Fatalf("effort %q: 预算为 nil（整段 thinking 没发），minimal 应当开着思考", in)
		}
		if *got != anthropicThinkingBudgetMin {
			t.Errorf("effort %q: budget = %d, want %d（最小档拿到了 %d = 顶格默认值？）",
				in, *got, anthropicThinkingBudgetMin, model.EffortBudgetDefault)
		}
		if *got == model.EffortBudgetDefault {
			t.Errorf("effort %q: budget = %d 与 EffortBudgetDefault 相同，minimal 被当成未知档位顶格处理", in, *got)
		}
	}
}

// 出站报文层面锁死：minimal 发出的是 enabled + 1024，不是 32768。
func TestMinimalEffortOnTheWire(t *testing.T) {
	maxTok := int64(8192)
	for _, in := range []string{"minimal", "MINIMAL", " Minimal "} {
		out := convertToAnthropicRequest(&model.InternalLLMRequest{
			Model:           "claude-sonnet-4-5",
			ReasoningEffort: in,
			MaxTokens:       &maxTok,
			Messages: []model.Message{
				{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
			},
		})
		if out.Thinking == nil {
			t.Fatalf("effort %q: thinking 整个没发", in)
		}
		raw, err := json.Marshal(out.Thinking)
		if err != nil {
			t.Fatalf("effort %q: marshal thinking: %v", in, err)
		}
		want := `{"type":"enabled","budget_tokens":1024}`
		if string(raw) != want {
			t.Errorf("effort %q: thinking = %s, want %s", in, string(raw), want)
		}
	}
}

// none/off 仍然是 disabled（已修好的行为不能被 minimal 这条改动带回去），
// 且 minimal 不能被误判成关闭。
func TestMinimalIsNotOffAndOffStaysDisabled(t *testing.T) {
	maxTok := int64(8192)
	wire := func(effort string) string {
		t.Helper()
		out := convertToAnthropicRequest(&model.InternalLLMRequest{
			Model:           "claude-sonnet-4-5",
			ReasoningEffort: effort,
			MaxTokens:       &maxTok,
			Messages: []model.Message{
				{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
			},
		})
		if out.Thinking == nil {
			return "<nil>"
		}
		raw, err := json.Marshal(out.Thinking)
		if err != nil {
			t.Fatalf("effort %q: marshal thinking: %v", effort, err)
		}
		return string(raw)
	}

	for _, in := range []string{"none", "off", " NONE ", "OFF", " Off "} {
		if got, want := wire(in), `{"type":"disabled"}`; got != want {
			t.Errorf("effort %q: thinking = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"minimal", "MINIMAL", " Minimal "} {
		if got := wire(in); got == `{"type":"disabled"}` {
			t.Errorf("effort %q 被当成关闭了：thinking = %s", in, got)
		}
		if model.IsEffortOff(in) {
			t.Errorf("effort %q 不应被判为关闭意图", in)
		}
	}
}

// low 的行为不变：仍是 1024，不能被 minimal 的改动挪动。
func TestLowEffortBudgetUnchanged(t *testing.T) {
	const maxTokens = int64(200000)
	got := getThinkingBudget("low", nil, maxTokens)
	if got == nil || *got != anthropicThinkingBudgetMin {
		t.Fatalf("low budget = %v, want %d", got, anthropicThinkingBudgetMin)
	}
	if got := getThinkingBudget("low", nil, maxTokens); got != nil && *got != model.EffortBudget["low"] {
		t.Errorf("low 与共享表不一致：%d vs %d", *got, model.EffortBudget["low"])
	}
}
