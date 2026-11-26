package relay

import (
	"context"

	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/prompt"
	"github.com/zhizhishu/octopus/internal/utils/log"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// promptOverrideSnapshot 是 metrics 消费的注入结果投影：动了哪些位置、哪些规则贡献
// 了文本（按应用顺序）。共享实现是 prompt.Snapshot。
type promptOverrideSnapshot struct {
	Mode    dbmodel.PromptOverrideMode
	Sources []string
}

// promptRuleListFn 是规则表读取缝：生产指向 op.PromptRuleList，测试可替换。
var promptRuleListFn = op.PromptRuleList

// loadPromptRulesOrNone 是一次请求的规则表读取（每请求只读一次，读到的这份就是本请求
// 的快照，串行候选与竞速候选共用）。读取失败只记日志并返回 nil —— nil 表示「本请求没有
// 规则」，转发照常进行：
//
//   - 绝不把读取失败升级成客户端错误（不给客户端 500、不中止请求）；
//   - 绝不回落到旧四层字段（旧算法只活在 prompt_override_test.go 的 oracle 里）。
//
// 这与旧 globalPromptOverride 的语义一致：读不到设置就是「什么都不加」，请求原样转发。
func loadPromptRulesOrNone(ctx context.Context, where string) []dbmodel.PromptRule {
	rules, err := promptRuleListFn(ctx)
	if err != nil {
		log.Errorf("relay: load prompt rules (%s): %v — continuing without prompt rules", where, err)
		return nil
	}
	return rules
}

// applyPromptRulesForAudience 在候选渠道确定后调用（base.restore 之后）：
// 按（tier, sort, id）正序评估并注入。rules 为空（含读取失败）时什么都不注入，
// 不回落旧四层字段。
func applyPromptRulesForAudience(req *transformerModel.InternalLLMRequest, rules []dbmodel.PromptRule, scope prompt.Scope) promptOverrideSnapshot {
	if req == nil {
		return promptOverrideSnapshot{}
	}
	snap := prompt.Apply(req, prompt.Evaluate(rules, scope))
	return promptOverrideSnapshot{Mode: snap.Mode, Sources: snap.Sources}
}

// promptScopePlanID 取方案 ID（nil → 0 表示无方案）。
func promptScopePlanID(plan *dbmodel.AccessPlan) int {
	if plan == nil {
		return 0
	}
	return plan.ID
}

// clearResponsesRawPromptShape 原位保留（relay.go / race_group.go 调用点语义不变）。
func clearResponsesRawPromptShape(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	req.ResponsesInstructions = nil
	req.ResponsesInputRaw = nil
}
