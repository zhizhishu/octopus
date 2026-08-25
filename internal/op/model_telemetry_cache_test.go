package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func TestModelRequestRankCacheInvalidatesOnRelayLogAdd(t *testing.T) {
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

	second, err := ModelRequestRank(ctx)
	if err != nil {
		t.Fatalf("second rank: %v", err)
	}
	got = findModelRankItem(t, second, "cached-model")
	if got.RequestCount != 2 {
		t.Fatalf("expected cache to invalidate after RelayLogAdd, got %#v", got)
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
