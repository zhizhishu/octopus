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

// TestSettingRefreshCacheUpgradesLegacyClaudeUserAgentAndPackage pins F2: the legacy
// claude UA 2.1.126 and package 0.81.0 now converge to the current default IN THE DB at
// startup (single authority), not only via a relay read-time patch. This keeps the admin
// settings display (SettingList / SettingGetString, which reads this cache) equal to what
// is actually sent on the wire.
func TestSettingRefreshCacheUpgradesLegacyClaudeUserAgentAndPackage(t *testing.T) {
	ctx := setupSettingTest(t)

	seed := []model.Setting{
		{Key: model.SettingKeyClaudeHeaderUserAgent, Value: model.LegacyDefaultClaudeHeaderUserAgent2126},
		{Key: model.SettingKeyClaudeHeaderPackage, Value: model.LegacyDefaultClaudeHeaderPackage0810},
	}
	for i := range seed {
		if err := db.GetDB().WithContext(ctx).Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed %s: %v", seed[i].Key, err)
		}
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	// Cache (== SettingList display == the value relay settingString reads) must show the
	// converged current default.
	ua, err := SettingGetString(model.SettingKeyClaudeHeaderUserAgent)
	if err != nil {
		t.Fatalf("get claude ua: %v", err)
	}
	if ua != model.DefaultClaudeHeaderUserAgent {
		t.Fatalf("legacy claude UA must upgrade to %q, got %q", model.DefaultClaudeHeaderUserAgent, ua)
	}
	pkg, err := SettingGetString(model.SettingKeyClaudeHeaderPackage)
	if err != nil {
		t.Fatalf("get claude package: %v", err)
	}
	if pkg != model.DefaultClaudeHeaderPackageVersion {
		t.Fatalf("legacy claude package must upgrade to %q, got %q", model.DefaultClaudeHeaderPackageVersion, pkg)
	}

	// The DB must be rewritten too (not just the cache), so a restart / SettingList never
	// re-surfaces the legacy value.
	var persistedUA, persistedPkg model.Setting
	if err := db.GetDB().WithContext(ctx).First(&persistedUA, "key = ?", model.SettingKeyClaudeHeaderUserAgent).Error; err != nil {
		t.Fatalf("load persisted claude ua: %v", err)
	}
	if persistedUA.Value != model.DefaultClaudeHeaderUserAgent {
		t.Fatalf("persisted claude UA must be %q, got %q", model.DefaultClaudeHeaderUserAgent, persistedUA.Value)
	}
	if err := db.GetDB().WithContext(ctx).First(&persistedPkg, "key = ?", model.SettingKeyClaudeHeaderPackage).Error; err != nil {
		t.Fatalf("load persisted claude package: %v", err)
	}
	if persistedPkg.Value != model.DefaultClaudeHeaderPackageVersion {
		t.Fatalf("persisted claude package must be %q, got %q", model.DefaultClaudeHeaderPackageVersion, persistedPkg.Value)
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

func TestSettingRefreshCacheKeepsEmptyRouteModeOverride(t *testing.T) {
	ctx := setupSettingTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyRouteModeOverride,
		Value: "",
	}).Error; err != nil {
		t.Fatalf("seed empty route_mode_override: %v", err)
	}

	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	cached, err := SettingGetString(model.SettingKeyRouteModeOverride)
	if err != nil {
		t.Fatalf("get cached route_mode_override: %v", err)
	}
	if cached != "" {
		t.Fatalf("expected empty route_mode_override to stay empty (follow group), got %q", cached)
	}

	var persisted model.Setting
	if err := db.GetDB().WithContext(ctx).First(&persisted, "key = ?", model.SettingKeyRouteModeOverride).Error; err != nil {
		t.Fatalf("load persisted route_mode_override: %v", err)
	}
	if persisted.Value != "" {
		t.Fatalf("expected persisted route_mode_override to stay empty, got %q", persisted.Value)
	}
}
