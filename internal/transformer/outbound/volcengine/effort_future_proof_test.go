package volcengine

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 共享表新增档位时，这里必须自动跟上，而不是靠人记得回来改 case。
//
// 历史：上一版是一张手写 case 表配**空 default**，加上 MarshalJSON 的
// `if Thinking.Type != ""` 门控，等于「不认识的档位就把整个 thinking 丢掉」。
// 当时 xhigh/max 不在 case 里，最强的两档反而什么参数都没发。事后只把
// xhigh/max 补进 case、空 default 留着，下一个新档位就会重演。
func TestVolcengineFollowsSharedEffortTable(t *testing.T) {
	for effort := range model.EffortBudget {
		if effort == "minimal" || effort == "none" {
			continue
		}
		if got := effortToThinkingType(effort); got != ThinkingTypeEnabled {
			t.Errorf("共享表里的档位 %q 应当开启思考，实际 thinking.type=%q", effort, got)
		}
	}
}

// reasoning_effort 是客户端自由透传字段，大小写与空白变体是必然输入。
func TestVolcengineEffortIgnoresCaseAndSpace(t *testing.T) {
	for _, variant := range []string{"max", "MAX", "Max", " max ", "MAX ", "\tmax\n", "HiGh"} {
		if got := effortToThinkingType(variant); got != ThinkingTypeEnabled {
			t.Errorf("effort=%q 应当开启思考（与 max/high 等价），实际 %q", variant, got)
		}
	}
}

// 三种状态必须彼此区分：关闭 / 开启 / 没表达意图（不发字段）。
// 把"没意图"和"关闭"混为一谈会把普通请求强行写死 disabled。
func TestVolcengineOffVersusAbsent(t *testing.T) {
	cases := map[string]ThinkingType{
		"none":     ThinkingTypeDisabled,
		"minimal":  ThinkingTypeDisabled,
		" NONE ":   ThinkingTypeDisabled,
		"":         "",
		"ultra":    "",
		"adaptive": "",
	}
	for effort, want := range cases {
		if got := effortToThinkingType(effort); got != want {
			t.Errorf("effort=%q 期望 %q，实际 %q", effort, want, got)
		}
	}
}
