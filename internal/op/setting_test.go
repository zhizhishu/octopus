package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupSettingTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	return context.Background()
}

func TestSettingRefreshCacheUpgradesLegacyCodexDefaultUserAgent(t *testing.T) {
	ctx := setupSettingTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyCodexHeaderUserAgent,
		Value: model.LegacyDefaultCodexHeaderUserAgent0133,
	}).Error; err != nil {
		t.Fatalf("seed legacy setting: %v", err)
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	cached, err := SettingGetString(model.SettingKeyCodexHeaderUserAgent)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != model.DefaultCodexHeaderUserAgent {
		t.Fatalf("expected cached Codex user-agent upgraded to %q, got %q", model.DefaultCodexHeaderUserAgent, cached)
	}

	var persisted model.Setting
	if err := db.GetDB().WithContext(ctx).First(&persisted, "key = ?", model.SettingKeyCodexHeaderUserAgent).Error; err != nil {
		t.Fatalf("load persisted setting: %v", err)
	}
	if persisted.Value != model.DefaultCodexHeaderUserAgent {
		t.Fatalf("expected persisted Codex user-agent upgraded to %q, got %q", model.DefaultCodexHeaderUserAgent, persisted.Value)
	}
}

func TestSettingRefreshCacheKeepsCustomCodexUserAgent(t *testing.T) {
	ctx := setupSettingTest(t)
	const customUA = "codex_exec/0.133.0 custom-admin-value"

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyCodexHeaderUserAgent,
		Value: customUA,
	}).Error; err != nil {
		t.Fatalf("seed custom setting: %v", err)
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	cached, err := SettingGetString(model.SettingKeyCodexHeaderUserAgent)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != customUA {
		t.Fatalf("expected custom Codex user-agent to stay %q, got %q", customUA, cached)
	}
}

func TestSettingRefreshCacheUpgradesLegacyMacCodexDefaults(t *testing.T) {
	ctx := setupSettingTest(t)

	seed := []model.Setting{
		{Key: model.SettingKeyCodexHeaderUserAgent, Value: model.LegacyDefaultCodexHeaderUserAgentCliRs0114},
		{Key: model.SettingKeyCodexHeaderBetaFeatures, Value: model.LegacyDefaultCodexHeaderBetaFeaturesMultiAgent},
	}
	for i := range seed {
		if err := db.GetDB().WithContext(ctx).Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed %s: %v", seed[i].Key, err)
		}
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	ua, err := SettingGetString(model.SettingKeyCodexHeaderUserAgent)
	if err != nil {
		t.Fatalf("get codex ua: %v", err)
	}
	if ua != model.DefaultCodexHeaderUserAgent {
		t.Fatalf("legacy macOS Codex UA must upgrade to %q, got %q", model.DefaultCodexHeaderUserAgent, ua)
	}

	beta, err := SettingGetString(model.SettingKeyCodexHeaderBetaFeatures)
	if err != nil {
		t.Fatalf("get codex beta: %v", err)
	}
	if beta != model.DefaultCodexHeaderBetaFeatures {
		t.Fatalf("legacy Codex beta must upgrade to %q, got %q", model.DefaultCodexHeaderBetaFeatures, beta)
	}
}

func TestSettingRefreshCacheUpgradesLegacyStreamDataTimeoutDefault(t *testing.T) {
	ctx := setupSettingTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyRelayStreamDataTimeoutSec,
		Value: model.LegacyDefaultRelayStreamDataIntervalTimeoutSeconds,
	}).Error; err != nil {
		t.Fatalf("seed legacy stream timeout setting: %v", err)
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	cached, err := SettingGetString(model.SettingKeyRelayStreamDataTimeoutSec)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != model.DefaultRelayStreamDataIntervalTimeoutSeconds {
		t.Fatalf("expected stream data timeout upgraded to %q, got %q", model.DefaultRelayStreamDataIntervalTimeoutSeconds, cached)
	}
}

func TestSettingRefreshCacheKeepsCustomStreamDataTimeout(t *testing.T) {
	ctx := setupSettingTest(t)
	const customTimeout = "240"

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyRelayStreamDataTimeoutSec,
		Value: customTimeout,
	}).Error; err != nil {
		t.Fatalf("seed custom stream timeout setting: %v", err)
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	cached, err := SettingGetString(model.SettingKeyRelayStreamDataTimeoutSec)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != customTimeout {
		t.Fatalf("expected custom stream data timeout to stay %q, got %q", customTimeout, cached)
	}
}
