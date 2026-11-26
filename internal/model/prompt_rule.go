package model

import (
	"time"
)

// PromptRulePosition 表示一条提示词规则加在客户端系统提示词的前面还是后面。
// 与旧的四层 prompt_override_mode（含 append_system/replace_system 历史值）不同，
// 规则表只有 before/after 两种位置，历史值留在旧字段里，不进新表。
type PromptRulePosition string

const (
	PromptRulePositionBefore PromptRulePosition = "before"
	PromptRulePositionAfter  PromptRulePosition = "after"
)

// PromptRulePlanMode / PromptRuleChannelMode / PromptRuleModelMode 是规则三条作用域的
// 取值。all = 作用域内全部；selected = 只按对应 ID/名称列表匹配（渠道还有 exclude）。
const (
	PromptRulePlanModeAll      = "all"
	PromptRulePlanModeSelected = "selected"

	PromptRuleChannelModeAll     = "all"
	PromptRuleChannelModeInclude = "include"
	PromptRuleChannelModeExclude = "exclude"

	PromptRuleModelModeAll      = "all"
	PromptRuleModelModeSelected = "selected"
)

// ValidPromptRulePosition 只认 before/after；空值无效（写入必须显式选边）。
func ValidPromptRulePosition(p PromptRulePosition) bool {
	return p == PromptRulePositionBefore || p == PromptRulePositionAfter
}

func ValidPromptRulePlanMode(m string) bool {
	return m == PromptRulePlanModeAll || m == PromptRulePlanModeSelected
}

func ValidPromptRuleChannelMode(m string) bool {
	return m == PromptRuleChannelModeAll || m == PromptRuleChannelModeInclude || m == PromptRuleChannelModeExclude
}

func ValidPromptRuleModelMode(m string) bool {
	return m == PromptRuleModelModeAll || m == PromptRuleModelModeSelected
}

// PromptRule 是提示词页的一条规则：一段文本 + 一条位置 + 三条作用域
// （方案 / 渠道 / 模型）+ 一个受众开关。命中判断与拼接全部在 internal/prompt，
// 这张表只存配置。旧的四层字段（方案/路由/渠道上的 system_prompt_override 三元组
// 和全局设置）原样保留，只读不写，由 migration 031 迁到这张表。
type PromptRule struct {
	ID       int                `json:"id" gorm:"primaryKey"`
	Name     string             `json:"name" gorm:"size:191;not null"`
	Enabled  bool               `json:"enabled" gorm:"not null;default:false"`
	Text     string             `json:"text" gorm:"size:262144"`
	Position PromptRulePosition `json:"position" gorm:"size:16"`

	// 方案作用域：all = 所有 key（自动包含无方案 key）；selected = 只看
	// plan_ids，PlanID==0（无方案 key）另由 include_no_plan 决定。
	PlanMode      string `json:"plan_mode" gorm:"size:16;not null;default:'all'"`
	PlanIDs       []int  `json:"plan_ids" gorm:"serializer:json;type:text"`
	IncludeNoPlan bool   `json:"include_no_plan"`

	// 渠道作用域：all / include(列表内) / exclude(列表外)。
	ChannelMode string   `json:"channel_mode" gorm:"size:16;not null;default:'all'"`
	ChannelIDs  []int    `json:"channel_ids" gorm:"serializer:json;type:text"`
	ModelMode   string   `json:"model_mode" gorm:"size:16;not null;default:'all'"`
	Models      []string `json:"models" gorm:"serializer:json;type:text"`

	// Audience 复用旧四层的取值与判定（all / admin_only），见 PromptAudienceAllows。
	Audience PromptAudience `json:"audience" gorm:"size:16;not null;default:'all'"`

	Sort int `json:"sort"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PositionValid 只认 before/after；空值无效（写入必须显式选边）。
func (r *PromptRule) PositionValid() bool { return ValidPromptRulePosition(r.Position) }

// PromptRuleTier 是规则的具体度层级：数字越大越具体，注入时排在越外侧。
// 取规则三条作用域里最高受限的那条维度。
const (
	PromptRuleTierGlobal  = 0
	PromptRuleTierPlan    = 1
	PromptRuleTierModel   = 2
	PromptRuleTierChannel = 3
)

// Tier 返回该规则的具体度层级：三条作用域里最高受限的那条维度决定。
func (r *PromptRule) Tier() int {
	switch {
	case r.ChannelMode != "" && r.ChannelMode != PromptRuleChannelModeAll:
		return PromptRuleTierChannel
	case r.ModelMode == PromptRuleModelModeSelected:
		return PromptRuleTierModel
	case r.PlanMode == PromptRulePlanModeSelected:
		return PromptRuleTierPlan
	default:
		return PromptRuleTierGlobal
	}
}
