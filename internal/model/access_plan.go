package model

import "strings"

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

const (
	PromptOverrideModeAppendSystem  PromptOverrideMode = "append_system"
	PromptOverrideModeReplaceSystem PromptOverrideMode = "replace_system"
)

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
	ID                   int                      `json:"id" gorm:"primaryKey"`
	RouteProfileID       int                      `json:"route_profile_id" gorm:"not null;index:idx_access_route_rule,unique"`
	RequestModel         string                   `json:"request_model" gorm:"not null;index:idx_access_route_rule,unique"`
	Mode                 GroupMode                `json:"mode" gorm:"default:3"`
	BillingModelSource   AccessBillingModelSource `json:"billing_model_source" gorm:"default:request_model"`
	BillingModelOverride string                   `json:"billing_model_override"`
	FallbackMode         AccessRouteFallbackMode  `json:"fallback_mode" gorm:"default:failover"`
	SystemPromptOverride string                   `json:"system_prompt_override"`
	PromptOverrideMode   PromptOverrideMode       `json:"prompt_override_mode" gorm:"default:append_system"`
	Targets              []AccessRouteTarget      `json:"targets,omitempty" gorm:"foreignKey:RouteRuleID;constraint:OnDelete:CASCADE"`
}

type AccessRouteTarget struct {
	ID            int    `json:"id" gorm:"primaryKey"`
	RouteRuleID   int    `json:"route_rule_id" gorm:"not null;index:idx_access_route_target,unique"`
	ChannelID     int    `json:"channel_id" gorm:"not null;index:idx_access_route_target,unique"`
	UpstreamModel string `json:"upstream_model" gorm:"not null;index:idx_access_route_target,unique"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	Enabled       bool   `json:"enabled" gorm:"default:true"`

	AccessPlanID         int                      `json:"access_plan_id,omitempty" gorm:"-"`
	RequestModel         string                   `json:"request_model,omitempty" gorm:"-"`
	Mode                 GroupMode                `json:"mode,omitempty" gorm:"-"`
	BillingModelSource   AccessBillingModelSource `json:"billing_model_source,omitempty" gorm:"-"`
	BillingModelOverride string                   `json:"billing_model_override,omitempty" gorm:"-"`
	FallbackMode         AccessRouteFallbackMode  `json:"fallback_mode,omitempty" gorm:"-"`
	SystemPromptOverride string                   `json:"system_prompt_override,omitempty" gorm:"-"`
	PromptOverrideMode   PromptOverrideMode       `json:"prompt_override_mode,omitempty" gorm:"-"`
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
