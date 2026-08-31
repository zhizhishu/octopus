package model

type AttemptStatus string

const (
	AttemptSuccess      AttemptStatus = "success"
	AttemptFailed       AttemptStatus = "failed"
	AttemptCircuitBreak AttemptStatus = "circuit_break"
	AttemptSkipped      AttemptStatus = "skipped"
)

const (
	RelayLogErrorCodeClientEmptyRequest      = "client_empty_request"
	RelayLogErrorCodeCursorEmptyProbe        = "cursor_empty_probe"
	RelayLogErrorStrategyLocalValidation     = "local_validation;upstream_forwarded=false;billable=false;stats_counted=false"
	RelayLogErrorStrategyLocalCursorProbe    = "local_validation;cursor_empty_probe=true;upstream_forwarded=false;billable=false;stats_counted=false"
	RelayLogErrorStrategyLocalValidationPart = "local_validation"
)

const (
	RelayLogUsageSourceUpstream        = "upstream_usage"
	RelayLogUsageSourceNoUsage         = "no_usage"
	RelayLogUsageSourceLocalValidation = "local_validation"
	// RelayLogUsageSourceLocalEstimate marks a log whose token/cost figures were
	// counted locally (via the tokenizer) because the upstream omitted usage — the
	// numbers are an estimate, not upstream-authoritative.
	RelayLogUsageSourceLocalEstimate = "local_estimate"

	RelayLogUsageMissingReasonClientAborted        = "client_aborted"
	RelayLogUsageMissingReasonNoInternalResponse   = "no_internal_response"
	RelayLogUsageMissingReasonUpstreamUsageMissing = "upstream_usage_missing"
	RelayLogUsageMissingReasonZeroUsageReported    = "zero_usage_reported"
	RelayLogUsageMissingReasonLocalValidation      = "local_validation"
	RelayLogUsageMissingReasonOpaqueResponse       = "opaque_response"
)

type ChannelAttempt struct {
	ChannelID    int           `json:"channel_id"`
	ChannelKeyID int           `json:"channel_key_id,omitempty"`
	ChannelName  string        `json:"channel_name"`
	ModelName    string        `json:"model_name"`
	UpstreamPath string        `json:"upstream_path,omitempty"`
	Endpoint     string        `json:"endpoint_family,omitempty"`
	Capability   string        `json:"capability,omitempty"`
	AttemptNum   int           `json:"attempt_num"`
	Status       AttemptStatus `json:"status"`
	Duration     int           `json:"duration"`
	Sticky       bool          `json:"sticky,omitempty"`
	ProxyUsed    bool          `json:"proxy_used,omitempty"`
	ProxySource  string        `json:"proxy_source,omitempty"`
	ProxyScheme  string        `json:"proxy_scheme,omitempty"`
	ProxyTarget  string        `json:"proxy_target,omitempty"`
	ProxyStatus  int           `json:"proxy_status,omitempty"`
	Msg          string        `json:"msg,omitempty"`
}

type RelayLog struct {
	ID                    int64            `json:"id" gorm:"primaryKey;autoIncrement:false"`
	UserID                int              `json:"user_id" gorm:"index;default:0"`
	APIKeyID              int              `json:"api_key_id" gorm:"index;default:0"`
	RequestIP             string           `json:"request_ip,omitempty" gorm:"size:128;index"`
	Time                  int64            `json:"time" gorm:"index"`
	RequestEndpoint       string           `json:"request_endpoint" gorm:"size:64;index"`
	RequestPath           string           `json:"request_path" gorm:"size:256"`
	RequestModelName      string           `json:"request_model_name"`
	RequestAPIKeyName     string           `json:"request_api_key_name"`
	UserName              string           `json:"user_name"`
	ReasoningEffort       string           `json:"reasoning_effort" gorm:"size:32"`
	ChannelId             int              `json:"channel"`
	ChannelName           string           `json:"channel_name"`
	ChannelKeyRemark      string           `json:"channel_key_remark"`
	ActualModelName       string           `json:"actual_model_name"`
	InputTokens           int              `json:"input_tokens"`
	OutputTokens          int              `json:"output_tokens"`
	CacheHitTokens        int              `json:"cache_hit_tokens"`
	CacheWriteTokens      int              `json:"cache_write_tokens"`
	CacheWrite5mTokens    int              `json:"cache_write_5m_tokens,omitempty"` // 5 分钟 TTL 缓存写入(Anthropic ephemeral_5m_input_tokens), CacheWriteTokens 的子集
	CacheWrite1hTokens    int              `json:"cache_write_1h_tokens,omitempty"` // 1 小时 TTL 缓存写入(Anthropic ephemeral_1h_input_tokens), 计费高于 5m
	CacheInputTokens      int              `json:"cache_input_tokens"`
	CacheHitRate          float64          `json:"cache_hit_rate" gorm:"type:real"`
	Ftut                  int              `json:"ftut"`
	UseTime               int              `json:"use_time"`
	Cost                  float64          `json:"cost"`
	RequestContent        string           `json:"request_content"`
	ResponseContent       string           `json:"response_content"`
	Error                 string           `json:"error"`
	ErrorCode             string           `json:"error_code"`
	ErrorStatus           int              `json:"error_status"`
	ErrorStrategy         string           `json:"error_strategy"`
	Attempts              []ChannelAttempt `json:"attempts" gorm:"serializer:json"`
	TotalAttempts         int              `json:"total_attempts"`
	SessionKey            string           `json:"session_key,omitempty" gorm:"size:96;index"`
	SessionSource         string           `json:"session_source,omitempty" gorm:"size:64"`
	RouteStickyHit        bool             `json:"route_sticky_hit,omitempty"`
	IsStream              bool             `json:"is_stream"`
	UsageSource           string           `json:"usage_source,omitempty" gorm:"size:64"`
	UsageMissingReason    string           `json:"usage_missing_reason,omitempty" gorm:"size:128"`
	AccessPlanID          int              `json:"access_plan_id"`
	PromptOverrideMode    string           `json:"prompt_override_mode"`
	PromptOverrideSources []string         `json:"prompt_override_sources" gorm:"serializer:json"`
	AccessPlanSlug        string           `json:"access_plan_slug"`
	AccessPlanName        string           `json:"access_plan_name"`
	RouteProfileID        int              `json:"route_profile_id"`
	RouteProfileName      string           `json:"route_profile_name"`
	BillingProfileID      int              `json:"billing_profile_id"`
	BillingProfileName    string           `json:"billing_profile_name"`
	BillingModel          string           `json:"billing_model"`
	BaseInputPrice        float64          `json:"base_input_price" gorm:"type:real"`
	BaseOutputPrice       float64          `json:"base_output_price" gorm:"type:real"`
	BaseCacheReadPrice    float64          `json:"base_cache_read_price" gorm:"type:real"`
	BaseCacheWritePrice   float64          `json:"base_cache_write_price" gorm:"type:real"`
	DefaultMultiplier     float64          `json:"default_multiplier" gorm:"type:real"`
	ModelMultiplier       float64          `json:"model_multiplier" gorm:"type:real"`
	FinalMultiplier       float64          `json:"final_multiplier" gorm:"type:real"`
	FinalInputCost        float64          `json:"final_input_cost" gorm:"type:real"`
	FinalOutputCost       float64          `json:"final_output_cost" gorm:"type:real"`
	FinalCacheReadCost    float64          `json:"final_cache_read_cost" gorm:"type:real"`
	FinalCacheWriteCost   float64          `json:"final_cache_write_cost" gorm:"type:real"`
}

