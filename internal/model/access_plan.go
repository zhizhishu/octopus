package model

import (
	"fmt"
	"strings"
)

type AccessBillingModelSource string

const (
	AccessBillingModelSourceRequest  AccessBillingModelSource = "request_model"
	AccessBillingModelSourceUpstream AccessBillingModelSource = "upstream_model"
	AccessBillingModelSourceOverride AccessBillingModelSource = "override_model"
)

type AccessRouteFallbackMode string

const (
	AccessRouteFallbackGroup       AccessRouteFallbackMode = "failover"
	AccessRouteFallbackReturnGroup AccessRouteFallbackMode = "return_group"
	AccessRouteFallbackNone        AccessRouteFallbackMode = "none"
)

type PromptOverrideMode string

// 系统提示词注入：每层（全局 / 方案 / 模型路由 / 渠道）一个开关 + 一个位置。
//
//	prompt_override_enabled = false → 这一层什么都不加，请求原样不动
//	prompt_override_mode    = before → 放在客户端系统提示词前面；after → 放在后面
//	                          客户端没带系统提示词时，两种位置都直接用这段。
//
// 拼接规则照 new-api（relay/chat_completions_via_responses.go applySystemPromptIfNeeded 等），
// 「后面」是它的镜像，见 internal/relay/prompt_override.go。
//
// 旧值：append_system（另加一条）/ replace_system（替换原有）都按「开 + 前面」处理；
// 旧数据没有开关列，migration 026 按「有文本 = 开」补上，并把模式改成 before。
const (
	PromptOverrideModeBefore PromptOverrideMode = "before"
	PromptOverrideModeAfter  PromptOverrideMode = "after"
	// Deprecated: legacy values, both mean enabled + before.
	PromptOverrideModeAppendSystem  PromptOverrideMode = "append_system"
	PromptOverrideModeReplaceSystem PromptOverrideMode = "replace_system"
)

// NormalizePromptOverrideMode maps every stored/legacy value onto before/after.
func NormalizePromptOverrideMode(mode PromptOverrideMode) PromptOverrideMode {
	if mode == PromptOverrideModeAfter {
		return PromptOverrideModeAfter
	}
	return PromptOverrideModeBefore
}

// IsLegacyPromptOverrideMode reports a value written before the on/off switch existed.
func IsLegacyPromptOverrideMode(mode PromptOverrideMode) bool {
	return mode == PromptOverrideModeAppendSystem || mode == PromptOverrideModeReplaceSystem
}

// ValidPromptOverrideMode reports whether mode is accepted on write.
func ValidPromptOverrideMode(mode PromptOverrideMode) bool {
	switch mode {
	case "", PromptOverrideModeBefore, PromptOverrideModeAfter, PromptOverrideModeAppendSystem, PromptOverrideModeReplaceSystem:
		return true
	}
	return false
}

// PromptOverrideActive reports whether a layer adds anything: switch on and non-blank text.
// A legacy mode value (row never migrated, old client) counts as on, the way it behaved.
func PromptOverrideActive(enabled bool, mode PromptOverrideMode, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	return enabled || IsLegacyPromptOverrideMode(mode)
}

// NormalizePromptOverride folds a written (enabled, mode, text) triple onto the stored form:
// a legacy mode becomes before and turns the switch on when there is text (an old client
// that never sends the switch keeps working); unknown modes are left for validation.
func NormalizePromptOverride(enabled *bool, mode *PromptOverrideMode, text string) {
	if mode == nil || !ValidPromptOverrideMode(*mode) {
		return
	}
	if IsLegacyPromptOverrideMode(*mode) && enabled != nil {
		*enabled = *enabled || strings.TrimSpace(text) != ""
	}
	*mode = NormalizePromptOverrideMode(*mode)
}

type AccessPlan struct {
	ID                   int                `json:"id" gorm:"primaryKey"`
	Slug                 string             `json:"slug" gorm:"unique;not null"`
	DisplayName          string             `json:"display_name" gorm:"not null"`
	Enabled              bool               `json:"enabled" gorm:"default:true"`
	IsDefault            bool               `json:"is_default" gorm:"default:false"`
	Sort                 int                `json:"sort"`
	RouteProfileID       int                `json:"route_profile_id" gorm:"index"`
	BillingProfileID     int                `json:"billing_profile_id" gorm:"index"`
	SystemPromptOverride string             `json:"system_prompt_override"`
	PromptOverrideMode   PromptOverrideMode `json:"prompt_override_mode" gorm:"default:append_system"`
	// PromptOverrideEnabled is the per-layer 启用 switch (false = this layer adds nothing).
	PromptOverrideEnabled bool `json:"prompt_override_enabled" gorm:"not null;default:false"`
	// AutoSyncChannels is retained for API compatibility. Reconciliation no longer
	// gates on this flag: every plan with a route profile is synced.
	AutoSyncChannels bool `json:"auto_sync_channels" gorm:"default:false"`

	RouteProfile   *AccessRouteProfile   `json:"route_profile,omitempty" gorm:"foreignKey:RouteProfileID"`
	BillingProfile *AccessBillingProfile `json:"billing_profile,omitempty" gorm:"foreignKey:BillingProfileID"`

	DefaultMultiplier float64                  `json:"default_multiplier" gorm:"-"`
	BillingRules      []AccessBillingModelRule `json:"billing_rules,omitempty" gorm:"-"`
	RouteTargets      []AccessRouteTarget      `json:"route_targets,omitempty" gorm:"-"`
}

