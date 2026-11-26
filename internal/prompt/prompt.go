// Package prompt 是提示词规则的共享核心：relay 的真实注入与提示词页预览
// 调用同一份 Evaluate/Apply，保证预览与真实注入一致。本包不打上游请求、不读 DB；
// 启用规则列表由 op.PromptRuleList 给出。
package prompt

import (
	"sort"
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// Scope 是一次请求要匹配的作用域：key 命中的单个方案、原始请求模型、实际候选
// 渠道、调用者是否管理员（API key 主体）。PlanID==0 表示无方案 key。
type Scope struct {
	PlanID       int
	RequestModel string
	ChannelID    int
	IsAdmin      bool
}

// Match 是一条规则在当前 scope 下的命中结论。Evaluate 恒为每条规则给一行，
// Matched=false 时 Reason 说明原因；Reason 取值固定为
// matched / disabled / audience / plan_scope / channel_scope / model_scope。
type Match struct {
	Rule    dbmodel.PromptRule `json:"rule"`
	Matched bool               `json:"matched"`
	Reason  string             `json:"reason"`
}

// 未命中/命中的原因常量（Match.Reason 字段取值）。
const (
	ReasonMatched  = "matched"
	ReasonDisabled = "disabled"
	ReasonAudience = "audience"
	ReasonPlan     = "plan_scope"
	ReasonChannel  = "channel_scope"
	ReasonModel    = "model_scope"
)

// Snapshot 表示这次注入动了哪些位置：Mode 取 before/after/before+after
// （空 = 一条都没应用）；Sources 是应用了的规则名，按应用顺序。
type Snapshot struct {
	Mode    dbmodel.PromptOverrideMode
	Sources []string
}

// Evaluate 按（层级 tier、sort、id）正序返回每条规则的命中结论，含未命中原因；
// 预览与 relay 注入共用这一份结论，保证显示与真实注入一致。
func Evaluate(rules []dbmodel.PromptRule, scope Scope) []Match {
	type entry struct {
		m    Match
		tier int
	}
	entries := make([]entry, 0, len(rules))
	for _, r := range rules {
		m, tier := evalOne(r, scope)
		entries = append(entries, entry{m: m, tier: tier})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].tier != entries[j].tier {
			return entries[i].tier < entries[j].tier
		}
		if entries[i].m.Rule.Sort != entries[j].m.Rule.Sort {
			return entries[i].m.Rule.Sort < entries[j].m.Rule.Sort
		}
		return entries[i].m.Rule.ID < entries[j].m.Rule.ID
	})
	out := make([]Match, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.m)
	}
	return out
}

// evalOne 判定单条规则；层级取规则的最高受限维度（model.PromptRule.Tier）。
func evalOne(r dbmodel.PromptRule, scope Scope) (Match, int) {
	tier := r.Tier()
	switch {
	case !r.Enabled:
		return Match{Rule: r, Reason: ReasonDisabled}, tier
	case !dbmodel.PromptAudienceAllows(r.Audience, scope.IsAdmin):
		return Match{Rule: r, Reason: ReasonAudience}, tier
	case !rulePlanAllows(r, scope.PlanID):
		return Match{Rule: r, Reason: ReasonPlan}, tier
	case !ruleChannelAllows(r, scope.ChannelID):
		return Match{Rule: r, Reason: ReasonChannel}, tier
	case !ruleModelAllows(r, scope.RequestModel):
		return Match{Rule: r, Reason: ReasonModel}, tier
	}
	return Match{Rule: r, Matched: true, Reason: ReasonMatched}, tier
}

// planAllows：plan_mode=all 自动匹配所有 key（含无方案）；selected 只看 plan_ids，
// 无方案 key（PlanID==0）另由 include_no_plan 决定。空值按 all 兼容，未知取值不命中。
func rulePlanAllows(r dbmodel.PromptRule, planID int) bool {
	switch r.PlanMode {
	case "", dbmodel.PromptRulePlanModeAll:
		return true
	case dbmodel.PromptRulePlanModeSelected:
		if planID == 0 {
			return r.IncludeNoPlan
		}
		return intIn(r.PlanIDs, planID)
	default:
		return false
	}
}

// ruleChannelAllows：all / include(列表内) / exclude(列表外)；未知取值不命中。
func ruleChannelAllows(r dbmodel.PromptRule, channelID int) bool {
	switch r.ChannelMode {
	case "", dbmodel.PromptRuleChannelModeAll:
		return true
	case dbmodel.PromptRuleChannelModeInclude:
		return intIn(r.ChannelIDs, channelID)
	case dbmodel.PromptRuleChannelModeExclude:
		return !intIn(r.ChannelIDs, channelID)
	default:
		return false
	}
}

