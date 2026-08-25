package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// The telemetry cache is deliberately TTL-bounded: RelayLogAdd does NOT invalidate
// it (per-add invalidation defeated the cache entirely under sustained traffic).
// A fresh rank is served stale until the TTL lapses or an explicit invalidation
// (RelayLogClear / manual) drops it. This test locks that contract.
func TestModelRequestRankCacheIsTTLBoundedNotPerAddInvalidated(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now().Unix()

	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               9101,
		Time:             now,
		RequestModelName: "cached-model",
		ActualModelName:  "cached-model",
		InputTokens:      10,
		OutputTokens:     2,
	}).Error; err != nil {
		t.Fatalf("create log: %v", err)
	}
	invalidateModelTelemetryCache()

	first, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("first rank: %v", err)
	}
	got := findModelRankItem(t, first, "cached-model")
	if got.RequestCount != 1 {
		t.Fatalf("expected one request before add, got %#v", got)
	}

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatalf("disable persistence: %v", err)
	}
	if err := RelayLogAdd(ctx, model.RelayLog{
		RequestModelName: "cached-model",
		ActualModelName:  "cached-model",
		InputTokens:      4,
		OutputTokens:     1,
	}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}

	// Within the TTL window the cached rank stays stale on purpose.
	second, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("second rank: %v", err)
	}
	got = findModelRankItem(t, second, "cached-model")
	if got.RequestCount != 1 {
		t.Fatalf("expected TTL-bounded cache to serve the stale rank after add, got %#v", got)
	}

	// An explicit invalidation must drop the entry so the next call recomputes.
	invalidateModelTelemetryCache()
	third, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("third rank: %v", err)
	}
	got = findModelRankItem(t, third, "cached-model")
	if got.RequestCount != 2 {
		t.Fatalf("expected explicit invalidation to force a recompute, got %#v", got)
	}
}

func TestRelayLogStreamTokenCarriesLiveFilters(t *testing.T) {
	token, err := RelayLogStreamTokenCreate(model.RelayLogScope{
		Endpoint:      "messages",
		Severity:      "error",
		RetriedOnly:   true,
		HideModelTest: true,
	}, true)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	scope, ok := RelayLogStreamTokenVerify(token)
	if !ok {
		t.Fatal("expected token to verify")
	}
	if scope.Endpoint != "messages" || scope.Severity != "error" || !scope.RetriedOnly || !scope.HideModelTest {
		t.Fatalf("stream token dropped live filters: %#v", scope)
	}
}