type RelayLogUserSummary struct {
	ID                int64  `json:"id"`
	UserID            int    `json:"user_id"`
	APIKeyID          int    `json:"api_key_id"`
	Time              int64  `json:"time"`
	RequestEndpoint   string `json:"request_endpoint"`
	RequestPath       string `json:"request_path"`
	RequestModelName  string `json:"request_model_name"`
	RequestAPIKeyName string `json:"request_api_key_name"`
	// Channel identity is deliberately omitted from the user-facing summary so a
	// normal user cannot see which upstream channel served them (only the model).
	ActualModelName    string  `json:"actual_model_name"`
	InputTokens        int     `json:"input_tokens"`
	OutputTokens       int     `json:"output_tokens"`
	CacheHitTokens     int     `json:"cache_hit_tokens"`
	CacheWriteTokens   int     `json:"cache_write_tokens"`
	CacheInputTokens   int     `json:"cache_input_tokens"`
	CacheHitRate       float64 `json:"cache_hit_rate"`
	Ftut               int     `json:"ftut"`
	UseTime            int     `json:"use_time"`
	Cost               float64 `json:"cost"`
	ErrorCode          string  `json:"error_code"`
	ErrorStatus        int     `json:"error_status"`
	TotalAttempts      int     `json:"total_attempts"`
	SessionKey         string  `json:"session_key,omitempty"`
	SessionSource      string  `json:"session_source,omitempty"`
	RouteStickyHit     bool    `json:"route_sticky_hit,omitempty"`
	IsStream           bool    `json:"is_stream,omitempty"`
	UsageSource        string  `json:"usage_source,omitempty"`
	UsageMissingReason string  `json:"usage_missing_reason,omitempty"`
	AccessPlanSlug     string  `json:"access_plan_slug"`
	AccessPlanName     string  `json:"access_plan_name"`
	BillingModel       string  `json:"billing_model"`
	BaseInputPrice     float64 `json:"base_input_price,omitempty"`
	BaseOutputPrice    float64 `json:"base_output_price,omitempty"`
}

type RelayLogStorage struct {
	StoredBytes int64   `json:"stored_bytes"`
	MaxBytes    int64   `json:"max_bytes"`
	MaxGB       float64 `json:"max_gb"`
}

type RelayLogScope struct {
	UserID   int
	APIKeyID int
	Endpoint string
	Provider string
	Model    string
	// Severity narrows to a single severity bucket: "success" | "warn" | "error".
	// Empty means all. Kept in lockstep with the SQL / Go / web severity rules.
	Severity string
	// RetriedOnly narrows to requests that took more than one channel attempt
	// (a retry / failover happened), regardless of final outcome — spans both the
	// "warn" bucket (recovered) and multi-attempt errors. Orthogonal to Severity.
	RetriedOnly bool
	// HideModelTest, when true, excludes channel-test probe rows (request_endpoint
	// "model_test*") from the list. A repeat/stress test — or an upstream capacity
	// bad-window — can emit many test-probe failures; this filter keeps them from
	// drowning out real traffic in the log view. Orthogonal to every other field.
	HideModelTest bool
	// Search filters by user_name, request_api_key_name, request_model_name,
	// actual_model_name, channel_name, request_endpoint, request_path,
	// session_key, error, and error_code case-insensitively, or numeric log/channel id.
	Search string
	Redact bool
}

// RelayLogSeverityCounts is the global severity breakdown for the current filter,
// used to render the log page's 全部 / 成功 / Warn / Error badges with real totals
// (not just the current page). Total == Success + Warn + Error.
type RelayLogSeverityCounts struct {
	Total   int64 `json:"total"`
	Success int64 `json:"success"`
	Warn    int64 `json:"warn"`
	Error   int64 `json:"error"`
}
