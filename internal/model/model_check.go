package model

type ModelTestRequest struct {
	Model          string   `json:"model,omitempty"`
	Models         []string `json:"models,omitempty"`
	ChannelID      int      `json:"channel_id,omitempty"`
	AccessPlanSlug string   `json:"access_plan_slug,omitempty"`
	Endpoint       string   `json:"endpoint,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Stream         *bool    `json:"stream,omitempty"`
	Concurrency    int      `json:"concurrency,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	UserID         int      `json:"user_id,omitempty"`
	APIKeyID       int      `json:"api_key_id,omitempty"`
	AuditLog       bool     `json:"audit_log,omitempty"`
}

type ChannelTestRequest struct {
	Channel        Channel `json:"channel"`
	Model          string  `json:"model,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"`
	Prompt         string  `json:"prompt,omitempty"`
	Stream         *bool   `json:"stream,omitempty"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
}

type ModelTestSummary struct {
	Total      int `json:"total"`
	Success    int `json:"success"`
	Failed     int `json:"failed"`
	DurationMs int `json:"duration_ms"`
}

type ModelTestResponse struct {
	Summary ModelTestSummary  `json:"summary"`
	Results []ModelTestResult `json:"results"`
}

type ModelTestResult struct {
	Model             string           `json:"model"`
	RequestModel      string           `json:"request_model"`
	UpstreamModel     string           `json:"upstream_model,omitempty"`
	AccessPlanSlug    string           `json:"access_plan_slug,omitempty"`
	AccessPlanName    string           `json:"access_plan_name,omitempty"`
	AccessPlanID      int              `json:"access_plan_id,omitempty"`
	RequestEndpoint   string           `json:"request_endpoint,omitempty"`
	RequestPath       string           `json:"request_path,omitempty"`
	UpstreamPath      string           `json:"upstream_path,omitempty"`
	IsStream          bool             `json:"is_stream"`
	RouteUsed         bool             `json:"route_used"`
	RouteFallbackUsed bool             `json:"route_fallback_used,omitempty"`
	GroupName         string           `json:"group_name,omitempty"`
	ChannelID         int              `json:"channel_id,omitempty"`
	ChannelName       string           `json:"channel_name,omitempty"`
	ChannelKeyID      int              `json:"channel_key_id,omitempty"`
	StatusCode        int              `json:"status_code,omitempty"`
	Success           bool             `json:"success"`
	DurationMs        int              `json:"duration_ms"`
	Error             string           `json:"error,omitempty"`
	ErrorCode         string           `json:"error_code,omitempty"`
	ResponsePreview   string           `json:"response_preview,omitempty"`
	ProxyUsed         bool             `json:"proxy_used,omitempty"`
	ProxySource       string           `json:"proxy_source,omitempty"`
	ProxyScheme       string           `json:"proxy_scheme,omitempty"`
	ProxyTarget       string           `json:"proxy_target,omitempty"`
	ProxyStatus       int              `json:"proxy_status,omitempty"`
	InputTokens       int64            `json:"input_tokens,omitempty"`
	OutputTokens      int64            `json:"output_tokens,omitempty"`
	Attempts          []ChannelAttempt `json:"attempts,omitempty"`
}