type AccessRouteProfile struct {
	ID    int               `json:"id" gorm:"primaryKey"`
	Name  string            `json:"name" gorm:"unique;not null"`
	Rules []AccessRouteRule `json:"rules,omitempty" gorm:"foreignKey:RouteProfileID;constraint:OnDelete:CASCADE"`
}

type AccessRouteRule struct {
	ID                    int                      `json:"id" gorm:"primaryKey"`
	RouteProfileID        int                      `json:"route_profile_id" gorm:"not null;index:idx_access_route_rule,unique"`
	RequestModel          string                   `json:"request_model" gorm:"not null;index:idx_access_route_rule,unique"`
	Mode                  GroupMode                `json:"mode" gorm:"default:3"`
	BillingModelSource    AccessBillingModelSource `json:"billing_model_source" gorm:"default:request_model"`
	BillingModelOverride  string                   `json:"billing_model_override"`
	FallbackMode          AccessRouteFallbackMode  `json:"fallback_mode" gorm:"default:failover"`
	SystemPromptOverride  string                   `json:"system_prompt_override"`
	PromptOverrideMode    PromptOverrideMode       `json:"prompt_override_mode" gorm:"default:append_system"`
	PromptOverrideEnabled bool                     `json:"prompt_override_enabled" gorm:"not null;default:false"`
	// PriorityOverridden marks a rule whose channel priorities are hand-tuned and must
	// not be re-derived or re-sorted by the flat API. True for every pre-migration rule
	// (backfilled by db migration 013) so historical ordering is preserved.
	PriorityOverridden bool                `json:"priority_overridden" gorm:"default:false"`
	Targets            []AccessRouteTarget `json:"targets,omitempty" gorm:"foreignKey:RouteRuleID;constraint:OnDelete:CASCADE"`
}

