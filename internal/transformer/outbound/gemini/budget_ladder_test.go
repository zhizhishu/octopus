package gemini

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// Gemini 的量级与共享表不同，合并时最容易犯的错是"直接把共享表的数字搬过来"。
// 那样 high(32768) 会正好等于 Gemini 上限，high/xhigh/max 全被压平成一个值——
// 表面上看"统一到单一真源了"，实际把档位阶梯弄塌了。
//
// 这条测试钉住分工：档位**存在性与强弱**来自共享表，**具体 token 数**留在 Gemini。
func TestGeminiBudgetLadderNotFlattenedBySharedTable(t *testing.T) {
	// 真实契约是 high < xhigh == max：xhigh 与 max 都表示"最强"，
	// 在 Gemini 上共享同一个上限是设计，不算压平。所以只要求
	// low < medium < high < xhigh 严格递增，max 与 xhigh 相等。
	ladder := []string{"low", "medium", "high", "xhigh"}
	prev := int32(-1)
	for _, effort := range ladder {
		got, ok := reasoningToThinkingBudget(effort, "gemini-2.5-pro")
		if !ok {
			t.Fatalf("effort %q 应当有预算", effort)
		}
		if got <= prev {
			t.Fatalf("阶梯必须在 %q 处严格递增，实际 %d <= 前一级 %d（档位被压平）", effort, got, prev)
		}
		prev = got
	}

	xhigh, _ := reasoningToThinkingBudget("xhigh", "gemini-2.5-pro")
	max, _ := reasoningToThinkingBudget("max", "gemini-2.5-pro")
	if xhigh != max {
		t.Fatalf("xhigh/max 应共用 Gemini 上限: %d vs %d", xhigh, max)
	}

	// Gemini 侧不得超过自己的上限，否则上游会拒。
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		if got, _ := reasoningToThinkingBudget(effort, "gemini-2.5-pro"); got > 32768 {
			t.Fatalf("effort %q 的预算 %d 超过 Gemini 上限 32768", effort, got)
		}
	}
}

// 档位是否已知必须问共享表：共享表新增档位后，Gemini 侧应自动认得它，
// 而不是继续按"未知"处理、静默不发 thinkingConfig。
func TestGeminiKnowsEverySharedTableTier(t *testing.T) {
	for effort := range model.EffortBudget {
		if effort == "minimal" || effort == "none" {
			continue // 这两个是"关掉思考"，本就不发配置
		}
		if _, ok := reasoningToThinkingBudget(effort, "gemini-2.5-pro"); !ok {
			t.Fatalf("共享表里有档位 %q，Gemini 却当成未知、不发 thinkingConfig", effort)
		}
	}
}
