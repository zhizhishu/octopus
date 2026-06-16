package op

import (
	"math"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func TestModelRequestRankGroupsByRequestedModelIncludingCache(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()

	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:                 4001,
		Time:               now - 10,
		RequestModelName:   "glm-5.1",
		ActualModelName:    "claude-sonnet-4.5",
		InputTokens:        100,
		OutputTokens:       50,
		CacheHitTokens:     20,
		CacheInputTokens:   100,
		CacheWriteTokens:   5,
		Ftut:               300,
		UseTime:            1300,
		Cost:               0.12,
		FinalInputCost:     10,
		FinalOutputCost:    10,
		FinalCacheReadCost: 10,
	}).Error; err != nil {
		t.Fatalf("create log: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               4003,
		Time:             now,
		RequestModelName: "glm-5.1",
		ActualModelName:  "glm-5.1",
		Error:            "empty request: messages or input is required",
		ErrorCode:        model.RelayLogErrorCodeClientEmptyRequest,
		ErrorStatus:      400,
		ErrorStrategy:    model.RelayLogErrorStrategyLocalValidation,
	}).Error; err != nil {
		t.Fatalf("create client validation log: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               4004,
		Time:             now,
		RequestModelName: "glm-5.1",
		ActualModelName:  "glm-5.1",
		Error:            "channel Claude-CPA failed: context canceled",
		ErrorCode:        "octopus_client_canceled",
		ErrorStatus:      499,
		ErrorStrategy:    "client_canceled;upstream_forwarded=true;breaker_counted=false",
	}).Error; err != nil {
		t.Fatalf("create client canceled log: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               4005,
		Time:             now,
		RequestModelName: "glm-5.1",
		ActualModelName:  "glm-5.1",
		Error:            "no available channel: Claude-CPA (circuit breaker tripped)",
		ErrorCode:        "octopus_channel_circuit_open",
		ErrorStatus:      503,
		ErrorStrategy:    "local_route_selection;reason=circuit_break;upstream_forwarded=false",
	}).Error; err != nil {
		t.Fatalf("create circuit open log: %v", err)
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, model.RelayLog{
		ID:               4002,
		Time:             now,
		RequestModelName: "glm-5.1",
		ActualModelName:  "claude-sonnet-4.5",
		InputTokens:      10,
		OutputTokens:     30,
		Ftut:             900,
		UseTime:          1900,
		Cost:             0.03,
	})
	relayLogCacheLock.Unlock()

	rank, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("model request rank: %v", err)
	}
	if len(rank) != 1 {
		t.Fatalf("expected one requested model rank, got %#v", rank)
	}
	got := rank[0]
	if got.Model != "glm-5.1" {
		t.Fatalf("expected request model glm-5.1, got %q", got.Model)
	}
	if got.RequestCount != 2 || got.RequestSuccess != 2 || got.RequestFailed != 0 {
		t.Fatalf("unexpected counts: %#v", got)
	}
	if got.InputToken != 110 || got.OutputToken != 80 || got.TotalToken != 190 {
		t.Fatalf("unexpected tokens: %#v", got)
	}
	if got.CacheHitToken != 20 || got.CacheInputToken != 100 || got.CacheWriteToken != 5 {
		t.Fatalf("unexpected cache tokens: %#v", got)
	}
	if math.Abs(got.CacheHitRate-float64(20)/float64(110)) > 1e-12 {
		t.Fatalf("expected cache hit rate 20/110, got %f", got.CacheHitRate)
	}
	if got.FirstTokenP90Ms != 900 {
		t.Fatalf("expected p90 900ms, got %d", got.FirstTokenP90Ms)
	}
	if got.AvgThroughput != 40 {
		t.Fatalf("expected throughput 40 tok/s, got %f", got.AvgThroughput)
	}
	if math.Abs(got.TotalCost-0.15) > 1e-12 {
		t.Fatalf("expected total cost 0.15, got %f", got.TotalCost)
	}
	if len(got.RecentActualModels) != 1 || got.RecentActualModels[0] != "claude-sonnet-4.5" {
		t.Fatalf("expected recent actual model audit sample, got %#v", got.RecentActualModels)
	}
}

func TestModelRequestRankFallsBackForLegacyLogs(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()

	logs := []model.RelayLog{
		{
			ID:              5001,
			Time:            now - 20,
			ActualModelName: "legacy-actual",
			InputTokens:     4,
			OutputTokens:    6,
			Cost:            0.01,
		},
		{
			ID:           5002,
			Time:         now - 10,
			BillingModel: "legacy-billing",
			Error:        "missing request model",
			Cost:         0.02,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create legacy logs: %v", err)
	}

	rank, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("model request rank: %v", err)
	}
	actual := findModelRankItem(t, rank, "legacy-actual")
	if actual.RequestCount != 1 || actual.RequestSuccess != 1 {
		t.Fatalf("unexpected actual fallback summary: %#v", actual)
	}
	billing := findModelRankItem(t, rank, "legacy-billing")
	if billing.RequestCount != 1 || billing.RequestFailed != 1 {
		t.Fatalf("unexpected billing fallback summary: %#v", billing)
	}
}

func TestModelRequestRankUsesCacheInputForEffectiveTotalTokens(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()

	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               5101,
		Time:             now,
		RequestModelName: "claude-opus",
		ActualModelName:  "claude-opus",
		InputTokens:      60,
		OutputTokens:     20,
		CacheHitTokens:   30,
		CacheWriteTokens: 10,
		CacheInputTokens: 100,
	}).Error; err != nil {
		t.Fatalf("create log: %v", err)
	}

	rank, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("model request rank: %v", err)
	}
	got := findModelRankItem(t, rank, "claude-opus")
	if got.InputToken != 60 || got.OutputToken != 20 || got.TotalToken != 120 {
		t.Fatalf("expected displayed total to use cache input base, got %#v", got)
	}
	if math.Abs(got.CacheHitRate-0.3) > 1e-12 {
		t.Fatalf("expected cache hit rate 30/100, got %f", got.CacheHitRate)
	}
}

func findModelRankItem(t *testing.T, rank []model.ModelRankItem, name string) model.ModelRankItem {
	t.Helper()
	for _, item := range rank {
		if item.Model == name {
			return item
		}
	}
	t.Fatalf("model %s not found in %#v", name, rank)
	return model.ModelRankItem{}
}
