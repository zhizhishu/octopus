package model

import (
	"fmt"
	"strings"
)

type APIKeyEndpointFamily string

const (
	APIKeyEndpointFamilyOpenAICompatible APIKeyEndpointFamily = "openai-compatible"
	APIKeyEndpointFamilyAnthropic        APIKeyEndpointFamily = "anthropic"
	APIKeyEndpointFamilyGemini           APIKeyEndpointFamily = "gemini"
	// APIKeyEndpointFamilyVideo 是可配置的独立 family（/v1/videos 按次计费，
	// 不能和 chat 共用一个 family，否则"只许聊天"的 key 也能生成视频）。
	APIKeyEndpointFamilyVideo APIKeyEndpointFamily = "video"
	// APIKeyEndpointFamilyUnclassified 不是可配置项：它是中间件对"未归类路径"
	// 的返回值。NormalizeAPIKeyEndpointFamily 不认它，因此 AllowsEndpointFamily
	// 恒为 false —— 新增 relay 端点忘记归类时是拒绝，而不是放行。
	APIKeyEndpointFamilyUnclassified APIKeyEndpointFamily = "unclassified"
)

type APIKey struct {
	ID               int                    `json:"id" gorm:"primaryKey"`
	UserID           int                    `json:"user_id" gorm:"index;default:0"`
	UserName         string                 `json:"user_name,omitempty" gorm:"-"`
	Name             string                 `json:"name" gorm:"not null"`
	APIKey           string                 `json:"api_key" gorm:"not null"`
	Enabled          bool                   `json:"enabled" gorm:"default:true"`
	ExpireAt         int64                  `json:"expire_at,omitempty"`
	MaxCost          float64                `json:"max_cost,omitempty"`
	SupportedModels  string                 `json:"supported_models,omitempty"`
	EndpointFamilies []APIKeyEndpointFamily `json:"endpoint_families,omitempty" gorm:"serializer:json"`
	// LastUsedAt / LastUsedIP：这把 Key 最近一次转发请求的时间和来源 IP（与 User.LastRelayAt/IP 同口径，
	// 纯审计字段：转发路径只写内存，周期任务批量落库）。更新 Key 时不随请求体覆盖。
	LastUsedAt int64  `json:"last_used_at" gorm:"default:0"`
	LastUsedIP string `json:"last_used_ip,omitempty" gorm:"size:128"`

	AccessPlanIDs       []int        `json:"access_plan_ids,omitempty" gorm:"-"`
	DefaultAccessPlanID int          `json:"default_access_plan_id,omitempty" gorm:"-"`
	AccessPlans         []AccessPlan `json:"access_plans,omitempty" gorm:"-"`
}

// API Key 批量操作（POST /api/v1/apikey/batch）。请求和结果的形状与 /user/batch 一致。
const (
	APIKeyBatchEnable  = "enable"
	APIKeyBatchDisable = "disable"
	APIKeyBatchDelete  = "delete"
)

type APIKeyBatchRequest struct {
	IDs    []int  `json:"ids"`
	Action string `json:"action"`
}

type APIKeyBatchResult = UserBatchResult

func AllAPIKeyEndpointFamilies() []APIKeyEndpointFamily {
	return []APIKeyEndpointFamily{
		APIKeyEndpointFamilyOpenAICompatible,
		APIKeyEndpointFamilyAnthropic,
		APIKeyEndpointFamilyGemini,
		APIKeyEndpointFamilyVideo,
	}
}

func NormalizeAPIKeyEndpointFamily(family APIKeyEndpointFamily) (APIKeyEndpointFamily, bool) {
	switch APIKeyEndpointFamily(strings.ToLower(strings.TrimSpace(string(family)))) {
	case APIKeyEndpointFamilyOpenAICompatible, "openai", "openai_compatible":
		return APIKeyEndpointFamilyOpenAICompatible, true
	case APIKeyEndpointFamilyAnthropic, "claude", "anthropic/claude":
		return APIKeyEndpointFamilyAnthropic, true
	case APIKeyEndpointFamilyGemini:
		return APIKeyEndpointFamilyGemini, true
	case APIKeyEndpointFamilyVideo, "videos":
		return APIKeyEndpointFamilyVideo, true
	default:
		return "", false
	}
}

func NormalizeAPIKeyEndpointFamilies(families []APIKeyEndpointFamily) ([]APIKeyEndpointFamily, error) {
	if len(families) == 0 {
		return nil, nil
	}
	seen := make(map[APIKeyEndpointFamily]struct{}, len(families))
	for _, family := range families {
		normalized, ok := NormalizeAPIKeyEndpointFamily(family)
		if !ok {
			return nil, fmt.Errorf("unsupported endpoint family %q", family)
		}
		seen[normalized] = struct{}{}
	}

	normalized := make([]APIKeyEndpointFamily, 0, len(seen))
	for _, family := range AllAPIKeyEndpointFamilies() {
		if _, ok := seen[family]; ok {
			normalized = append(normalized, family)
		}
	}
	return normalized, nil
}

func (key APIKey) EffectiveEndpointFamilies() []APIKeyEndpointFamily {
	if len(key.EndpointFamilies) == 0 {
		return AllAPIKeyEndpointFamilies()
	}
	families, err := NormalizeAPIKeyEndpointFamilies(key.EndpointFamilies)
	if err != nil || len(families) == 0 {
		return nil
	}
	return families
}

func (key APIKey) AllowsEndpointFamily(family APIKeyEndpointFamily) bool {
	normalized, ok := NormalizeAPIKeyEndpointFamily(family)
	if !ok {
		return false
	}
	for _, allowed := range key.EffectiveEndpointFamilies() {
		if allowed == normalized {
			return true
		}
	}
	return false
}
