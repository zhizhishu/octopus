package op

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

const (
	cacheDiagnosticLowRateThreshold      = 0.20
	cacheDiagnosticMinBucketRequests     = 3
	cacheDiagnosticMinCacheableTextChars = 1024
	cacheDiagnosticAnthropicLongChars    = 4096
)

type cacheDiagnosticAccumulator struct {
	key                               string
	requestCount                      int64
	cacheableRequestCount             int64
	promptCacheKeyRequestCount        int64
	missingPromptCacheKeyRequestCount int64
	anthropicCacheControlCandidates   int64
	cacheHitToken                     int64
	cacheWriteToken                   int64
	cacheInputToken                   int64
	promptCacheKeys                   map[string]struct{}
	stableAnchors                     map[string]struct{}
	recommendations                   map[string]struct{}
}

type cacheDiagnosticLogMeta struct {
	promptCacheKey           string
	hasPromptCacheKey        bool
	promptCacheKeyApplicable bool
	cacheable                bool
	anthropicCandidate       bool
	stableAnchor             string
}

func CacheDiagnosticsGet(ctx context.Context) (model.CacheDiagnostics, error) {
	logs, err := cacheDiagnosticLogs(ctx)
	if err != nil {
		return model.CacheDiagnostics{}, err
	}

	byModel := map[string]*cacheDiagnosticAccumulator{}
	byUser := map[string]*cacheDiagnosticAccumulator{}
	byEndpoint := map[string]*cacheDiagnosticAccumulator{}
	var total cacheDiagnosticAccumulator
	total.promptCacheKeys = map[string]struct{}{}
	total.stableAnchors = map[string]struct{}{}
	total.recommendations = map[string]struct{}{}

	for _, relayLog := range logs {
		if relayLogClientEmptyRequest(relayLog) {
			continue
		}
		meta := cacheDiagnosticMeta(relayLog)
		cacheDiagnosticAdd(&total, relayLog, meta)
		cacheDiagnosticAdd(bucketAccumulator(byModel, modelCacheDiagnosticKey(relayLog)), relayLog, meta)
		cacheDiagnosticAdd(bucketAccumulator(byUser, userCacheDiagnosticKey(relayLog)), relayLog, meta)
		cacheDiagnosticAdd(bucketAccumulator(byEndpoint, endpointCacheDiagnosticKey(relayLog)), relayLog, meta)
	}

	modelBuckets := cacheDiagnosticBuckets(byModel)
	userBuckets := cacheDiagnosticBuckets(byUser)
	endpointBuckets := cacheDiagnosticBuckets(byEndpoint)
	issues := make([]model.CacheDiagnosticIssue, 0)
	issues = append(issues, cacheDiagnosticIssues("model", modelBuckets)...)
	issues = append(issues, cacheDiagnosticIssues("user", userBuckets)...)
	issues = append(issues, cacheDiagnosticIssues("endpoint", endpointBuckets)...)
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Severity == issues[j].Severity {
			if issues[i].AffectedCount == issues[j].AffectedCount {
				return issues[i].Key < issues[j].Key
			}
			return issues[i].AffectedCount > issues[j].AffectedCount
		}
		return issues[i].Severity < issues[j].Severity
	})

	summaryBucket := cacheDiagnosticBucket(&total)
	return model.CacheDiagnostics{
		GeneratedAt: time.Now().Unix(),
		Summary: model.CacheDiagnosticSummary{
			RequestCount:                      summaryBucket.RequestCount,
			CacheableRequestCount:             summaryBucket.CacheableRequestCount,
			PromptCacheKeyRequestCount:        summaryBucket.PromptCacheKeyRequestCount,
			MissingPromptCacheKeyRequestCount: summaryBucket.MissingPromptCacheKeyRequestCount,
			AnthropicCacheControlCandidates:   summaryBucket.AnthropicCacheControlCandidates,
			CacheHitToken:                     summaryBucket.CacheHitToken,
			CacheWriteToken:                   summaryBucket.CacheWriteToken,
			CacheInputToken:                   summaryBucket.CacheInputToken,
			CacheHitRate:                      summaryBucket.CacheHitRate,
			LowCacheRateBucketCount:           countCacheDiagnosticIssues(issues, "low_cache_hit_rate"),
			UnstableCacheKeyBucketCount:       countCacheDiagnosticIssues(issues, "unstable_prompt_cache_key"),
		},
		ByModel:    modelBuckets,
		ByUser:     userBuckets,
		ByEndpoint: endpointBuckets,
		Issues:     issues,
	}, nil
}

