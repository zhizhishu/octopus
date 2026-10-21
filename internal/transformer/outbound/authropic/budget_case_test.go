package authropic

import "testing"

// 客户端发 "MAX" / " max " 这类大小写与空白变体时，必须与 "max" 完全等价。
//
// 这里曾经踩过：getThinkingBudget 用原始字符串 switch 小写常量，大写落进
// default 分支拿到 EffortBudgetDefault(32768)，而 max 应是 64000——
// 不报错、不告警，只是悄悄给了一半的思考预算。
//
// " max " 一并覆盖：那是 TrimSpace 漏掉时同样会静默降级的形态。
func TestThinkingBudgetIsCaseAndSpaceInsensitive(t *testing.T) {
	const maxTokens = int64(200000) // 足够大，避免被 max_tokens 夹紧后看不出差异

	want := *getThinkingBudget("max", nil, maxTokens)
	if want <= 0 {
		t.Fatalf("max 应当给出正预算，实际 %d", want)
	}

	for _, in := range []string{"MAX", "Max", " max ", "MAX ", "\tmax\n"} {
		got := *getThinkingBudget(in, nil, maxTokens)
		if got != want {
			t.Errorf("effort %q 得到 budget %d，应等同于 max 的 %d（落到了默认分支）", in, got, want)
		}
	}

	// 未知档位仍要回落到默认值，别为了修大小写把兜底也改掉。
	if got := *getThinkingBudget("ultra", nil, maxTokens); got != want && got == 0 {
		t.Errorf("未知档位不应给 0，应回落默认值，实际 %d", got)
	}
}
