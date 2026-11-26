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

	// Guest：访客 Key（管理员专用、临时给客人用）。只能由 /api/v1/guestkey/* 建（AdminOnly），
	// 建好后永远是访客 Key：普通 /apikey/create|update 都不能设、不能改、不能把它改成普通 Key。
	// 归属固定是建它的管理员（计费走管理员的池子 + 这把 Key 自己的 MaxCost），
	// 鉴权时只放行转发端点、且必须带模型白名单（见 middleware.APIKeyAuth）。
	Guest bool `json:"guest" gorm:"default:false;index"`
	// Note：备注（访客 Key 用来写「给谁、干什么」）。
	Note string `json:"note,omitempty" gorm:"size:512"`
	// RevokedAt：作废时间（Unix 秒，0 = 没作废）。作废不可撤销：鉴权一律拒绝，也不能再启用；
	// 记录保留，日志 / 用量 / IP 还能按这把 Key 查。
	RevokedAt int64 `json:"revoked_at,omitempty" gorm:"default:0"`

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

// GuestKeyRequest 是访客 Key 的建 / 改请求（POST /api/v1/guestkey/create|update）。
// 故意不复用 APIKey：没有 user_id / api_key / guest / revoked_at 这些字段，
// 请求体里写了也没地方落，从形状上就杜绝「顺手改归属 / 改成普通 Key」。
type GuestKeyRequest struct {
	ID               int                    `json:"id"`
	Name             string                 `json:"name"`
	Note             string                 `json:"note"`
	Models           []string               `json:"models"`
	ExpireAt         int64                  `json:"expire_at"`
	MaxCost          float64                `json:"max_cost"`
	Enabled          *bool                  `json:"enabled,omitempty"`
	EndpointFamilies []APIKeyEndpointFamily `json:"endpoint_families,omitempty"`
}

// GuestKeyAccess 是一把访客 Key 的来源 IP / 客户端（来自 user_access_seen，与用户页同一份数据）。
type GuestKeyAccess struct {
	APIKeyID      int               `json:"api_key_id"`
	TotalRequests int64             `json:"total_requests"`
	KeepDays      int               `json:"keep_days"`
	Last          *UserAccessLast   `json:"last,omitempty"`
	IPs           []UserAccessGroup `json:"ips"`
	Clients       []UserAccessGroup `json:"clients"`
}

// APIKeyGuestContextKey：gin 上下文里「这次请求用的是访客 Key」（bool）。中间件写、relay 读。
const APIKeyGuestContextKey = "api_key_guest"

// IsRevoked：已作废。
func (key APIKey) IsRevoked() bool { return key.RevokedAt > 0 }

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