type AccessRouteTarget struct {
	ID            int    `json:"id" gorm:"primaryKey"`
	RouteRuleID   int    `json:"route_rule_id" gorm:"not null;index:idx_access_route_target,unique"`
	ChannelID     int    `json:"channel_id" gorm:"not null;index:idx_access_route_target,unique"`
	UpstreamModel string `json:"upstream_model" gorm:"not null;index:idx_access_route_target,unique"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	Enabled       bool   `json:"enabled" gorm:"default:true"`

	AccessPlanID          int                      `json:"access_plan_id,omitempty" gorm:"-"`
	RequestModel          string                   `json:"request_model,omitempty" gorm:"-"`
	Mode                  GroupMode                `json:"mode,omitempty" gorm:"-"`
	BillingModelSource    AccessBillingModelSource `json:"billing_model_source,omitempty" gorm:"-"`
	BillingModelOverride  string                   `json:"billing_model_override,omitempty" gorm:"-"`
	FallbackMode          AccessRouteFallbackMode  `json:"fallback_mode,omitempty" gorm:"-"`
	SystemPromptOverride  string                   `json:"system_prompt_override,omitempty" gorm:"-"`
	PromptOverrideMode    PromptOverrideMode       `json:"prompt_override_mode,omitempty" gorm:"-"`
	PromptOverrideEnabled bool                     `json:"prompt_override_enabled,omitempty" gorm:"-"`
	// PriorityOverridden on AccessRouteTarget is a flat-API-only echo of
	// AccessRouteRule.PriorityOverridden (never persisted; see gorm:"-").
	PriorityOverridden bool `json:"priority_overridden,omitempty" gorm:"-"`
}

type AccessBillingProfile struct {
	ID                int                      `json:"id" gorm:"primaryKey"`
	Name              string                   `json:"name" gorm:"unique;not null"`
	DefaultMultiplier float64                  `json:"default_multiplier" gorm:"default:1"`
	ModelRules        []AccessBillingModelRule `json:"model_rules,omitempty" gorm:"foreignKey:BillingProfileID;constraint:OnDelete:CASCADE"`
}

type AccessBillingModelRule struct {
	ID               int     `json:"id" gorm:"primaryKey"`
	BillingProfileID int     `json:"billing_profile_id" gorm:"not null;index:idx_access_billing_model_rule,unique"`
	ModelName        string  `json:"model_name" gorm:"not null;index:idx_access_billing_model_rule,unique"`
	Multiplier       float64 `json:"multiplier" gorm:"default:1"`
	Enabled          bool    `json:"enabled" gorm:"default:true"`

	AccessPlanID int `json:"access_plan_id,omitempty" gorm:"-"`
}

type APIKeyAccessPlan struct {
	ID           int  `json:"id" gorm:"primaryKey"`
	APIKeyID     int  `json:"api_key_id" gorm:"not null;index:idx_api_key_access_plan,unique"`
	AccessPlanID int  `json:"access_plan_id" gorm:"not null;index:idx_api_key_access_plan,unique"`
	IsDefault    bool `json:"is_default" gorm:"default:false"`
}

type UserAccessPlan struct {
	ID           int  `json:"id" gorm:"primaryKey"`
	UserID       int  `json:"user_id" gorm:"not null;index:idx_user_access_plan,unique"`
	AccessPlanID int  `json:"access_plan_id" gorm:"not null;index:idx_user_access_plan,unique"`
	IsDefault    bool `json:"is_default" gorm:"default:false"`
}

type AccessPlanBillingSnapshot struct {
	AccessPlanID        int                      `json:"access_plan_id,omitempty"`
	AccessPlanSlug      string                   `json:"access_plan_slug,omitempty"`
	AccessPlanName      string                   `json:"access_plan_name,omitempty"`
	RouteProfileID      int                      `json:"route_profile_id,omitempty"`
	RouteProfileName    string                   `json:"route_profile_name,omitempty"`
	BillingProfileID    int                      `json:"billing_profile_id,omitempty"`
	BillingProfileName  string                   `json:"billing_profile_name,omitempty"`
	BillingModelName    string                   `json:"billing_model_name,omitempty"`
	BillingModelSource  AccessBillingModelSource `json:"billing_model_source,omitempty"`
	DefaultMultiplier   float64                  `json:"default_multiplier,omitempty"`
	ModelMultiplier     float64                  `json:"model_multiplier,omitempty"`
	FinalMultiplier     float64                  `json:"final_multiplier,omitempty"`
	BaseInputPrice      float64                  `json:"base_input_price,omitempty"`
	BaseOutputPrice     float64                  `json:"base_output_price,omitempty"`
	BaseCacheReadPrice  float64                  `json:"base_cache_read_price,omitempty"`
	BaseCacheWritePrice float64                  `json:"base_cache_write_price,omitempty"`
	FinalInputCost      float64                  `json:"final_input_cost,omitempty"`
	FinalOutputCost     float64                  `json:"final_output_cost,omitempty"`
	FinalCacheReadCost  float64                  `json:"final_cache_read_cost,omitempty"`
	FinalCacheWriteCost float64                  `json:"final_cache_write_cost,omitempty"`
}

func NormalizeAccessPlanSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// PromptOverridePatch is a partial write of one layer's system prompt (the 提示词 page).
// Nil fields keep the stored value. It only ever touches the three prompt columns, so a
// prompt save cannot change routing, and routing saves never carry it.
type PromptOverridePatch struct {
	SystemPromptOverride  *string             `json:"system_prompt_override,omitempty"`
	PromptOverrideMode    *PromptOverrideMode `json:"prompt_override_mode,omitempty"`
	PromptOverrideEnabled *bool               `json:"prompt_override_enabled,omitempty"`
}

// Empty reports whether the patch changes nothing.
func (p PromptOverridePatch) Empty() bool {
	return p.SystemPromptOverride == nil && p.PromptOverrideMode == nil && p.PromptOverrideEnabled == nil
}

// Apply merges the patch onto the stored (text, mode, enabled) triple and returns the
// normalized result. A legacy mode sent without the switch turns it on when there is text.
func (p PromptOverridePatch) Apply(text string, mode PromptOverrideMode, enabled bool) (string, PromptOverrideMode, bool, error) {
	if p.SystemPromptOverride != nil {
		text = strings.TrimSpace(*p.SystemPromptOverride)
	}
	if p.PromptOverrideMode != nil {
		if !ValidPromptOverrideMode(*p.PromptOverrideMode) {
			return "", "", false, fmt.Errorf("invalid prompt_override_mode %q", *p.PromptOverrideMode)
		}
		mode = *p.PromptOverrideMode
		if IsLegacyPromptOverrideMode(mode) && p.PromptOverrideEnabled == nil {
			enabled = strings.TrimSpace(text) != ""
		}
	}
	if p.PromptOverrideEnabled != nil {
		enabled = *p.PromptOverrideEnabled
	}
	return text, NormalizePromptOverrideMode(mode), enabled, nil
}
