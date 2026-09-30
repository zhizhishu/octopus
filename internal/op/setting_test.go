package op

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
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

func assertRouteModeSetting(t *testing.T, wantDB, wantCache string) {
	t.Helper()
	cached, err := SettingGetString(model.SettingKeyRouteModeOverride)
	if err != nil || cached != wantCache {
		t.Fatalf("cached route mode = %q, %v; want %q", cached, err, wantCache)
	}
	var persisted model.Setting
	if err := db.GetDB().First(&persisted, "key = ?", model.SettingKeyRouteModeOverride).Error; err != nil {
		t.Fatalf("load persisted route mode: %v", err)
	}
	if persisted.Value != wantDB {
		t.Fatalf("persisted route mode = %q, want %q", persisted.Value, wantDB)
	}
}

func TestSettingRefreshCacheNormalizesRouteModeOverride(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		missing         bool
	}{
		{name: "new database", missing: true, want: "fill_first"},
		{name: "legacy empty", want: "fill_first"},
		{name: "legacy blank", raw: " \t ", want: "fill_first"},
		{name: "legacy unknown", raw: "smart", want: "fill_first"},
		{name: "spread", raw: "spread", want: "spread"},
		{name: "canonical spread", raw: " SPREAD ", want: "spread"},
		{name: "fill first", raw: "fill_first", want: "fill_first"},
		{name: "canonical fill first", raw: " Fill_First ", want: "fill_first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupSettingTest(t)
			if !tc.missing {
				if err := db.GetDB().Create(&model.Setting{Key: model.SettingKeyRouteModeOverride, Value: tc.raw}).Error; err != nil {
					t.Fatalf("seed route mode: %v", err)
				}
			}
			if err := settingRefreshCache(ctx); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			assertRouteModeSetting(t, tc.want, tc.want)

			// A second refresh must not issue even an idempotent UPDATE.
			const callback = "test:reject_repeated_route_update"
			if err := db.GetDB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				tx.AddError(errors.New("unexpected update after normalization"))
			}); err != nil {
				t.Fatal(err)
			}
			defer db.GetDB().Callback().Update().Remove(callback)
			if err := settingRefreshCache(ctx); err != nil {
				t.Fatalf("idempotent refresh: %v", err)
			}
			assertRouteModeSetting(t, tc.want, tc.want)
		})
	}
}

func TestSettingSetRouteModeOverrideCanonicalAndRejectsUnknown(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{raw: " SPREAD ", want: "spread"},
		{raw: "", want: "fill_first"},
		{raw: "spread", want: "spread"},
		{raw: " \t ", want: "fill_first"},
		{raw: " Fill_First ", want: "fill_first"},
	} {
		if err := SettingSetString(model.SettingKeyRouteModeOverride, tc.raw); err != nil {
			t.Fatalf("set %q: %v", tc.raw, err)
		}
		assertRouteModeSetting(t, tc.want, tc.want)
	}
	for _, raw := range []string{"smart", "round_robin"} {
		if err := SettingSetString(model.SettingKeyRouteModeOverride, raw); err == nil {
			t.Fatalf("unknown mode %q accepted", raw)
		}
		assertRouteModeSetting(t, "fill_first", "fill_first")
	}
}

func TestSettingRouteModeWriteFailurePreservesCache(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	if err := SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Model(&model.Setting{Key: model.SettingKeyRouteModeOverride}).Update("value", "legacy-invalid").Error; err != nil {
		t.Fatal(err)
	}
	const callback = "test:reject_route_mode_write"
	if err := db.GetDB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		tx.AddError(errors.New("injected setting write failure"))
	}); err != nil {
		t.Fatal(err)
	}
	defer db.GetDB().Callback().Update().Remove(callback)

	if err := settingRefreshCache(ctx); err == nil {
		t.Fatal("normalization must fail when its database write fails")
	}
	assertRouteModeSetting(t, "legacy-invalid", "spread")
	if err := SettingSetString(model.SettingKeyRouteModeOverride, ""); err == nil {
		t.Fatal("setter must fail when its database write fails")
	}
	assertRouteModeSetting(t, "legacy-invalid", "spread")
	for i := 0; i < 2; i++ {
		if _, err := SettingList(ctx); err != nil {
			t.Fatal(err)
		}
		assertRouteModeSetting(t, "legacy-invalid", "spread")
	}
}