func cacheDiagnosticLogs(ctx context.Context) ([]model.RelayLog, error) {
	var dbLogs []model.RelayLog
	err := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select("id", "time", "user_id", "api_key_id", "request_endpoint", "request_path", "request_model_name", "actual_model_name", "input_tokens", "cache_hit_tokens", "cache_write_tokens", "cache_input_tokens", "request_content", "error_code", "error_strategy").
		Find(&dbLogs).Error
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]struct{}, len(dbLogs))
	logs := make([]model.RelayLog, 0, len(dbLogs)+len(relayLogCache))
	for _, relayLog := range dbLogs {
		seen[relayLog.ID] = struct{}{}
		logs = append(logs, relayLog)
	}

	relayLogCacheLock.Lock()
	cachedLogs := make([]model.RelayLog, len(relayLogCache))
	copy(cachedLogs, relayLogCache)
	relayLogCacheLock.Unlock()
	for _, relayLog := range cachedLogs {
		if _, ok := seen[relayLog.ID]; ok {
			continue
		}
		logs = append(logs, relayLog)
	}
	return logs, nil
}

func bucketAccumulator(buckets map[string]*cacheDiagnosticAccumulator, key string) *cacheDiagnosticAccumulator {
	acc := buckets[key]
	if acc == nil {
		acc = &cacheDiagnosticAccumulator{
			key:             key,
			promptCacheKeys: map[string]struct{}{},
			stableAnchors:   map[string]struct{}{},
			recommendations: map[string]struct{}{},
		}
		buckets[key] = acc
	}
	return acc
}

func cacheDiagnosticAdd(acc *cacheDiagnosticAccumulator, relayLog model.RelayLog, meta cacheDiagnosticLogMeta) {
	if acc.promptCacheKeys == nil {
		acc.promptCacheKeys = map[string]struct{}{}
	}
	if acc.stableAnchors == nil {
		acc.stableAnchors = map[string]struct{}{}
	}
	if acc.recommendations == nil {
		acc.recommendations = map[string]struct{}{}
	}

	acc.requestCount++
	acc.cacheHitToken += int64(relayLog.CacheHitTokens)
	acc.cacheWriteToken += int64(relayLog.CacheWriteTokens)
	acc.cacheInputToken += relayLogCacheRateBase(relayLog)
	if meta.cacheable {
		acc.cacheableRequestCount++
	}
	if meta.hasPromptCacheKey {
		acc.promptCacheKeyRequestCount++
		acc.promptCacheKeys[meta.promptCacheKey] = struct{}{}
	}
	if meta.cacheable && meta.promptCacheKeyApplicable && !meta.hasPromptCacheKey {
		acc.missingPromptCacheKeyRequestCount++
		acc.recommendations["enable or preserve prompt_cache_key for stable OpenAI-compatible prompts"] = struct{}{}
	}
	if meta.anthropicCandidate {
		acc.anthropicCacheControlCandidates++
		acc.recommendations["enable conservative Anthropic cache_control for long stable prefixes"] = struct{}{}
	}
	if meta.stableAnchor != "" {
		acc.stableAnchors[meta.stableAnchor] = struct{}{}
	}
}

func cacheDiagnosticMeta(relayLog model.RelayLog) cacheDiagnosticLogMeta {
	var root map[string]any
	if err := json.Unmarshal([]byte(relayLog.RequestContent), &root); err != nil {
		return cacheDiagnosticLogMeta{}
	}

	promptCacheKey := jsonString(root["prompt_cache_key"])
	hasPromptCacheKey := strings.TrimSpace(promptCacheKey) != ""
	textChars := requestTextChars(root["messages"]) +
		requestTextChars(root["input"]) +
		requestTextChars(root["contents"]) +
		requestTextChars(root["system"]) +
		requestTextChars(root["system_instruction"]) +
		requestTextChars(root["tools"])
	hasCacheControl := containsKeyRecursive(root, "cache_control")
	hasPromptCacheRetention := strings.TrimSpace(jsonString(root["prompt_cache_retention"])) != ""
	cacheable := textChars >= cacheDiagnosticMinCacheableTextChars || hasPromptCacheKey || hasPromptCacheRetention || hasCacheControl || relayLog.CacheInputTokens > 0 || relayLog.CacheHitTokens > 0 || relayLog.CacheWriteTokens > 0
	anthropicCandidate := cacheDiagnosticAnthropicEndpoint(relayLog) && !hasCacheControl && textChars >= cacheDiagnosticAnthropicLongChars

	return cacheDiagnosticLogMeta{
		promptCacheKey:           promptCacheKey,
		hasPromptCacheKey:        hasPromptCacheKey,
		promptCacheKeyApplicable: cacheDiagnosticPromptCacheKeyApplicable(relayLog, hasPromptCacheKey, hasPromptCacheRetention),
		cacheable:                cacheable,
		anthropicCandidate:       anthropicCandidate,
		stableAnchor:             stableRequestAnchor(root),
	}
}

