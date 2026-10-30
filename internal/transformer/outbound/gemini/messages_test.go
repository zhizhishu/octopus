package gemini

import "testing"

// 别名必须归并，否则整个 thinkingConfig 被丢掉。
//
// 背景：本函数曾经只用 TrimSpace+ToLower 归一，然后直接查 model.EffortBudget。
// 客户端写 "x-high"/"extra-high"/"very-high"（别名表里的写法）时查不到，
// 于是返回 (0,false)——调用点据此**整个不发 thinkingConfig**。用户以为"比 high
// 更强"，实际拿到的可能是模型默认行为甚至无思考。GLM 早已走共享判据。
func TestReasoningToThinkingBudgetMergesAliases(t *testing.T) {
	cases := map[string]int32{
		"x-high":     geminiBudgetTopPro,
		"extra-high": geminiBudgetTopPro,
		"very-high":  geminiBudgetTopPro,
		"xhigh":      geminiBudgetTopPro,
		"extra_high": geminiBudgetTopPro,
	}
	for input, want := range cases {
		got, ok := reasoningToThinkingBudget(input, "gemini-2.5-pro")
		if !ok {
			t.Errorf("reasoningToThinkingBudget(%q): ok=false（effort 别名没被归并，thinkingConfig 会被整个丢掉）", input)
			continue
		}
		if got != want {
			t.Errorf("reasoningToThinkingBudget(%q) = %d, want %d", input, got, want)
		}
	}
}

// 上限必须分型号。
//
// 依据参考项目 registry/models/models.json：
//   gemini-2.5-pro   thinking.max = 32768
//   gemini-2.5-flash thinking.max = 24576
//
// 本仓曾经只有一个 32768 通吃，于是 xhigh/max 在 flash 上会发 32768 ——
// 超出上游允许上限。发小了只是思考少一点，发大了是 400 直接失败。
func TestReasoningToThinkingBudgetRespectsPerModelCeiling(t *testing.T) {
	cases := []struct {
		model string
		want  int32
	}{
		{"gemini-2.5-pro", geminiBudgetTopPro},
		{"gemini-2.5-flash", geminiBudgetTopFlash},
		{"gemini-2.5-flash-lite", geminiBudgetTopFlash},
		// 未知型号保守取小的：宁可思考少，不要 400
		{"gemini-future-model", geminiBudgetTopFlash},
	}
	for _, tc := range cases {
		for _, effort := range []string{"xhigh", "max"} {
			got, ok := reasoningToThinkingBudget(effort, tc.model)
			if !ok {
				t.Errorf("%s/%s: ok=false, want a budget", tc.model, effort)
				continue
			}
			if got > tc.want {
				t.Errorf("%s/%s: budget %d 超过该型号上限 %d（上游会 400）", tc.model, effort, got, tc.want)
			}
			if got != tc.want {
				t.Errorf("%s/%s: budget = %d, want %d", tc.model, effort, got, tc.want)
			}
		}
	}
}
