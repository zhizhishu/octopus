package model

type CacheDiagnosticBucket struct {
	Key                               string   `json:"key"`
	RequestCount                      int64    `json:"request_count"`
	CacheableRequestCount             int64    `json:"cacheable_request_count"`
	PromptCacheKeyRequestCount        int64    `json:"prompt_cache_key_request_count"`
	MissingPromptCacheKeyRequestCount int64    `json:"missing_prompt_cache_key_request_count"`
	AnthropicCacheControlCandidates   int64    `json:"anthropic_cache_control_candidates"`
	CacheHitToken                     int64    `json:"cache_hit_token"`
	CacheWriteToken                   int64    `json:"cache_write_token"`
	CacheInputToken                   int64    `json:"cache_input_token"`
	CacheHitRate                      float64  `json:"cache_hit_rate"`
	DistinctPromptCacheKeys           int      `json:"distinct_prompt_cache_keys"`
	DistinctStableAnchors             int      `json:"distinct_stable_anchors"`
	Recommendations                   []string `json:"recommendations,omitempty"`
}

type CacheDiagnosticIssue struct {
	Code           string `json:"code"`
	Severity       string `json:"severity"`
	Scope          string `json:"scope"`
	Key            string `json:"key"`
	RequestCount   int64  `json:"request_count"`
	AffectedCount  int64  `json:"affected_count"`
	Recommendation string `json:"recommendation"`
}

type CacheDiagnosticSummary struct {
	RequestCount                      int64   `json:"request_count"`
	CacheableRequestCount             int64   `json:"cacheable_request_count"`
	PromptCacheKeyRequestCount        int64   `json:"prompt_cache_key_request_count"`
	MissingPromptCacheKeyRequestCount int64   `json:"missing_prompt_cache_key_request_count"`
	AnthropicCacheControlCandidates   int64   `json:"anthropic_cache_control_candidates"`
	CacheHitToken                     int64   `json:"cache_hit_token"`
	CacheWriteToken                   int64   `json:"cache_write_token"`
	CacheInputToken                   int64   `json:"cache_input_token"`
	CacheHitRate                      float64 `json:"cache_hit_rate"`
	LowCacheRateBucketCount           int     `json:"low_cache_rate_bucket_count"`
	UnstableCacheKeyBucketCount       int     `json:"unstable_cache_key_bucket_count"`
}

type CacheDiagnostics struct {
	GeneratedAt int64                   `json:"generated_at"`
	Summary     CacheDiagnosticSummary  `json:"summary"`
	ByModel     []CacheDiagnosticBucket `json:"by_model"`
	ByUser      []CacheDiagnosticBucket `json:"by_user"`
	ByEndpoint  []CacheDiagnosticBucket `json:"by_endpoint"`
	Issues      []CacheDiagnosticIssue  `json:"issues"`
}