func cacheDiagnosticAnthropicEndpoint(relayLog model.RelayLog) bool {
	endpoint := strings.ToLower(strings.TrimSpace(relayLog.RequestEndpoint))
	if strings.Contains(endpoint, "messages") {
		return true
	}
	path := strings.ToLower(strings.TrimSpace(relayLog.RequestPath))
	return path == "/v1/messages" || strings.HasSuffix(path, "/messages")
}

func cacheDiagnosticPromptCacheKeyApplicable(relayLog model.RelayLog, hasPromptCacheKey, hasPromptCacheRetention bool) bool {
	if hasPromptCacheKey || hasPromptCacheRetention {
		return true
	}
	endpoint := strings.ToLower(strings.TrimSpace(relayLog.RequestEndpoint))
	switch endpoint {
	case "chat", "responses":
		return true
	}
	path := strings.ToLower(strings.TrimSpace(relayLog.RequestPath))
	return strings.HasPrefix(path, "/v1/chat/completions") || path == "/v1/responses"
}

func cacheDiagnosticBuckets(accs map[string]*cacheDiagnosticAccumulator) []model.CacheDiagnosticBucket {
	out := make([]model.CacheDiagnosticBucket, 0, len(accs))
	for _, acc := range accs {
		out = append(out, cacheDiagnosticBucket(acc))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RequestCount == out[j].RequestCount {
			return out[i].Key < out[j].Key
		}
		return out[i].RequestCount > out[j].RequestCount
	})
	return out
}

func cacheDiagnosticBucket(acc *cacheDiagnosticAccumulator) model.CacheDiagnosticBucket {
	bucket := model.CacheDiagnosticBucket{
		Key:                               acc.key,
		RequestCount:                      acc.requestCount,
		CacheableRequestCount:             acc.cacheableRequestCount,
		PromptCacheKeyRequestCount:        acc.promptCacheKeyRequestCount,
		MissingPromptCacheKeyRequestCount: acc.missingPromptCacheKeyRequestCount,
		AnthropicCacheControlCandidates:   acc.anthropicCacheControlCandidates,
		CacheHitToken:                     acc.cacheHitToken,
		CacheWriteToken:                   acc.cacheWriteToken,
		CacheInputToken:                   acc.cacheInputToken,
		DistinctPromptCacheKeys:           len(acc.promptCacheKeys),
		DistinctStableAnchors:             len(acc.stableAnchors),
	}
	if acc.cacheHitToken > 0 && acc.cacheInputToken > 0 {
		bucket.CacheHitRate = float64(acc.cacheHitToken) / float64(acc.cacheInputToken)
	}
	if len(acc.recommendations) > 0 {
		bucket.Recommendations = sortedSet(acc.recommendations)
	}
	return bucket
}

