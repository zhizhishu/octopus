package model

import (
	"sort"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

type AutoGroupType int

const (
	AutoGroupTypeNone  AutoGroupType = 0 //不自动分组
	AutoGroupTypeFuzzy AutoGroupType = 1 //模糊匹配
	AutoGroupTypeExact AutoGroupType = 2 //准确匹配
	AutoGroupTypeRegex AutoGroupType = 3 //正则匹配
)

// KeySelectStrategy controls the order in which a channel's available keys are tried.
type KeySelectStrategy int

const (
	// KeySelectStrategyCostBalanced is the zero value (and historical default): among
	// the available (enabled, not-cooling-down) keys, the one with the lowest
	// accumulated TotalCost is tried first. Every channel that predates this field
	// deserialises to 0 and behaves exactly as before.
	KeySelectStrategyCostBalanced KeySelectStrategy = 0
	// KeySelectStrategySticky ("同 key 优先") keeps hammering the SAME key — the
	// lowest-ID enabled key — for as long as it is healthy, ignoring cost. The
	// primary key only yields while it is cooling down / quarantined, and reclaims
	// priority the moment it recovers. This maximises per-key prompt-cache affinity
	// for upstreams (NVIDIA, Anthropic, …) that cache per key/account, where rotating
	// keys would throw the cache away on every turn.
	KeySelectStrategySticky KeySelectStrategy = 1
)

type Channel struct {
	ID                   int                   `json:"id" gorm:"primaryKey"`
	Name                 string                `json:"name" gorm:"unique;not null"`
	Type                 outbound.OutboundType `json:"type"`
	Enabled              bool                  `json:"enabled" gorm:"default:true"`
	Priority             int                   `json:"priority" gorm:"default:0"`
	MaxConcurrent        int                   `json:"max_concurrent" gorm:"default:0"` // 单渠道并发上限(在途+预约请求数), 0=不限. 达到上限后该渠道在选路中降档(让请求铺到其它渠道), 但不硬拉黑(无更优时仍可用)
	RPMLimit             int                   `json:"rpm_limit" gorm:"default:0"`      // 单渠道每分钟请求上限(近60s在途/已发起的上游尝试数), 0=不限. 达到上限后该渠道在选路中降档(同 MaxConcurrent 的软降档语义, 让突发铺到其它渠道), 不硬拉黑
	KeySelectStrategy    KeySelectStrategy     `json:"key_select_strategy" gorm:"default:0"` // key 选取策略: 0=成本均衡(默认,按TotalCost升序) 1=同key优先(粘住最小ID的健康key,只在它冷却时让位、恢复即切回,最大化按key的prompt缓存命中)
	BaseUrls             []BaseUrl             `json:"base_urls" gorm:"serializer:json"`
	Keys                 []ChannelKey          `json:"keys" gorm:"foreignKey:ChannelID"`
	Model                string                `json:"model"`
	CustomModel          string                `json:"custom_model"`
	DiscoveredModels     []string              `json:"discovered_models" gorm:"serializer:json"`
	SelectedModels       []string              `json:"selected_models" gorm:"serializer:json"`
	AnthropicContext1M   bool                  `json:"anthropic_context_1m" gorm:"column:anthropic_context_1m;default:false"`
	Proxy                bool                  `json:"proxy" gorm:"default:false"`
	AutoSync             bool                  `json:"auto_sync" gorm:"default:false"`
	AutoGroup            AutoGroupType         `json:"auto_group" gorm:"default:0"`
	CustomHeader         []CustomHeader        `json:"custom_header" gorm:"serializer:json"`
	Cloak                ChannelCloak          `json:"cloak" gorm:"serializer:json"`
	ParamOverride        *string               `json:"param_override"`
	SystemPromptOverride string                `json:"system_prompt_override"`
	PromptOverrideMode   PromptOverrideMode    `json:"prompt_override_mode" gorm:"default:append_system"`
	ChannelProxy         *string               `json:"channel_proxy"`
	OpenAIChatPath       string                `json:"openai_chat_path"`
	OpenAIModelsPath     string                `json:"openai_models_path"`
	Stats                *StatsChannel         `json:"stats,omitempty" gorm:"foreignKey:ChannelID"`
	MatchRegex           *string               `json:"match_regex"`
	CircuitTripped       bool                  `json:"circuit_tripped" gorm:"-"`
	CircuitRemainingSecs int                   `json:"circuit_remaining_seconds" gorm:"-"`
	CircuitOpenKeys      int                   `json:"circuit_open_keys" gorm:"-"`
}

type ChannelCircuitStatus struct {
	Tripped          bool `json:"circuit_tripped"`
	RemainingSeconds int  `json:"circuit_remaining_seconds"`
	OpenKeys         int  `json:"circuit_open_keys"`
}

type BaseUrl struct {
	URL   string `json:"url"`
	Delay int    `json:"delay"`
}

type CustomHeader struct {
	HeaderKey   string `json:"header_key"`
	HeaderValue string `json:"header_value"`
}

type ChannelCloak struct {
	Mode string `json:"mode,omitempty"`
	// ProfileID selects which fingerprint profile this channel cloaks as. 0 (the
	// zero value every existing channel deserialises to) means "use the global
	// default single fingerprint" — i.e. the per-instance seed + global header
	// settings, byte-for-byte identical to the behaviour before profiles existed.
	// A value > 0 points at a FingerprintProfile row so two channels (e.g. two
	// upstream keys behind different egress IPs) can present distinct devices.
	ProfileID int `json:"profile_id,omitempty"`
}

type ChannelKey struct {
	ID               int     `json:"id" gorm:"primaryKey"`
	ChannelID        int     `json:"channel_id"`
	Enabled          bool    `json:"enabled" gorm:"default:true"`
	ChannelKey       string  `json:"channel_key"`
	StatusCode       int     `json:"status_code"`
	LastUseTimeStamp int64   `json:"last_use_time_stamp"`
	TotalCost        float64 `json:"total_cost"`
	Remark           string  `json:"remark"`
	// DisabledReason/DisabledAt record WHY a key was last quarantined (e.g. an auth
	// error) and when, for operator visibility. They are set when the key's last
	// upstream status indicates a dead key and cleared on the next healthy (2xx)
	// response (self-heal). Persisted alongside StatusCode so the reason survives a
	// restart instead of silently retrying a known-bad key.
	DisabledReason string `json:"disabled_reason,omitempty"`
	DisabledAt     int64  `json:"disabled_at,omitempty"`
}

// ChannelUpdateRequest 渠道更新请求 - 仅包含变更的数据
type ChannelUpdateRequest struct {
	ID                   int                    `json:"id" binding:"required"`
	Name                 *string                `json:"name,omitempty"`
	Type                 *outbound.OutboundType `json:"type,omitempty"`
	Enabled              *bool                  `json:"enabled,omitempty"`
	Priority             *int                   `json:"priority,omitempty"`
	MaxConcurrent        *int                   `json:"max_concurrent,omitempty"`
	RPMLimit             *int                   `json:"rpm_limit,omitempty"`
	KeySelectStrategy    *KeySelectStrategy     `json:"key_select_strategy,omitempty"`
	BaseUrls             *[]BaseUrl             `json:"base_urls,omitempty"`
	Model                *string                `json:"model,omitempty"`
	CustomModel          *string                `json:"custom_model,omitempty"`
	DiscoveredModels     *[]string              `json:"discovered_models,omitempty"`
	SelectedModels       *[]string              `json:"selected_models,omitempty"`
	AnthropicContext1M   *bool                  `json:"anthropic_context_1m,omitempty"`
	Proxy                *bool                  `json:"proxy,omitempty"`
	AutoSync             *bool                  `json:"auto_sync,omitempty"`
	AutoGroup            *AutoGroupType         `json:"auto_group,omitempty"`
	CustomHeader         *[]CustomHeader        `json:"custom_header,omitempty"`
	Cloak                *ChannelCloak          `json:"cloak,omitempty"`
	ChannelProxy         *string                `json:"channel_proxy,omitempty"`
	ParamOverride        *string                `json:"param_override,omitempty"`
	SystemPromptOverride *string                `json:"system_prompt_override,omitempty"`
	PromptOverrideMode   *PromptOverrideMode    `json:"prompt_override_mode,omitempty"`
	MatchRegex           *string                `json:"match_regex,omitempty"`
	OpenAIChatPath       *string                `json:"openai_chat_path,omitempty"`
	OpenAIModelsPath     *string                `json:"openai_models_path,omitempty"`

	KeysToAdd    []ChannelKeyAddRequest    `json:"keys_to_add,omitempty"`
	KeysToUpdate []ChannelKeyUpdateRequest `json:"keys_to_update,omitempty"`
	KeysToDelete []int                     `json:"keys_to_delete,omitempty"`
}

type ChannelKeyAddRequest struct {
	Enabled    bool   `json:"enabled"`
	ChannelKey string `json:"channel_key" binding:"required"`
	Remark     string `json:"remark"`
}

type ChannelKeyUpdateRequest struct {
	ID         int     `json:"id" binding:"required"`
	Enabled    *bool   `json:"enabled,omitempty"`
	ChannelKey *string `json:"channel_key,omitempty"`
	Remark     *string `json:"remark,omitempty"`
}

type ChannelCSVImportOptions struct {
	DryRun     bool `json:"dry_run"`
	ReplaceKey bool `json:"replace_key"`
}

type ChannelCSVImportRowResult struct {
	Row       int    `json:"row"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	ChannelID int    `json:"channel_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ChannelCSVImportResult struct {
	Total   int                         `json:"total"`
	Created int                         `json:"created"`
	Updated int                         `json:"updated"`
	Skipped int                         `json:"skipped"`
	Failed  int                         `json:"failed"`
	Rows    []ChannelCSVImportRowResult `json:"rows"`
}

// ChannelFetchModelRequest is used by /channel/fetch-model (not persisted).
type ChannelFetchModelRequest struct {
	Type    outbound.OutboundType `json:"type" binding:"required"`
	BaseURL string                `json:"base_url" binding:"required"`
	Key     string                `json:"key" binding:"required"`
	Proxy   bool                  `json:"proxy"`
}

func (c *Channel) GetBaseUrl() string {
	if c == nil || len(c.BaseUrls) == 0 {
		return ""
	}

	bestURL := ""
	bestDelay := 0
	bestSet := false

	for _, bu := range c.BaseUrls {
		if bu.URL == "" {
			continue
		}
		if !bestSet || bu.Delay < bestDelay {
			bestURL = bu.URL
			bestDelay = bu.Delay
			bestSet = true
		}
	}

	return bestURL
}

func (c *Channel) GetOpenAIChatBaseUrl() string {
	if c == nil {
		return ""
	}
	baseURL := c.GetBaseUrl()
	chatPath := strings.TrimSpace(c.OpenAIChatPath)
	if chatPath == "" {
		return baseURL
	}
	joined, err := xurl.JoinCustomOpenAIChatPath(baseURL, chatPath)
	if err != nil {
		return baseURL
	}
	return joined
}

func (c *Channel) GetChannelKey() ChannelKey {
	keys := c.GetAvailableChannelKeys()
	if len(keys) == 0 {
		return ChannelKey{}
	}
	return keys[0]
}

// ChannelKeyCooldown is how long a key is skipped after a 429, and
// ChannelKeyTransientCooldown the (slightly shorter) skip for transient 5xx
// (502/503/504/520/529). Both are deliberately short: relay-style upstreams
// (e.g. anyrouter) return a bare 429/503 "Service Unavailable" for transient
// backend overload — with no Retry-After — so a multi-minute bench would needlessly
// shed a key that recovers in seconds and make "just retry" never succeed. A
// genuine provider rate-limit ships a Retry-After, which the runtime telemetry path
// honours and escalates over these floors (exponential backoff). Package defaults,
// not wired to a setting.
var (
	ChannelKeyCooldown          = 60 * time.Second
	ChannelKeyTransientCooldown = 30 * time.Second
	// ChannelKeyAuthErrorCooldown quarantines a key whose last upstream status was
	// 401 (the key itself is invalid/revoked — affects every model, unlike a
	// per-model rate-limit/circuit trip). It is long enough to stop burning requests
	// on a dead key every turn, but still finite so the key is periodically re-probed
	// and self-heals if the 401 was transient. Genuine 403/429/5xx stay on their own
	// (shorter, per-model-circuit) paths; only a key-wide 401 lands here.
	ChannelKeyAuthErrorCooldown = 15 * time.Minute
)

// keyCooldownWindow returns how long a key whose last upstream status was code
// should be skipped, and whether any cooldown applies. 429 and transient 5xx
// (502/503/504/520/529) both use short windows — relay upstreams use them
// interchangeably for "backend busy" — while genuine rate-limits escalate via the
// runtime path's exponential backoff and Retry-After.
func keyCooldownWindow(code int) (time.Duration, bool) {
	switch code {
	case 401:
		// Key-wide auth failure (invalid/revoked) — quarantine longer so we stop
		// retrying a dead key every turn, but keep it finite for periodic re-probe.
		return ChannelKeyAuthErrorCooldown, true
	case 429:
		return ChannelKeyCooldown, true
	case 502, 503, 504, 520, 529:
		return ChannelKeyTransientCooldown, true
	}
	return 0, false
}

func (c *Channel) GetAvailableChannelKeys() []ChannelKey {
	if c == nil || len(c.Keys) == 0 {
		return nil
	}

	nowSec := time.Now().Unix()
	keys := make([]ChannelKey, 0, len(c.Keys))
	cooled := make([]ChannelKey, 0, len(c.Keys))

	for _, k := range c.Keys {
		if !k.Enabled || k.ChannelKey == "" {
			continue
		}
		if window, ok := keyCooldownWindow(k.StatusCode); ok && k.LastUseTimeStamp > 0 &&
			nowSec-k.LastUseTimeStamp < int64(window/time.Second) {
			cooled = append(cooled, k)
			continue
		}
		keys = append(keys, k)
	}

	// Never black out a route that still has a usable key: if every enabled key is
	// only cooling down, fall back to them so the circuit breaker and Retry-After
	// remain the real recovery governors instead of returning "no available
	// channel" for the whole window (critical for single-key routes).
	if len(keys) == 0 {
		keys = cooled
	}

	switch c.KeySelectStrategy {
	case KeySelectStrategySticky:
		// 同 key 优先: order by key ID so the same (lowest-ID) healthy key is always
		// tried first regardless of cost. It only yields while cooling down (it then
		// drops to the `cooled` bucket above and the next-lowest ID takes over) and
		// reclaims priority the moment it self-heals — maximising prompt-cache affinity.
		sort.SliceStable(keys, func(i, j int) bool {
			return keys[i].ID < keys[j].ID
		})
	default:
		// 成本均衡(默认/历史行为): spread load toward the least-used key by cost.
		sort.SliceStable(keys, func(i, j int) bool {
			return keys[i].TotalCost < keys[j].TotalCost
		})
	}
	return keys
}
