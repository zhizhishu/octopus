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

	AccessPlanIDs       []int        `json:"access_plan_ids,omitempty" gorm:"-"`
	DefaultAccessPlanID int          `json:"default_access_plan_id,omitempty" gorm:"-"`
	AccessPlans         []AccessPlan `json:"access_plans,omitempty" gorm:"-"`
}

func AllAPIKeyEndpointFamilies() []APIKeyEndpointFamily {
	return []APIKeyEndpointFamily{
		APIKeyEndpointFamilyOpenAICompatible,
		APIKeyEndpointFamilyAnthropic,
		APIKeyEndpointFamilyGemini,
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
