package relay

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

func TestUsageCacheStatsOpenAIStyle(t *testing.T) {
	usage := &transformerModel.Usage{
		PromptTokens: 100,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 40,
		},
	}

	hit, write, input := usageCacheStats(usage)
	if hit != 40 || write != 0 || input != 100 {
		t.Fatalf("unexpected cache stats: hit=%d write=%d input=%d", hit, write, input)
	}
	if diff := math.Abs(cacheHitRate(hit, input) - 0.4); diff > 1e-9 {
		t.Fatalf("unexpected cache hit rate diff: %f", diff)
	}
}

func TestUsageCacheStatsAnthropicStyle(t *testing.T) {
	usage := &transformerModel.Usage{
		PromptTokens:             60,
		AnthropicUsage:           true,
		CacheCreationInputTokens: 10,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 30,
		},
	}

	hit, write, input := usageCacheStats(usage)
	if hit != 30 || write != 10 || input != 100 {
		t.Fatalf("unexpected cache stats: hit=%d write=%d input=%d", hit, write, input)
	}
	if diff := math.Abs(cacheHitRate(hit, input) - 0.3); diff > 1e-9 {
		t.Fatalf("unexpected cache hit rate diff: %f", diff)
	}
}

func TestUsageCacheStatsSeparateCacheInputAliases(t *testing.T) {
	usage := &transformerModel.Usage{
		PromptTokens:             60,
		SeparateCacheInputTokens: true,
		CacheCreationInputTokens: 10,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 30,
		},
	}

	hit, write, input := usageCacheStats(usage)
	if hit != 30 || write != 10 || input != 100 {
		t.Fatalf("unexpected cache stats: hit=%d write=%d input=%d", hit, write, input)
	}
	if diff := math.Abs(cacheHitRate(hit, input) - 0.3); diff > 1e-9 {
		t.Fatalf("unexpected cache hit rate diff: %f", diff)
	}
}

func TestMixedCacheUsageStatsAndBilling(t *testing.T) {
	const modelName = "mixed-cache-billing-test"
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{Input: 10, Output: 20, CacheRead: 2, CacheWrite: 15})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	for _, tc := range []struct {
		name, body        string
		hit, write, input int64
		uncachedCost      float64
	}{
		{"included DeepSeek cache", `{"prompt_tokens":5033,"completion_tokens":5,"total_tokens":5038,"cache_read_input_tokens":4864}`, 4864, 0, 5033, 1690e-6},
		{"included cache read and write", `{"prompt_tokens":100,"completion_tokens":5,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}`, 30, 10, 100, 600e-6},
		{"separate cache read and write", `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}`, 30, 5, 45, 100e-6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage transformerModel.Usage
			if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
				t.Fatal(err)
			}
			hit, write, input := usageCacheStats(&usage)
			if hit != tc.hit || write != tc.write || input != tc.input ||
				math.Abs(cacheHitRate(hit, input)-float64(tc.hit)/float64(tc.input)) > 1e-12 {
				t.Fatalf("unexpected cache stats: hit=%d write=%d input=%d", hit, write, input)
			}
			snapshot := (&RelayMetrics{RequestModel: modelName}).buildBillingSnapshot(modelName, &usage)
			if math.Abs(snapshot.FinalInputCost-tc.uncachedCost) > 1e-12 ||
				math.Abs(snapshot.FinalCacheReadCost-float64(tc.hit)*2e-6) > 1e-12 ||
				math.Abs(snapshot.FinalCacheWriteCost-float64(tc.write)*15e-6) > 1e-12 ||
				math.Abs(snapshot.FinalOutputCost-100e-6) > 1e-12 {
				t.Fatalf("unexpected billing: %+v", snapshot)
			}
		})
	}
}

func TestUsageCacheStatsClampsInputToCacheTokens(t *testing.T) {
	usage := &transformerModel.Usage{
		PromptTokens:             5,
		CacheCreationInputTokens: 3,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 10,
		},
	}

	hit, write, input := usageCacheStats(usage)
	if hit != 10 || write != 3 || input != 13 {
		t.Fatalf("unexpected cache stats: hit=%d write=%d input=%d", hit, write, input)
	}
}