func cacheDiagnosticIssues(scope string, buckets []model.CacheDiagnosticBucket) []model.CacheDiagnosticIssue {
	issues := make([]model.CacheDiagnosticIssue, 0)
	for _, bucket := range buckets {
		if bucket.CacheableRequestCount >= cacheDiagnosticMinBucketRequests && bucket.MissingPromptCacheKeyRequestCount > 0 {
			issues = append(issues, model.CacheDiagnosticIssue{
				Code:           "missing_prompt_cache_key",
				Severity:       "P1",
				Scope:          scope,
				Key:            bucket.Key,
				RequestCount:   bucket.RequestCount,
				AffectedCount:  bucket.MissingPromptCacheKeyRequestCount,
				Recommendation: "Keep explicit prompt_cache_key from clients, or rely on Octopus auto prompt-cache-key for stable OpenAI Chat/Responses requests.",
			})
		}
		if bucket.CacheableRequestCount >= cacheDiagnosticMinBucketRequests && bucket.CacheHitToken == 0 {
			issues = append(issues, model.CacheDiagnosticIssue{
				Code:           "zero_cache_hits",
				Severity:       "P1",
				Scope:          scope,
				Key:            bucket.Key,
				RequestCount:   bucket.RequestCount,
				AffectedCount:  bucket.CacheableRequestCount,
				Recommendation: "Check whether upstream supports prompt caching, whether prompt anchors are stable, and whether cache telemetry fields are returned.",
			})
		} else if bucket.CacheableRequestCount >= cacheDiagnosticMinBucketRequests && bucket.CacheHitRate > 0 && bucket.CacheHitRate < cacheDiagnosticLowRateThreshold {
			issues = append(issues, model.CacheDiagnosticIssue{
				Code:           "low_cache_hit_rate",
				Severity:       "P2",
				Scope:          scope,
				Key:            bucket.Key,
				RequestCount:   bucket.RequestCount,
				AffectedCount:  bucket.CacheableRequestCount,
				Recommendation: "Inspect changing system/developer prompts, first user turns, tools, and routed model names for cache-key instability.",
			})
		}
		if scope == "user" && bucket.DistinctPromptCacheKeys > 1 && bucket.DistinctStableAnchors == 1 {
			issues = append(issues, model.CacheDiagnosticIssue{
				Code:           "unstable_prompt_cache_key",
				Severity:       "P2",
				Scope:          scope,
				Key:            bucket.Key,
				RequestCount:   bucket.RequestCount,
				AffectedCount:  int64(bucket.DistinctPromptCacheKeys),
				Recommendation: "The stable prompt anchor looks reused but multiple prompt_cache_key values were observed; check client-side key generation.",
			})
		}
		if bucket.AnthropicCacheControlCandidates > 0 {
			issues = append(issues, model.CacheDiagnosticIssue{
				Code:           "anthropic_cache_control_candidate",
				Severity:       "P2",
				Scope:          scope,
				Key:            bucket.Key,
				RequestCount:   bucket.RequestCount,
				AffectedCount:  bucket.AnthropicCacheControlCandidates,
				Recommendation: "Long Anthropic Messages prompts without cache_control were seen; consider enabling conservative auto cache_control.",
			})
		}
	}
	return issues
}

func countCacheDiagnosticIssues(issues []model.CacheDiagnosticIssue, code string) int {
	var count int
	for _, issue := range issues {
		if issue.Code == code {
			count++
		}
	}
	return count
}

func modelCacheDiagnosticKey(relayLog model.RelayLog) string {
	if key := strings.TrimSpace(relayLog.RequestModelName); key != "" {
		return key
	}
	if key := strings.TrimSpace(relayLog.ActualModelName); key != "" {
		return key
	}
	return "unknown"
}

func userCacheDiagnosticKey(relayLog model.RelayLog) string {
	if relayLog.UserID > 0 {
		return fmt.Sprintf("user:%d", relayLog.UserID)
	}
	if relayLog.APIKeyID > 0 {
		return fmt.Sprintf("api_key:%d", relayLog.APIKeyID)
	}
	return "anonymous"
}

func endpointCacheDiagnosticKey(relayLog model.RelayLog) string {
	if key := strings.TrimSpace(relayLog.RequestEndpoint); key != "" {
		return key
	}
	if key := strings.TrimSpace(relayLog.RequestPath); key != "" {
		return key
	}
	return "unknown"
}

func jsonString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func requestTextChars(value any) int {
	switch v := value.(type) {
	case string:
		return len(strings.TrimSpace(v))
	case []any:
		total := 0
		for _, item := range v {
			total += requestTextChars(item)
		}
		return total
	case map[string]any:
		total := 0
		for key, item := range v {
			switch strings.ToLower(key) {
			case "content", "text", "input", "messages", "contents", "parts", "system", "system_instruction", "tools", "description":
				total += requestTextChars(item)
			}
		}
		return total
	default:
		return 0
	}
}

func containsKeyRecursive(value any, key string) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			if strings.EqualFold(k, key) {
				return true
			}
			if containsKeyRecursive(item, key) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsKeyRecursive(item, key) {
				return true
			}
		}
	}
	return false
}

func stableRequestAnchor(root map[string]any) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(jsonString(root["model"]))),
		firstStableText(root["system"]),
		firstStableText(root["system_instruction"]),
		firstStableText(root["tools"]),
		firstStableText(root["messages"]),
		firstStableText(root["input"]),
		firstStableText(root["contents"]),
	}
	joined := strings.TrimSpace(strings.Join(parts, "|"))
	if strings.Trim(joined, "|") == "" {
		return ""
	}
	sum := sha1.Sum([]byte(joined))
	return hex.EncodeToString(sum[:8])
}

func firstStableText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, item := range v {
			if text := firstStableText(item); text != "" {
				return text
			}
		}
	case map[string]any:
		for _, key := range []string{"content", "text", "input", "messages", "contents", "parts"} {
			if text := firstStableText(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func sortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
