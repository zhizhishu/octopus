package authropic

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 「关掉思考」必须真的关掉，不能变成「顶格开」。
//
// 曾经这里只看 req.ReasoningEffort != ""，于是 none/off 一路走到
// getThinkingBudget：在 EffortBudget 表里查不到 → 落 EffortBudgetDefault(32768)
// → 被 max_tokens 夹到 maxTokens-1。客户端明确说"别思考"，发出去的是
// {"type":"enabled","budget_tokens":<顶格>}，正好反了。
//
// 修法照参考项目：关闭是独立模式，走专门分支（thinking.type=disabled，并清掉
// output_config.effort），不进预算换算。
func TestThinkingOffSendsDisabledNotEnabled(t *testing.T) {
	maxTok := int64(8192)
	off := func(effort string) string {
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
			t.Fatalf("effort %q: thinking 整个没发（关闭应当显式发 disabled，不是省略）", effort)
		}
		raw, err := json.Marshal(out.Thinking)
		if err != nil {
			t.Fatalf("effort %q: marshal thinking: %v", effort, err)
		}
		return string(raw)
	}

	for _, effort := range []string{"none", "NONE", "None", "off", "OFF", " off "} {
		if got, want := off(effort), `{"type":"disabled"}`; got != want {
			t.Errorf("effort %q: thinking = %s, want %s（客户端说别思考，就不能发 enabled+预算）", effort, got, want)
		}
	}
}

// off 必须清掉 output_config.effort：参考项目的 disableThinking() 也删它。
// 留着 effort 会让上游看到"关掉思考 + 指定强度"这种自相矛盾的组合。
func TestThinkingOffClearsAdaptiveEffort(t *testing.T) {
	maxTok := int64(8192)
	out := convertToAnthropicRequest(&model.InternalLLMRequest{
		Model:            "claude-sonnet-4-5",
		ReasoningEffort:  "none",
		AdaptiveThinking: true, // 管理员开了 adaptive，也不能覆盖客户端的"关"
		MaxTokens:        &maxTok,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
		},
	})
	if out.Thinking == nil || out.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v, want type=disabled（客户端的关必须优先于 adaptive 默认）", out.Thinking)
	}
	if out.OutputConfig != nil && out.OutputConfig.Effort != "" {
		t.Errorf("output_config.effort = %q, want empty（关闭时不该再指定强度）", out.OutputConfig.Effort)
	}
}

// 反过来也要守住：minimal 是「尽量少」不是「关」。
// 参考项目把 minimal 与 none 分开处理；把 minimal 也关掉会让用户以为
// "最少思考"等于"不思考"，而它其实应该拿到最小合法预算。
func TestMinimalIsNotTreatedAsOff(t *testing.T) {
	maxTok := int64(8192)
	out := convertToAnthropicRequest(&model.InternalLLMRequest{
		Model:           "claude-sonnet-4-5",
		ReasoningEffort: "minimal",
		MaxTokens:       &maxTok,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
		},
	})
	if out.Thinking == nil || out.Thinking.Type == "disabled" {
		t.Fatalf("minimal 被当成关闭了：thinking = %+v（minimal 是尽量少，不是不思考）", out.Thinking)
	}
}

func strPtr(s string) *string { return &s }