// ruleModelAllows：all / selected（精确名或尾 * 前缀，忽略大小写；并按旧路由约定
// 对请求模型先做 1M 别名清洗）。未知取值不命中。
func ruleModelAllows(r dbmodel.PromptRule, requestModel string) bool {
	if r.ModelMode == "" || r.ModelMode == dbmodel.PromptRuleModelModeAll {
		return true
	}
	if r.ModelMode != dbmodel.PromptRuleModelModeSelected {
		return false
	}
	request := strings.ToLower(dbmodel.CleanOneMillionCapabilityModelName(requestModel))
	for _, pattern := range r.Models {
		p := strings.ToLower(dbmodel.CleanOneMillionCapabilityModelName(strings.TrimSpace(pattern)))
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(request, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if request == p {
			return true
		}
	}
	return false
}

func intIn(list []int, id int) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// Apply 把 Evaluate 的命中结论逐条应用到请求上：（tier、sort、id）正序，before
// 前插、after 后插，所以越具体的规则越靠外（与旧四层算法最终顺序逐字节一致；
// 旧算法只在测试 oracle：internal/relay/prompt_override_test.go）。
//
// 一条规则被跳过的情形：Evaluate 未命中、文本 trim 后为空。客户端自己的系统
// 内容永不删除或重排；Claude CLI 的计费头/身份块由 ApplyText 内搬入的原有逻辑
// 保留在最前。快照 Mode 取 before/after/before+after（空 = 一条都没应用）。
func Apply(req *transformerModel.InternalLLMRequest, matches []Match) Snapshot {
	snap := Snapshot{}
	if req == nil || !req.IsChatRequest() {
		return snap
	}
	before, after := false, false
	for _, m := range matches {
		if !m.Matched {
			continue
		}
		text := strings.TrimSpace(m.Rule.Text)
		if text == "" {
			continue
		}
		mode := PositionToMode(m.Rule.Position)
		ApplyText(req, text, mode)
		snap.Sources = append(snap.Sources, m.Rule.Name)
		if mode == dbmodel.PromptOverrideModeAfter {
			after = true
		} else {
			before = true
		}
	}
	switch {
	case before && after:
		snap.Mode = "before+after"
	case after:
		snap.Mode = dbmodel.PromptOverrideModeAfter
	case before:
		snap.Mode = dbmodel.PromptOverrideModeBefore
	}
	return snap
}

// PositionToMode 把规则位置（PromptRulePosition）映射为旧拼接函数认的
// before/after 值（同 NormalizePromptOverrideMode 语义：非 after 一律 before）。
func PositionToMode(p dbmodel.PromptRulePosition) dbmodel.PromptOverrideMode {
	if p == dbmodel.PromptRulePositionAfter {
		return dbmodel.PromptOverrideModeAfter
	}
	return dbmodel.PromptOverrideModeBefore
}

// Segment 是预览结果里的一段来源文本；客户端自带系统的段固定
// RuleID=0、Name=client、Position=client。
type Segment struct {
	RuleID   int    `json:"rule_id"`
	Name     string `json:"name"`
	Position string `json:"position"`
	Text     string `json:"text"`
}

// PreviewResult 是预览输出：全部规则的命中结论、最终文本的来源段与最终文本。
// 纯文本展示（chat 语义的 "\n" 拼接），不代表具体协议的 wire 形状；真实协议
// 拼接（Anthropic 块、Responses instructions、trim）以 relay 实际注入与
// wire golden 测试为准，预览只给人看。
type PreviewResult struct {
	Matches  []Match   `json:"matches"`
	Segments []Segment `json:"segments"`
	Text     string    `json:"text"`
}

const (
	clientSegmentName     = "client"
	clientSegmentPosition = "client"
)

// Preview 用纯文本（chat 语义的 "\n" 拼接）模拟注入结果：所有规则的命中结论、
// 最终文本的来源段与最终文本。与 relay 共用 Evaluate；段顺序按最终文本阅读序：
// before 段与评估顺序相反（后应用的更具体规则排更前），客户端段固定在中，
// after 段与评估顺序一致。
func Preview(rules []dbmodel.PromptRule, scope Scope, clientSystem string) PreviewResult {
	matches := Evaluate(rules, scope)
	res := PreviewResult{Matches: matches}

	text := clientSystem
	var beforeSegs, afterSegs []Segment
	for _, m := range matches {
		if !m.Matched {
			continue
		}
		t := strings.TrimSpace(m.Rule.Text)
		if t == "" {
			continue
		}
		if m.Rule.Position == dbmodel.PromptRulePositionAfter {
			if text == "" {
				text = t
			} else {
				text = text + "\n" + t
			}
			afterSegs = append(afterSegs, Segment{RuleID: m.Rule.ID, Name: m.Rule.Name, Position: string(m.Rule.Position), Text: t})
			continue
		}
		if text == "" {
			text = t
		} else {
			text = t + "\n" + text
		}
		beforeSegs = append(beforeSegs, Segment{RuleID: m.Rule.ID, Name: m.Rule.Name, Position: string(m.Rule.Position), Text: t})
	}
	// 客户端段固定：RuleID=0、Name=client、Position=client；before 段与评估顺序
	// 相反（后应用的更具体规则排更前），after 段与评估顺序一致。
	for i := len(beforeSegs) - 1; i >= 0; i-- {
		res.Segments = append(res.Segments, beforeSegs[i])
	}
	res.Segments = append(res.Segments, Segment{RuleID: 0, Name: clientSegmentName, Position: clientSegmentPosition, Text: clientSystem})
	res.Segments = append(res.Segments, afterSegs...)
	res.Text = text
	return res
}