func TestImagesUsageSeparateCacheInputAliases(t *testing.T) {
	usage := imagesUsage{
		InputTokens:              60,
		OutputTokens:             20,
		CacheReadInputTokens:     30,
		CacheCreationInputTokens: 10,
	}

	if got := usage.CacheReadTokenCount(); got != 30 {
		t.Fatalf("expected cache read tokens 30, got %d", got)
	}
	if got := usage.CacheWriteTokenCount(); got != 10 {
		t.Fatalf("expected cache write tokens 10, got %d", got)
	}
	if got := usage.CacheInputTokenCount(); got != 100 {
		t.Fatalf("expected cache input tokens 100, got %d", got)
	}
}

func TestFinalChannelKeepsCircuitBreakChannelVisible(t *testing.T) {
	channelID, channelName := finalChannel([]dbmodel.ChannelAttempt{
		{
			ChannelID:   4,
			ChannelName: "Claude-CPA",
			Status:      dbmodel.AttemptCircuitBreak,
			Msg:         "circuit breaker tripped, remaining cooldown: 30s",
		},
	})
	if channelID != 4 || channelName != "Claude-CPA" {
		t.Fatalf("expected circuit-break channel to stay visible, got %d(%s)", channelID, channelName)
	}
}

func TestRelayMetricsSavePersistsAfterRequestContextCanceled(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "metrics-user",
		Password: "secret",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  5,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apiKey := dbmodel.APIKey{
		UserID:  user.ID,
		Name:    "metrics-key",
		APIKey:  "sk-metrics",
		Enabled: true,
	}
	if err := op.APIKeyCreate(&apiKey, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	metrics := NewRelayMetrics(apiKey.ID, user.ID, "203.0.113.9", "request-model", nil)
	metrics.Stats.InputCost = 1.25
	metrics.Save(canceledCtx, true, nil, []dbmodel.ChannelAttempt{{
		ChannelID:   0,
		ChannelName: "",
		Status:      dbmodel.AttemptSuccess,
	}})

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].APIKeyID != apiKey.ID || logs[0].RequestAPIKeyName != apiKey.Name {
		t.Fatalf("expected relay log saved after canceled ctx, got %#v", logs)
	}
	updated, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if math.Abs(updated.Balance-3.75) > 1e-9 {
		t.Fatalf("expected balance deducted with detached ctx, got %f", updated.Balance)
	}
	if stats := op.StatsAPIKeyGet(apiKey.ID); stats.RequestSuccess != 1 {
		t.Fatalf("expected api key stats success, got %#v", stats)
	}
}

func TestRelayMetricsClientAbortDoesNotCountProviderFailure(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "abort-user",
		Password: "secret",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  5,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apiKey := dbmodel.APIKey{
		UserID:  user.ID,
		Name:    "abort-key",
		APIKey:  "sk-abort",
		Enabled: true,
	}
	if err := op.APIKeyCreate(&apiKey, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	metrics := NewRelayMetrics(apiKey.ID, user.ID, "203.0.113.10", "claude-opus-4-7[1m]", nil)
	metrics.Save(ctx, false, context.Canceled, []dbmodel.ChannelAttempt{{
		ChannelID:   4,
		ChannelName: "Claude-CPA",
		Status:      dbmodel.AttemptFailed,
		Msg:         "context canceled",
	}})

	if stats := op.StatsAPIKeyGet(apiKey.ID); stats.RequestFailed != 0 || stats.RequestSuccess != 0 {
		t.Fatalf("client abort should not count api key success/failure, got %#v", stats)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected one relay log, got %d", len(logs))
	}
	if logs[0].ErrorCode != "octopus_client_canceled" || logs[0].ErrorStatus != statusClientClosedRequest {
		t.Fatalf("expected client abort audit fields, got status=%d code=%q", logs[0].ErrorStatus, logs[0].ErrorCode)
	}
	if !strings.Contains(logs[0].ErrorStrategy, "breaker_counted=false") {
		t.Fatalf("expected breaker_counted=false strategy, got %q", logs[0].ErrorStrategy)
	}
}

func TestRelayMetricsAppliesAccessPlanMultipliers(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	if err := op.LLMCreate(dbmodel.LLMInfo{
		Name: "upstream-model",
		LLMPrice: dbmodel.LLMPrice{
			Input:     1,
			Output:    2,
			CacheRead: 0.5,
		},
	}, ctx); err != nil {
		t.Fatalf("create llm price: %v", err)
	}

	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var svip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}
	if svip.BillingProfile == nil {
		t.Fatalf("svip billing profile not loaded")
	}
	billingProfile := *svip.BillingProfile
	billingProfile.DefaultMultiplier = 2
	if err := op.AccessBillingProfileUpdate(&billingProfile, ctx); err != nil {
		t.Fatalf("update billing profile: %v", err)
	}
	if err := op.AccessBillingModelRuleCreate(&dbmodel.AccessBillingModelRule{
		BillingProfileID: billingProfile.ID,
		ModelName:        "upstream-model",
		Multiplier:       3,
		Enabled:          true,
	}, ctx); err != nil {
		t.Fatalf("create billing rule: %v", err)
	}

	plan, err := op.AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatalf("select plan: %v", err)
	}
	metrics := NewRelayMetrics(1, 1, "127.0.0.1", "request-model", nil)
	metrics.SetAccessPlan(plan, &dbmodel.AccessRouteRule{
		BillingModelSource: dbmodel.AccessBillingModelSourceUpstream,
	}, true)
	metrics.SetInternalResponse(&transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			PromptTokensDetails: &transformerModel.PromptTokensDetails{
				CachedTokens: 2,
			},
		},
	}, "upstream-model")

	expectedInput := (8*1 + 2*0.5) * 6 * 1e-6
	expectedOutput := 5 * 2 * 6 * 1e-6
	if math.Abs(metrics.Stats.InputCost-expectedInput) > 1e-12 {
		t.Fatalf("unexpected input cost: got %.12f want %.12f", metrics.Stats.InputCost, expectedInput)
	}
	if math.Abs(metrics.Stats.OutputCost-expectedOutput) > 1e-12 {
		t.Fatalf("unexpected output cost: got %.12f want %.12f", metrics.Stats.OutputCost, expectedOutput)
	}
	if metrics.BillingSnapshot.FinalMultiplier != 6 {
		t.Fatalf("expected final multiplier 6, got %v", metrics.BillingSnapshot.FinalMultiplier)
	}
	if metrics.BillingSnapshot.BillingModelName != "upstream-model" {
		t.Fatalf("expected upstream billing model, got %q", metrics.BillingSnapshot.BillingModelName)
	}
}

