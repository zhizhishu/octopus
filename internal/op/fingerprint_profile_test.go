package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupFingerprintProfileTest(t *testing.T) context.Context {
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

// An earlier build seeded a redundant all-empty "默认(Windows)" profile that
// duplicates the dropdown's ProfileID=0 option (so it showed THREE entries). The
// refresh must drop that exact auto-seed on upgrade, rename the legacy "Linux 真机"
// preset to its clearer "Linux · Debian" name, and backfill the 2nd built-in.
func TestFingerprintProfileRefreshDropsRedundantDefault(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	redundant := &model.FingerprintProfile{Name: "默认(Windows)", Seed: "stale-instance-seed"}
	if err := db.GetDB().WithContext(ctx).Create(redundant).Error; err != nil {
		t.Fatalf("seed redundant default: %v", err)
	}
	linux := &model.FingerprintProfile{
		Name:            "Linux 真机",
		Seed:            "linux-seed",
		ClaudeUserAgent: "claude-cli/2.1.186 (external, sdk-cli)",
		ClaudeOS:        "Linux",
	}
	if err := db.GetDB().WithContext(ctx).Create(linux).Error; err != nil {
		t.Fatalf("seed linux profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var remaining []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Order("id").Find(&remaining).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	// After cleanup the redundant all-empty 默认(Windows) is dropped; the legacy
	// "Linux 真机" preset is renamed in place to "Linux · Debian"; and because the 2nd
	// built-in ("Linux · Ubuntu") is missing it is backfilled, so exactly the two
	// built-in Linux identities remain under their clearer names.
	names := make(map[string]bool, len(remaining))
	for _, p := range remaining {
		names[p.Name] = true
	}
	if names["默认(Windows)"] {
		t.Fatalf("redundant all-empty 默认(Windows) must be dropped, got %+v", remaining)
	}
	if names["Linux 真机"] || names["Linux 真机 2 (Ubuntu)"] {
		t.Fatalf("legacy 真机 preset names must be renamed away, got %+v", remaining)
	}
	if len(remaining) != 2 || !names["Linux · Debian"] || !names["Linux · Ubuntu"] {
		t.Fatalf("expected 默认(Windows) dropped and both built-in Linux profiles present under new names, got %d: %+v", len(remaining), remaining)
	}
}

// A user-customised profile that merely happens to be named "默认(Windows)" but has
// a real header field set must NOT be removed — only the all-empty auto-seed is.
func TestFingerprintProfileRefreshKeepsCustomizedProfileNamedDefault(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	custom := &model.FingerprintProfile{Name: "默认(Windows)", Seed: "x", ClaudeOS: "Windows"}
	if err := db.GetDB().WithContext(ctx).Create(custom).Error; err != nil {
		t.Fatalf("seed customized default-named profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var remaining []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Find(&remaining).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "默认(Windows)" || remaining[0].ClaudeOS != "Windows" {
		t.Fatalf("customised default-named profile must be preserved, got %+v", remaining)
	}
}

// TestFingerprintProfileSeedsDistinctGenericUA pins that a FRESH deploy seeds the two
// built-in Linux presets with DISTINCT generic (non-CLI) User-Agents — Debian gets the
// global-default UA, Ubuntu gets its own — so picking a preset actually changes the
// non-CLI UA (before this they were both empty and fell back to one value).
func TestFingerprintProfileSeedsDistinctGenericUA(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var profiles []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Order("id").Find(&profiles).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	byName := make(map[string]model.FingerprintProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}
	debian, ok := byName["Linux · Debian"]
	if !ok {
		t.Fatalf("Debian preset missing: %+v", profiles)
	}
	ubuntu, ok := byName["Linux · Ubuntu"]
	if !ok {
		t.Fatalf("Ubuntu preset missing: %+v", profiles)
	}
	if debian.GenericUA != model.DefaultGenericUA {
		t.Fatalf("Debian preset GenericUA = %q, want DefaultGenericUA %q", debian.GenericUA, model.DefaultGenericUA)
	}
	if ubuntu.GenericUA != model.GenericUAUbuntu {
		t.Fatalf("Ubuntu preset GenericUA = %q, want GenericUAUbuntu %q", ubuntu.GenericUA, model.GenericUAUbuntu)
	}
	if debian.GenericUA == ubuntu.GenericUA {
		t.Fatalf("the two presets must carry DISTINCT generic UAs, both = %q", debian.GenericUA)
	}
}

// TestFingerprintProfileBackfillsGenericUAOnlyWhenEmpty pins the upgrade path: a
// deployment whose built-in presets predate GenericUA (empty) gets them backfilled to
// the distinct values, while an operator-customised GenericUA on a built-in preset is
// NEVER overwritten.
func TestFingerprintProfileBackfillsGenericUAOnlyWhenEmpty(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	// Debian preset present but with an empty GenericUA (pre-field seed) -> must backfill.
	// Ubuntu preset present with an operator-customised GenericUA -> must be preserved.
	custom := "Mozilla/5.0 (operator-pinned) CustomAgent/1.0"
	if err := db.GetDB().WithContext(ctx).Create(&model.FingerprintProfile{
		Name: "Linux · Debian", Seed: "seed-debian", ClaudeUserAgent: "claude-cli/2.1.212 (external, sdk-cli)", ClaudeOS: "Linux",
	}).Error; err != nil {
		t.Fatalf("seed empty-genericua debian: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.FingerprintProfile{
		Name: "Linux · Ubuntu", Seed: "seed-ubuntu", ClaudeUserAgent: "claude-cli/2.1.212 (external, sdk-cli)", ClaudeOS: "Linux", GenericUA: custom,
	}).Error; err != nil {
		t.Fatalf("seed customized-genericua ubuntu: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var profiles []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Find(&profiles).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	byName := make(map[string]model.FingerprintProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}
	if got := byName["Linux · Debian"].GenericUA; got != model.DefaultGenericUA {
		t.Fatalf("empty Debian GenericUA must be backfilled to DefaultGenericUA, got %q", got)
	}
	if got := byName["Linux · Ubuntu"].GenericUA; got != custom {
		t.Fatalf("operator-customised Ubuntu GenericUA must be preserved, got %q want %q", got, custom)
	}
}
