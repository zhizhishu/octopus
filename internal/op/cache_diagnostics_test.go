package op

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func TestCacheDiagnosticsHighlightsMissingPromptCacheKeyAndAnthropicCandidates(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()
	longPrompt := strings.Repeat("stable policy ", 420)

	logs := []model.RelayLog{
		{
			ID:               6101,
			Time:             now - 30,
			UserID:           7,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-cache",
			InputTokens:      100,
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-cache","messages":[{"role":"system","content":"` + longPrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6102,
			Time:             now - 20,
			UserID:           7,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-cache",
			InputTokens:      100,
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-cache","messages":[{"role":"system","content":"` + longPrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6103,
			Time:             now - 10,
			UserID:           7,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-cache",
			InputTokens:      100,
			CacheHitTokens:   30,
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-cache","prompt_cache_key":"stable-key","messages":[{"role":"system","content":"` + longPrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6104,
			Time:             now,
			UserID:           8,
			RequestEndpoint:  "messages",
			RequestPath:      "/v1/messages",
			RequestModelName: "claude-cache",
			InputTokens:      150,
			CacheInputTokens: 150,
			RequestContent:   `{"model":"claude-cache","messages":[{"role":"user","content":"` + longPrompt + `"}]}`,
		},
		{
			ID:               6105,
			Time:             now,
			UserID:           8,
			RequestPath:      "/v1/messages",
			RequestModelName: "claude-cache",
			InputTokens:      150,
			CacheInputTokens: 150,
			RequestContent:   `{"model":"claude-cache","system":"` + longPrompt + `","messages":[{"role":"user","content":"hello"}]}`,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	diagnostics, err := CacheDiagnosticsGet(context.Background())
	if err != nil {
		t.Fatalf("cache diagnostics: %v", err)
	}
	if diagnostics.Summary.RequestCount != 5 {
		t.Fatalf("expected 5 requests, got %#v", diagnostics.Summary)
	}
	if diagnostics.Summary.MissingPromptCacheKeyRequestCount != 2 {
		t.Fatalf("expected two missing prompt cache keys, got %#v", diagnostics.Summary)
	}
	if diagnostics.Summary.AnthropicCacheControlCandidates != 2 {
		t.Fatalf("expected two Anthropic cache_control candidates, got %#v", diagnostics.Summary)
	}

	gpt := findCacheDiagnosticBucket(t, diagnostics.ByModel, "gpt-cache")
	if gpt.CacheableRequestCount != 3 || gpt.MissingPromptCacheKeyRequestCount != 2 || gpt.PromptCacheKeyRequestCount != 1 {
		t.Fatalf("unexpected gpt bucket: %#v", gpt)
	}
	if gpt.CacheHitRate != 0.1 {
		t.Fatalf("expected gpt cache hit rate 30/300, got %f", gpt.CacheHitRate)
	}
	claude := findCacheDiagnosticBucket(t, diagnostics.ByModel, "claude-cache")
	if claude.AnthropicCacheControlCandidates != 2 {
		t.Fatalf("expected anthropic candidate bucket, got %#v", claude)
	}
	if !hasCacheDiagnosticIssue(diagnostics.Issues, "missing_prompt_cache_key", "model", "gpt-cache") {
		t.Fatalf("expected missing prompt cache key issue, got %#v", diagnostics.Issues)
	}
	if !hasCacheDiagnosticIssue(diagnostics.Issues, "anthropic_cache_control_candidate", "model", "claude-cache") {
		t.Fatalf("expected anthropic candidate issue, got %#v", diagnostics.Issues)
	}
}

func TestCacheDiagnosticsIncludesRecentRelayLogCache(t *testing.T) {
	setupRelayLogTest(t)

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, model.RelayLog{
		ID:               6201,
		Time:             time.Now().Unix(),
		UserID:           9,
		RequestEndpoint:  "responses",
		RequestPath:      "/v1/responses",
		RequestModelName: "gpt-recent",
		InputTokens:      80,
		CacheHitTokens:   40,
		CacheInputTokens: 80,
		RequestContent:   `{"model":"gpt-recent","prompt_cache_key":"recent-key","input":"` + strings.Repeat("recent ", 200) + `"}`,
	})
	relayLogCacheLock.Unlock()

	diagnostics, err := CacheDiagnosticsGet(context.Background())
	if err != nil {
		t.Fatalf("cache diagnostics: %v", err)
	}
	got := findCacheDiagnosticBucket(t, diagnostics.ByModel, "gpt-recent")
	if got.CacheHitToken != 40 || got.CacheInputToken != 80 || got.PromptCacheKeyRequestCount != 1 {
		t.Fatalf("expected cached recent log in diagnostics, got %#v", got)
	}
}

func TestCacheDiagnosticsExcludesPersistedLocalValidationLogs(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()
	longPrompt := strings.Repeat("local validation ", 420)

	logs := []model.RelayLog{
		{
			ID:               6251,
			Time:             now,
			UserID:           9,
			RequestEndpoint:  "messages",
			RequestPath:      "/v1/messages",
			RequestModelName: "claude-cache",
			InputTokens:      100,
			CacheInputTokens: 100,
			RequestContent:   `{"model":"claude-cache","messages":[{"role":"user","content":"` + longPrompt + `"}]}`,
			ErrorCode:        model.RelayLogErrorCodeClientEmptyRequest,
			ErrorStrategy:    model.RelayLogErrorStrategyLocalValidation,
		},
		{
			ID:               6252,
			Time:             now,
			UserID:           9,
			RequestEndpoint:  "messages",
			RequestPath:      "/v1/messages",
			RequestModelName: "claude-cache",
			InputTokens:      100,
			CacheInputTokens: 100,
			RequestContent:   `{"model":"claude-cache","messages":[]}`,
			ErrorCode:        model.RelayLogErrorCodeCursorEmptyProbe,
			ErrorStrategy:    model.RelayLogErrorStrategyLocalCursorProbe,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	diagnostics, err := CacheDiagnosticsGet(context.Background())
	if err != nil {
		t.Fatalf("cache diagnostics: %v", err)
	}
	if diagnostics.Summary.RequestCount != 0 {
		t.Fatalf("local validation logs must not affect cache diagnostics, got %#v", diagnostics.Summary)
	}
}

func TestCacheDiagnosticsUnstablePromptCacheKeyIsUserScoped(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()
	stablePrompt := strings.Repeat("shared policy ", 120)

	logs := []model.RelayLog{
		{
			ID:               6301,
			Time:             now - 40,
			UserID:           21,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-shared",
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-shared","prompt_cache_key":"user-21-key","messages":[{"role":"system","content":"` + stablePrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6302,
			Time:             now - 30,
			UserID:           22,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-shared",
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-shared","prompt_cache_key":"user-22-key","messages":[{"role":"system","content":"` + stablePrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6303,
			Time:             now - 20,
			UserID:           23,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-shared",
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-shared","prompt_cache_key":"rotating-key-a","messages":[{"role":"system","content":"` + stablePrompt + `"},{"role":"user","content":"hello"}]}`,
		},
		{
			ID:               6304,
			Time:             now - 10,
			UserID:           23,
			RequestEndpoint:  "chat",
			RequestPath:      "/v1/chat/completions",
			RequestModelName: "gpt-shared",
			CacheInputTokens: 100,
			RequestContent:   `{"model":"gpt-shared","prompt_cache_key":"rotating-key-b","messages":[{"role":"system","content":"` + stablePrompt + `"},{"role":"user","content":"hello"}]}`,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	diagnostics, err := CacheDiagnosticsGet(context.Background())
	if err != nil {
		t.Fatalf("cache diagnostics: %v", err)
	}
	if hasCacheDiagnosticIssue(diagnostics.Issues, "unstable_prompt_cache_key", "model", "gpt-shared") {
		t.Fatalf("model-level user-isolated cache keys should not be flagged: %#v", diagnostics.Issues)
	}
	if hasCacheDiagnosticIssue(diagnostics.Issues, "unstable_prompt_cache_key", "endpoint", "chat") {
		t.Fatalf("endpoint-level user-isolated cache keys should not be flagged: %#v", diagnostics.Issues)
	}
	if !hasCacheDiagnosticIssue(diagnostics.Issues, "unstable_prompt_cache_key", "user", "user:23") {
		t.Fatalf("expected same-user unstable prompt cache key issue, got %#v", diagnostics.Issues)
	}
}

func findCacheDiagnosticBucket(t *testing.T, buckets []model.CacheDiagnosticBucket, key string) model.CacheDiagnosticBucket {
	t.Helper()
	for _, bucket := range buckets {
		if bucket.Key == key {
			return bucket
		}
	}
	t.Fatalf("bucket %s not found in %#v", key, buckets)
	return model.CacheDiagnosticBucket{}
}

func hasCacheDiagnosticIssue(issues []model.CacheDiagnosticIssue, code, scope, key string) bool {
	for _, issue := range issues {
		if issue.Code == code && issue.Scope == scope && issue.Key == key {
			return true
		}
	}
	return false
}