func TestRelayMetricsDefaultsBillingToRequestModel(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	if err := op.LLMCreate(dbmodel.LLMInfo{
		Name: "request-model",
		LLMPrice: dbmodel.LLMPrice{
			Input:  2,
			Output: 4,
		},
	}, ctx); err != nil {
		t.Fatalf("create request model price: %v", err)
	}
	if err := op.LLMCreate(dbmodel.LLMInfo{
		Name: "upstream-model",
		LLMPrice: dbmodel.LLMPrice{
			Input:  100,
			Output: 100,
		},
	}, ctx); err != nil {
		t.Fatalf("create upstream model price: %v", err)
	}

	metrics := NewRelayMetrics(1, 1, "127.0.0.1", "request-model", nil)
	metrics.SetInternalResponse(&transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
		},
	}, "upstream-model")

	if metrics.BillingSnapshot.BillingModelName != "request-model" {
		t.Fatalf("expected request model billing, got %q", metrics.BillingSnapshot.BillingModelName)
	}
	if metrics.BillingSnapshot.BillingModelSource != dbmodel.AccessBillingModelSourceRequest {
		t.Fatalf("expected request_model billing source, got %q", metrics.BillingSnapshot.BillingModelSource)
	}
	expectedInput := 10 * 2 * 1e-6
	expectedOutput := 5 * 4 * 1e-6
	if math.Abs(metrics.Stats.InputCost-expectedInput) > 1e-12 {
		t.Fatalf("unexpected input cost: got %.12f want %.12f", metrics.Stats.InputCost, expectedInput)
	}
	if math.Abs(metrics.Stats.OutputCost-expectedOutput) > 1e-12 {
		t.Fatalf("unexpected output cost: got %.12f want %.12f", metrics.Stats.OutputCost, expectedOutput)
	}

	imageMetrics := newImagesRelayMetrics(1, 1, "127.0.0.1", "request-model", "images_generations", "/v1/images/generations")
	imageMetrics.SetUsageFromImages("upstream-model", imagesUsage{InputTokens: 10, OutputTokens: 5})
	if imageMetrics.BillingSnapshot.BillingModelName != "request-model" {
		t.Fatalf("expected images request model billing, got %q", imageMetrics.BillingSnapshot.BillingModelName)
	}
	if imageMetrics.BillingSnapshot.BillingModelSource != dbmodel.AccessBillingModelSourceRequest {
		t.Fatalf("expected images request_model billing source, got %q", imageMetrics.BillingSnapshot.BillingModelSource)
	}
}
