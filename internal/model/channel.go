package model

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
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

// CapabilityEmbeddings 表示该渠道可承接 /v1/embeddings 流量。
const CapabilityEmbeddings = "embeddings"

type Channel struct {
	ID                int                   `json:"id" gorm:"primaryKey"`
	Name              string                `json:"name" gorm:"unique;not null"`
	Type              outbound.OutboundType `json:"type"`
	Enabled           bool                  `json:"enabled" gorm:"default:true"`
	Priority          int                   `json:"priority" gorm:"default:0"`
	MaxConcurrent     int                   `json:"max_concurrent" gorm:"default:0"`      // 单渠道并发上限(在途+预约请求数), 0=不限. 达到上限后该渠道在选路中降档(让请求铺到其它渠道), 但不硬拉黑(无更优时仍可用)
	RPMLimit          int                   `json:"rpm_limit" gorm:"default:0"`           // 单渠道每分钟请求上限(近60s在途/已发起的上游尝试数), 0=不限. 达到上限后该渠道在选路中降档(同 MaxConcurrent 的软降档语义, 让突发铺到其它渠道), 不硬拉黑
	KeySelectStrategy KeySelectStrategy     `json:"key_select_strategy" gorm:"default:0"` // key 选取策略: 0=成本均衡(默认,按TotalCost升序) 1=同key优先(粘住最小ID的健康key,只在它冷却时让位、恢复即切回,最大化按key的prompt缓存命中)
	// DisableCircuitBreaker turns this channel into a "no circuit breaker" pass-through:
	// it forwards EVERY request to the upstream like a direct client — never
	// short-circuited by the per-(channel,key,model) breaker, never benched by key
	// cooldown/quarantine, and failures accumulate no breaker/soft-cool state. It hands
	// the "retry to squeeze into upstream capacity" control back to the client (the exact
	// reason a direct connection can claw into a busy the relay while octopus could not).
	// The zero value (false) — every pre-existing channel — keeps the full breaker
	// behaviour byte-for-byte, so this is opt-in and changes nothing by default.
	DisableCircuitBreaker bool              `json:"disable_circuit_breaker" gorm:"default:false"`
	RaceMode              bool              `json:"race_mode" gorm:"default:false"`
	RaceKeyConcurrency    int               `json:"race_key_concurrency" gorm:"default:2"`
	RaceDelayMs           int               `json:"race_delay_ms" gorm:"default:0"`
	BaseUrls              []BaseUrl         `json:"base_urls" gorm:"serializer:json"`
	Keys                  []ChannelKey      `json:"keys" gorm:"foreignKey:ChannelID"`
	Model                 string            `json:"model"`
	CustomModel           string            `json:"custom_model"`
	DiscoveredModels      []string          `json:"discovered_models" gorm:"serializer:json"`
	SelectedModels        []string          `json:"selected_models" gorm:"serializer:json"`
	ModelMapping          map[string]string `json:"model_mapping" gorm:"serializer:json"`
	AnthropicContext1M    bool              `json:"anthropic_context_1m" gorm:"column:anthropic_context_1m;default:false"`
	// RedactEnabled (opt-in, default false) enables credential redaction for this
	// channel: outbound request bodies are scanned and secrets are replaced with
	// reversible {{Redact:...}} placeholders BEFORE octopus sends the request with
	// its own TLS/header fingerprint stack (the fingerprint bytes are never touched
	// — only body text content changes, which is the point of redaction). Response
	// text (JSON and SSE, including tool-argument deltas) restores placeholders
	// back to the original values before the client sees them. Detection and
	// restoration logic is vendored from CosyRedactGateway (Apache-2.0).
	RedactEnabled bool `json:"redact_enabled" gorm:"default:false"`
	// RedactFlags selects which detectors run for this channel, as CosyRedactGateway
	// flag letters: H high-entropy credentials, P phone, S sk- keys, I ID card,
	// B bank card, E email, G gitleaks. Empty = all detectors (HPSIBEG).
	RedactFlags  string         `json:"redact_flags" gorm:"default:''"`
	Proxy        bool           `json:"proxy" gorm:"default:false"`
	AutoSync     bool           `json:"auto_sync" gorm:"default:false"`
	CustomHeader []CustomHeader `json:"custom_header" gorm:"serializer:json"`
	Cloak        ChannelCloak   `json:"cloak" gorm:"serializer:json"`
	// ThinkingToContent (opt-in, default false), new-api style: on /v1/chat/completions
	// responses, reasoning is always folded into content as <think>…</think> (stream
	// and non-stream alike) and reasoning_content is dropped; tool-call turns are not
	// folded. Only the copy sent to the client changes — never the upstream request,
	// never the stored response used for billing/logs. See internal/model/channel_toggles.md.
	ThinkingToContent bool `json:"thinking_to_content" gorm:"default:false"`
	// Capabilities 声明本渠道能承接哪些非聊天流量（当前只有 "embeddings"）。
	// 空 = 只接聊天；所有存量渠道初始为空，行为与改动前完全一致。
	// 用 JSON 切片存，将来加 images/videos/rerank 只需加字符串，不必再改表。
	Capabilities         []string           `json:"capabilities" gorm:"serializer:json"`
	ParamOverride        *string            `json:"param_override"`
	SystemPromptOverride string             `json:"system_prompt_override"`
	PromptOverrideMode   PromptOverrideMode `json:"prompt_override_mode" gorm:"default:append_system"`
	// PromptOverrideEnabled is the 启用 switch of this channel's system prompt layer.
	PromptOverrideEnabled bool    `json:"prompt_override_enabled" gorm:"not null;default:false"`
	ChannelProxy          *string `json:"channel_proxy"`
	OpenAIChatPath        string  `json:"openai_chat_path"`
	OpenAIModelsPath      string  `json:"openai_models_path"`
	// OpenAIEmbeddingsPath 是嵌入端点的逃生口（与 OpenAIChatPath 同款）：火山这类
	// 上游的嵌入路径是 /api/v3/embeddings，用 JoinOpenAIPath 拼会变成
	// /api/v3/v1/embeddings。空 = 走标准 /v1/embeddings 拼接，存量渠道行为不变。
	OpenAIEmbeddingsPath string `json:"openai_embeddings_path"`
	// OpenAIImagesPath 图片端点的上游路径覆盖。空 = 沿用 /v1/images/<endpoint>。
	//
	// 用途：上游不是标准 OpenAI 路径时的逃生口（例如某些中转把图片挂在
	// /v1/images 之外的路径上）。存的是**完整路径**，不是前缀片段。
	OpenAIImagesPath string `json:"openai_images_path"`
	// OpenAIVideosPath 视频端点的上游路径覆盖。空 = 沿用 /v1/videos[/<kind>]。
	//
	// 只管**前缀段**：/generations、/edits、/extensions 子路径与 /{id}[/content]
	// 仍然拼在它后面，否则填了逃生口之后 edits 任务会打到 generations 上去。
	OpenAIVideosPath string `json:"openai_videos_path"`
	// OpenAITasksPath 视频/图片任务查询的路径覆盖。空 = 与创建路径同前缀。
	//
	// 单独一个字段的理由：有些上游创建在 /v1/videos，查询却在 /v1/tasks/{id}，
	// 两者不同源。硬绑成「同前缀」就接不上这类。
	OpenAITasksPath string `json:"openai_tasks_path"`
	// BatchImage 是离线批量生图（/v1/images/batches*）的渠道级配置。
	// 空 = 这个渠道不承接批处理流量（存量渠道全是空，行为与改动前完全一致）。
	//
	// 为什么用 JSON 列而不是散列几个顶层字段：批处理的上游只有 Google 两家，
	// 两家的必填项不一样（Gemini 只要 key；Vertex 要 project/location/桶），
	// 而且这属于**一种新流量的接入参数**，不是渠道的通用属性。散成顶层字段会让
	// channels 表为两个上游长出五六个永远为空的列，将来再加一个上游又是一片。
	BatchImage           *ChannelBatchImage `json:"batch_image,omitempty" gorm:"serializer:json"`
	Stats                *StatsChannel      `json:"stats,omitempty" gorm:"foreignKey:ChannelID"`
	MatchRegex           *string            `json:"match_regex"`
	CircuitTripped       bool               `json:"circuit_tripped" gorm:"-"`
	CircuitRemainingSecs int                `json:"circuit_remaining_seconds" gorm:"-"`
	CircuitOpenKeys      int                `json:"circuit_open_keys" gorm:"-"`
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

// ChannelBatchImage 是离线批量生图（/v1/images/batches*）的渠道级接入参数。
//
// 【凭证在哪】不在这里。上游的密钥 / 服务账号 JSON 一律走 ChannelKey.ChannelKey
// ——那是本仓唯一存凭证的地方，有轮换、健康、熔断一整套机制。把它俩分开放会让
// "换一把 key"变成要改两个地方。
//
// 【Provider 为什么在渠道上】批处理走哪家上游，取决于这个渠道的 key 是什么：
// 一把 AI Studio 的 API key 只能走 gemini_api，一个服务账号 JSON 只能走 Vertex。
// 让调用方在请求里点上游，等于允许用户替我们选凭证。
type ChannelBatchImage struct {
	// Provider 见 model.BatchImageProvider* 常量。空 = 不承接批处理。
	Provider string `json:"provider,omitempty"`
	// ProjectID / Location 只对 Vertex 有意义（Gemini 开发者 API 不需要）。
	// Vertex 的批次任务必须指定 project 与 location，且它们属于凭证的归属，
	// 不是用户请求能决定的。
	ProjectID string `json:"project_id,omitempty"`
	Location  string `json:"location,omitempty"`
	// GCSBucket / GCSPrefix 只对 Vertex 有意义：Vertex 的批处理**只认 GCS 上的
	// JSONL**，不接受内联请求，所以必须有个桶给输入和输出用。
	// 这是 Vertex 接入的硬前提，缺了它这一路根本发不出去（不是"可选优化"）。
	GCSBucket string `json:"gcs_bucket,omitempty"`
	GCSPrefix string `json:"gcs_prefix,omitempty"`
	// Models 是这个渠道声明可承接批处理的模型白名单。
	//
	// 必须显式声明而不是"渠道配了就能收"：批处理是**折扣计价**的，哪些模型在上游
	// 真的支持离线批处理由上游决定，我们猜不出来。空 = 这个渠道不报任何模型
	// （/models 端点据此枚举，宁可少报也不报一个必然失败的）。
	Models []string `json:"models,omitempty"`
}

// BatchImageProvider 返回渠道声明的批处理上游；未配置返回空串。
func (c *Channel) BatchImageProvider() string {
	if c == nil || c.BatchImage == nil {
		return ""
	}
	return strings.TrimSpace(c.BatchImage.Provider)
}

// BatchImageSupportsModel 报告这个渠道是否声明承接某个模型的批处理。
//
// 判据只看渠道自己的白名单，不看渠道的其它模型字段（Model / SelectedModels）：
// 后者是**聊天**流量的准入，与离线批处理不是一回事，混用会让"某个模型能聊天"
// 被误读成"某个模型能批处理"。
func (c *Channel) BatchImageSupportsModel(model string) bool {
	if c == nil || c.BatchImage == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range c.BatchImage.Models {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

type CustomHeader struct {
	HeaderKey   string `json:"header_key"`
	HeaderValue string `json:"header_value"`
}

type ChannelCloak struct {
	Mode string `json:"mode,omitempty"`
	// Shape overrides the outbound identity family for Chat channels only.
	// "" (zero value) = generic chat identity (byte-identical to today's behaviour).
	// "claude" = dress the chat outbound in the Claude CLI header set + claude TLS face.
	// "codex" = dress the chat outbound in the codex CLI header set + codex TLS face.
	// Ignored on Anthropic/Responses channels (their shape IS their protocol).
	// Any other value fails safe to generic.
	Shape string `json:"shape,omitempty"`
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
	ID                    int                    `json:"id" binding:"required"`
	Name                  *string                `json:"name,omitempty"`
	Type                  *outbound.OutboundType `json:"type,omitempty"`
	Enabled               *bool                  `json:"enabled,omitempty"`
	Priority              *int                   `json:"priority,omitempty"`
	MaxConcurrent         *int                   `json:"max_concurrent,omitempty"`
	RPMLimit              *int                   `json:"rpm_limit,omitempty"`
	KeySelectStrategy     *KeySelectStrategy     `json:"key_select_strategy,omitempty"`
	DisableCircuitBreaker *bool                  `json:"disable_circuit_breaker,omitempty"`
	RaceMode              *bool                  `json:"race_mode,omitempty"`
	RaceKeyConcurrency    *int                   `json:"race_key_concurrency,omitempty"`
	RaceDelayMs           *int                   `json:"race_delay_ms,omitempty"`
	BaseUrls              *[]BaseUrl             `json:"base_urls,omitempty"`
	Model                 *string                `json:"model,omitempty"`
	CustomModel           *string                `json:"custom_model,omitempty"`
	DiscoveredModels      *[]string              `json:"discovered_models,omitempty"`
	SelectedModels        *[]string              `json:"selected_models,omitempty"`
	AnthropicContext1M    *bool                  `json:"anthropic_context_1m,omitempty"`
	RedactEnabled         *bool                  `json:"redact_enabled,omitempty"`
	RedactFlags           *string                `json:"redact_flags,omitempty"`
	ThinkingToContent     *bool                  `json:"thinking_to_content,omitempty"`
	Capabilities          *[]string              `json:"capabilities,omitempty"`
	Proxy                 *bool                  `json:"proxy,omitempty"`
	AutoSync              *bool                  `json:"auto_sync,omitempty"`
	CustomHeader          *[]CustomHeader        `json:"custom_header,omitempty"`
	Cloak                 *ChannelCloak          `json:"cloak,omitempty"`
	ChannelProxy          *string                `json:"channel_proxy,omitempty"`
	ParamOverride         *string                `json:"param_override,omitempty"`
	SystemPromptOverride  *string                `json:"system_prompt_override,omitempty"`
	PromptOverrideMode    *PromptOverrideMode    `json:"prompt_override_mode,omitempty"`
	PromptOverrideEnabled *bool                  `json:"prompt_override_enabled,omitempty"`
	MatchRegex            *string                `json:"match_regex,omitempty"`
	ModelMapping          *map[string]string     `json:"model_mapping,omitempty"`
	OpenAIChatPath        *string                `json:"openai_chat_path,omitempty"`
	OpenAIModelsPath      *string                `json:"openai_models_path,omitempty"`
	OpenAIEmbeddingsPath  *string                `json:"openai_embeddings_path,omitempty"`
	OpenAIImagesPath      *string                `json:"openai_images_path,omitempty"`
	OpenAIVideosPath      *string                `json:"openai_videos_path,omitempty"`
	OpenAITasksPath       *string                `json:"openai_tasks_path,omitempty"`
	// BatchImage 部分更新批处理接入参数。nil = 不动这一列（与其余指针字段同语义）。
	// 传一个空对象 `{}` 则是**清空**（= 这个渠道不再承接批处理流量）。
	BatchImage *ChannelBatchImage `json:"batch_image,omitempty"`

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

// ValidateChannelRaceSettings validates race parameters for a channel or request.
// Concurrency must be in [2, 5] (default 2 if 0/unspecified).
// Delay ms must be in [0, 5000] (default 0 if unspecified).
func ValidateChannelRaceSettings(raceMode bool, concurrency int, delayMs int) error {
	if !raceMode {
		return nil
	}
	if concurrency < 2 || concurrency > 5 {
		return fmt.Errorf("race_key_concurrency must be between 2 and 5 (got %d)", concurrency)
	}
	if delayMs < 0 || delayMs > 5000 {
		return fmt.Errorf("race_delay_ms must be between 0 and 5000 (got %d)", delayMs)
	}
	return nil
}

func (c *Channel) Validate() error {
	if c == nil {
		return nil
	}
	// Reject unknown detector letters instead of silently dropping them.
	// NormalizeRedactFlags keeps only the known alphabet, so "EX" would quietly
	// become "E" and the operator would believe a detector is on when it is not.
	// Failing the save is the only outcome they can notice.
	if HasInvalidRedactFlag(c.RedactFlags) {
		return fmt.Errorf("redact_flags 含有未知检测器字母 %q，可用字母为 %s", c.RedactFlags, RedactFlagLetters)
	}
	if c.RaceMode {
		concurrency := c.RaceKeyConcurrency
		if concurrency == 0 {
			concurrency = 2
		}
		if err := ValidateChannelRaceSettings(true, concurrency, c.RaceDelayMs); err != nil {
			return err
		}
	}
	return nil
}

func (req *ChannelUpdateRequest) Validate() error {
	if req == nil {
		return nil
	}
	// Same rule as Channel.Validate: only validate when the caller actually sets the
	// field, otherwise a PATCH that does not mention redaction would be rejected on
	// the stored value's behalf.
	if req.RedactFlags != nil && HasInvalidRedactFlag(*req.RedactFlags) {
		return fmt.Errorf("redact_flags 含有未知检测器字母 %q，可用字母为 %s", *req.RedactFlags, RedactFlagLetters)
	}
	concurrency := 2
	if req.RaceKeyConcurrency != nil {
		concurrency = *req.RaceKeyConcurrency
	}
	delayMs := 0
	if req.RaceDelayMs != nil {
		delayMs = *req.RaceDelayMs
	}
	if req.RaceMode != nil && *req.RaceMode {
		if err := ValidateChannelRaceSettings(true, concurrency, delayMs); err != nil {
			return err
		}
	} else {
		if req.RaceKeyConcurrency != nil && (*req.RaceKeyConcurrency < 2 || *req.RaceKeyConcurrency > 5) {
			return fmt.Errorf("race_key_concurrency must be between 2 and 5 (got %d)", *req.RaceKeyConcurrency)
		}
		if req.RaceDelayMs != nil && (*req.RaceDelayMs < 0 || *req.RaceDelayMs > 5000) {
			return fmt.Errorf("race_delay_ms must be between 0 and 5000 (got %d)", *req.RaceDelayMs)
		}
	}
	return nil
}

// SupportsCapability 判断渠道能否承接某类非聊天流量。
//
// 🔴 空 Capabilities 必须退回按渠道类型判定 —— 这是上线零影响的安全带：
// 存量渠道没填过这个字段，行为必须与改动前完全一致。
// SupportsCapability: 渠道类型自带的能力 OR 能力位里勾选的（2026-10-31 决定）。
// 以前非空 Capabilities 会整体替换类型默认值——将来给嵌入渠道（type 5）加个别的能力位
// （例如 "rerank"）就会悄悄丢掉它本职的嵌入。现在类型默认值永远保留，勾选只做加法。
// 空 Capabilities 的存量渠道行为不变。
func (c *Channel) SupportsCapability(capability string) bool {
	if c == nil {
		return false
	}
	switch capability {
	case CapabilityEmbeddings:
		if outbound.IsEmbeddingChannelType(c.Type) {
			return true
		}
	}
	return slices.Contains(c.Capabilities, capability)
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

// VerbatimRequestBaseURL is the chat URL for a type-6 channel.
// The stored base URL is the chat request. An absolute chat-path override
// replaces it. Embedding requests use an absolute embeddings path only;
// a missing or relative embeddings path returns empty so the caller can skip
// the channel instead of posting to the chat URL.
func (c *Channel) VerbatimRequestBaseURL(embedding bool) string {
	if c == nil {
		return ""
	}
	if embedding {
		if abs, ok, err := xurl.ParseEndpointOverride(c.OpenAIEmbeddingsPath); err == nil && ok {
			return abs
		}
		return ""
	}
	if abs, ok, err := xurl.ParseEndpointOverride(c.OpenAIChatPath); err == nil && ok {
		return abs
	}
	return c.GetBaseUrl()
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

// GetOpenAIEmbeddingsBaseUrl 把渠道的嵌入自定义路径拼进 base url（与
// GetOpenAIChatBaseUrl 同款拼法）。空路径 = 原样返回 base url，存量渠道行为不变。
func (c *Channel) GetOpenAIEmbeddingsBaseUrl() string {
	if c == nil {
		return ""
	}
	baseURL := c.GetBaseUrl()
	embeddingsPath := strings.TrimSpace(c.OpenAIEmbeddingsPath)
	if embeddingsPath == "" {
		return baseURL
	}
	joined, err := xurl.JoinCustomOpenAIEmbeddingsPath(baseURL, embeddingsPath)
	if err != nil {
		return baseURL
	}
	return joined
}

// 渠道级路径逃生口（图片/视频/任务查询）。
//
// 这三个方法的共同约定：返回的是**要直接传给 xurl.JoinOpenAIPath 的路径串**，
// 字段为空时返回默认值（= 改动前的标准路径，一字节不变）。判据单一来源，
// 调用点不用各写一遍判空。
const (
	openAIImagesDefaultPrefix = "/v1"
	openAIImagesSegment       = "/images"
	openAIVideosDefaultPrefix = "/v1/videos" // 必须与 relay.videoUpstreamPath 的前缀一致
)

// GetOpenAIImagesPath 返回图片端点的上游路径（要传给 JoinOpenAIPath 的串）。
//
// endpoint 是 canonical 的 /images/generations | /images/edits | /images/variations。
//   - 字段为空："/v1" + endpoint（存量行为，一字节不变）；
//   - 字段非空：用字段值替换 "/v1/images" 这段前缀，**操作段必须留着**
//     （填 /openai/images 后 generations 打到 /openai/images/generations）。
func (c *Channel) GetOpenAIImagesPath(endpoint string) string {
	defaultPath := openAIImagesDefaultPrefix + endpoint
	if c == nil {
		return defaultPath
	}
	override := strings.TrimSpace(c.OpenAIImagesPath)
	if override == "" {
		return defaultPath
	}
	operation := strings.TrimPrefix("/"+strings.Trim(strings.TrimSpace(endpoint), "/"), openAIImagesSegment)
	return strings.TrimRight(override, "/") + operation
}

// GetOpenAIVideosPath 返回视频创建/查询的**前缀**路径（要传给 JoinOpenAIPath 的串）。
//
// 空 = /v1/videos（存量行为）。非空只替换 /v1/videos 这一段前缀，
// /generations | /edits | /extensions 子路径与 /{id}[/content] 由调用方拼在后面——
// 丢掉 kind 子路径会让 edits 任务打到 generations 上去。
func (c *Channel) GetOpenAIVideosPath() string {
	if c == nil {
		return openAIVideosDefaultPrefix
	}
	if override := strings.TrimSpace(c.OpenAIVideosPath); override != "" {
		return strings.TrimRight(override, "/")
	}
	return openAIVideosDefaultPrefix
}

// GetOpenAITasksPath 返回任务查询/取成品的路径覆盖（要传给 JoinOpenAIPath 的串）。
//
// 空串 = 不覆盖：查询与创建同前缀（存量行为）。非空时查询/取成品用这个前缀 +
// /{id}[/content]，创建仍然走 GetOpenAIVideosPath()。
func (c *Channel) GetOpenAITasksPath() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(c.OpenAITasksPath), "/")
}

func (c *Channel) GetChannelKey() ChannelKey {
	// Fetching a key to build an auth header (model sync/discovery) is not the
	// concurrent-request hot path, so it must NOT consume a cooldown probe slot and
	// keeps the historical unthrottled fallback (returns the cooling key if that is all
	// there is) — no behaviour change for that path.
	keys := c.availableChannelKeys(false)
	if len(keys) == 0 {
		return ChannelKey{}
	}
	return keys[0]
}

// ChannelKeyCooldown is how long a key is skipped after a 429, and
// ChannelKeyTransientCooldown the (slightly shorter) skip for transient 5xx
// (502/503/504/520/529). Both are deliberately short and aligned with the runtime
// telemetry backoff base (429→5s, 5xx→3s): relay-style upstreams (e.g. the relay)
// return a bare 429/503 "Service Unavailable" for transient backend overload — with
// no Retry-After — that clears in seconds. On a single-key route these windows are
// the ONLY thing between the client and the upstream, so a long bench (the old 60s)
// turned one transient 429 into a wall of "no available channel" that outlasted a
// CLI's own retry budget — the exact opposite of hitting the relay directly, where
// the client just backs off a few seconds and gets straight back in. Keeping the
// window at a few seconds lets the caller's native retry land right after it and
// succeed like a direct connection, while a genuine sustained rate-limit still
// escalates via the runtime telemetry path (exponential backoff, honoured
// Retry-After) and the per-model circuit breaker. Package defaults, not wired to a
// setting.
var (
	ChannelKeyCooldown          = 5 * time.Second
	ChannelKeyTransientCooldown = 3 * time.Second
	// ChannelKeyAuthErrorCooldown quarantines a key whose last upstream status was
	// 401 (the key itself is invalid/revoked — affects every model, unlike a
	// per-model rate-limit/circuit trip). It is long enough to stop burning requests
	// on a dead key every turn, but still finite so the key is periodically re-probed
	// and self-heals if the 401 was transient. Genuine 403/429/5xx stay on their own
	// (shorter, per-model-circuit) paths; only a key-wide 401 lands here.
	ChannelKeyAuthErrorCooldown = 15 * time.Minute
	// ChannelKeyProbeInterval throttles the all-keys-cooling fallback in
	// GetAvailableChannelKeys: while every enabled key of a channel is cooling down we
	// still let ONE request through per this interval to probe the (single) key so the
	// route self-heals, but make every other concurrent/rapid request fail over (or
	// fail fast) instead of all slamming the just-failed key at once — each of those
	// would otherwise pay a wasted upstream round-trip every turn (the cooldown was
	// effectively toothless for single-key routes). Kept short so a recovered key is
	// re-tried within ~1s; the circuit breaker and Retry-After remain the real recovery
	// governors. Not wired to a setting (same as the cooldown knobs above).
	ChannelKeyProbeInterval = 1 * time.Second
)

// cooldownProbeLastAdmit records, per channel ID, the last time GetAvailableChannelKeys
// admitted a cooldown-fallback probe. cooldownProbeMu guards it so the check-and-record
// is atomic: under a concurrent burst exactly one request per ChannelKeyProbeInterval is
// admitted and the rest get an empty key set. The map is bounded by the number of
// channels (a handful of time.Time entries) so it needs no eviction.
var (
	cooldownProbeMu        sync.Mutex
	cooldownProbeLastAdmit = map[int]time.Time{}
)

// admitCooldownProbe reports whether GetAvailableChannelKeys may surface the cooling
// fallback key for channelID right now, recording the admission when it returns true.
// A channelID of 0 (an unpersisted channel, e.g. in unit tests) has no stable identity
// to throttle on, so it is never throttled — preserving the "never fully black out a
// single usable key" fallback.
func admitCooldownProbe(channelID int, now time.Time) bool {
	if channelID == 0 {
		return true
	}
	cooldownProbeMu.Lock()
	defer cooldownProbeMu.Unlock()
	if last, ok := cooldownProbeLastAdmit[channelID]; ok && now.Sub(last) < ChannelKeyProbeInterval {
		return false
	}
	cooldownProbeLastAdmit[channelID] = now
	return true
}

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

// GetAvailableChannelKeys returns the keys the relay hot path may try for this channel,
// in priority order. When every enabled key is cooling down it throttles the fallback
// probe (see availableChannelKeys / ChannelKeyProbeInterval) so a burst of concurrent
// requests does not all slam the just-failed key.
func (c *Channel) GetAvailableChannelKeys() []ChannelKey {
	return c.availableChannelKeys(true)
}

// GetAvailableChannelKeysLastResort returns the tryable keys WITHOUT the hot-path
// probe throttle: when every enabled key is only briefly cooling, it still surfaces
// them so the request reaches the upstream instead of the route blacking out. The
// relay uses this only on the final candidate channel (no peer left to spill over to),
// so a single-route client's retry behaves like a direct upstream connection — the
// throttle still governs earlier candidates that DO have peers to spread onto. The
// circuit breaker remains the real backstop against hammering a genuinely dead key.
func (c *Channel) GetAvailableChannelKeysLastResort() []ChannelKey {
	return c.availableChannelKeys(false)
}

// availableChannelKeys computes the tryable keys. throttleFallback gates the
// all-keys-cooling fallback: when true (the request hot path) at most one probe per
// ChannelKeyProbeInterval is surfaced, so excess concurrent requests get an empty set
// and fail over / fast instead of hammering the just-failed key; when false (auth-header
// fetch) the fallback is unthrottled, preserving the historical never-black-out
// behaviour. The throttle only ever engages on the fallback — as long as at least one
// key is not cooling down, callers get the full ready set unchanged.
func (c *Channel) availableChannelKeys(throttleFallback bool) []ChannelKey {
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
	// channel" for the whole window (critical for single-key routes). On the hot path
	// this fallback is rate-limited (admitCooldownProbe): at most one probe per
	// ChannelKeyProbeInterval, so a concurrent burst does not all re-hit the just-failed
	// key (each paying a wasted round-trip). The && short-circuits so a probe slot is
	// only consumed when there is actually a cooling key to fall back to.
	if len(keys) == 0 {
		if len(cooled) > 0 && (!throttleFallback || admitCooldownProbe(c.ID, time.Now())) {
			keys = cooled
		}
	}

	sortChannelKeysByStrategy(keys, c.KeySelectStrategy)
	return keys
}

// sortChannelKeysByStrategy orders keys in place per the channel's key-select strategy.
// Shared by the cooldown-aware hot path (availableChannelKeys) and the cooldown-ignoring
// GetAllEnabledChannelKeys so both honour "同 key 优先" vs "成本均衡" identically.
func sortChannelKeysByStrategy(keys []ChannelKey, strategy KeySelectStrategy) {
	switch strategy {
	case KeySelectStrategySticky:
		// 同 key 优先: order by key ID so the same (lowest-ID) healthy key is always
		// tried first regardless of cost — maximising prompt-cache affinity.
		sort.SliceStable(keys, func(i, j int) bool {
			return keys[i].ID < keys[j].ID
		})
	default:
		// 成本均衡(默认/历史行为): spread load toward the least-used key by cost.
		sort.SliceStable(keys, func(i, j int) bool {
			return keys[i].TotalCost < keys[j].TotalCost
		})
	}
}

// GetAllEnabledChannelKeys returns every enabled, non-empty key in the channel's
// configured select-strategy order, WITHOUT any cooldown/quarantine filtering or
// fallback throttling. It backs the per-channel DisableCircuitBreaker mode: such a
// channel forwards every request to the upstream exactly like a direct client, so a key
// that just returned 429/5xx/401 must NOT be benched — the client's own retry keeps
// hammering the upstream to claw back capacity. Regular channels never call this; they
// stay on GetAvailableChannelKeys with its cooldown discipline.
func (c *Channel) GetAllEnabledChannelKeys() []ChannelKey {
	if c == nil || len(c.Keys) == 0 {
		return nil
	}
	keys := make([]ChannelKey, 0, len(c.Keys))
	for _, k := range c.Keys {
		if !k.Enabled || k.ChannelKey == "" {
			continue
		}
		keys = append(keys, k)
	}
	sortChannelKeysByStrategy(keys, c.KeySelectStrategy)
	return keys
}
