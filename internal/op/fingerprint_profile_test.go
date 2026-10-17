package op

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
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

// loadFingerprintProfilesByName reloads every row from the DB (not the cache) keyed
// by name, so a test asserts on what actually persisted.
func loadFingerprintProfilesByName(t *testing.T, ctx context.Context) map[string]model.FingerprintProfile {
	t.Helper()

	var profiles []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Order("id").Find(&profiles).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	byName := make(map[string]model.FingerprintProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}
	if len(byName) != len(profiles) {
		t.Fatalf("duplicate profile names in %+v", profiles)
	}
	return byName
}

// canonicalIdentity strips the two row-local fields (primary key + Seed) so a row can
// be compared field-for-field against the canonical preset it must converge to. Seed
// is deliberately excluded: convergence must NEVER rewrite it (that would change the
// device_id / installation id the preset already ships).
func canonicalIdentity(p model.FingerprintProfile) model.FingerprintProfile {
	p.ID = 0
	p.Seed = ""
	return p
}

// assertConvergedTo pins that EVERY identity field of a row equals the canonical
// preset's — the whole point of the force-converge design, and what stops a stale
// field (old UA, codex_exec originator, empty GenericUA) from surviving an upgrade.
func assertConvergedTo(t *testing.T, got model.FingerprintProfile, want *model.FingerprintProfile) {
	t.Helper()

	if !reflect.DeepEqual(canonicalIdentity(got), canonicalIdentity(*want)) {
		t.Fatalf("profile %q did not converge to canonical\n got: %+v\nwant: %+v", want.Name, got, *want)
	}
}

// An earlier build seeded a redundant all-empty "默认(Windows)" profile that
// duplicates the dropdown's ProfileID=0 option (so it showed THREE entries). The
// refresh must drop that exact auto-seed on upgrade, converge the legacy "Linux 真机"
// preset onto its canonical "Linux · Debian" identity, and backfill the 2nd built-in.
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

	// After cleanup the redundant all-empty 默认(Windows) is dropped; the legacy
	// "Linux 真机" preset converges in place to "Linux · Debian"; and because the 2nd
	// built-in ("Linux · Ubuntu") is missing it is backfilled, so exactly the two
	// Linux built-in identities remain under their canonical names. The retired
	// "macOS · Chrome" preset is never re-seeded next to them.
	byName := loadFingerprintProfilesByName(t, ctx)
	if _, ok := byName["默认(Windows)"]; ok {
		t.Fatalf("redundant all-empty 默认(Windows) must be dropped, got %+v", byName)
	}
	if _, ok := byName["Linux 真机"]; ok {
		t.Fatalf("legacy 真机 preset name must be converged away, got %+v", byName)
	}
	if _, ok := byName["Linux 真机 2 (Ubuntu)"]; ok {
		t.Fatalf("legacy 真机 2 preset name must be converged away, got %+v", byName)
	}
	presets := builtinLinuxPresets()
	if len(byName) != len(presets) {
		t.Fatalf("expected exactly the %d built-in presets, got %d: %+v", len(presets), len(byName), byName)
	}
	for _, preset := range presets {
		got, ok := byName[preset.Name]
		if !ok {
			t.Fatalf("built-in preset %q missing after refresh: %+v", preset.Name, byName)
		}
		assertConvergedTo(t, got, preset)
	}
}

// A user-customised profile that merely happens to be named "默认(Windows)" but has
// a real header field set must NOT be removed — only the all-empty auto-seed is. It
// is also not a built-in name, so the built-ins are NOT resurrected next to it.
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

// TestFingerprintProfileSeedsCanonicalBuiltins pins the FRESH-deploy path: an empty DB
// gets exactly the two built-in Linux presets, every field straight from
// builtinLinuxPresets() (the single source of truth), with DISTINCT generic (non-CLI)
// User-Agents and DISTINCT device seeds — so picking a preset really does change both
// the non-CLI UA and the derived device_id, instead of the two reading as one machine.
func TestFingerprintProfileSeedsCanonicalBuiltins(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	presets := builtinLinuxPresets()
	if len(byName) != len(presets) {
		t.Fatalf("fresh deploy must seed exactly %d presets, got %d: %+v", len(presets), len(byName), byName)
	}
	for _, preset := range presets {
		got, ok := byName[preset.Name]
		if !ok {
			t.Fatalf("preset %q missing: %+v", preset.Name, byName)
		}
		assertConvergedTo(t, got, preset)
		if got.Seed != preset.Seed {
			t.Fatalf("preset %q seed = %q, want the derived %q", preset.Name, got.Seed, preset.Seed)
		}
	}
	debian := byName[builtinDebianPresetName]
	ubuntu := byName[builtinUbuntuPresetName]
	if debian.GenericUA != model.DefaultGenericUA {
		t.Fatalf("Debian preset GenericUA = %q, want DefaultGenericUA %q", debian.GenericUA, model.DefaultGenericUA)
	}
	if ubuntu.GenericUA != model.GenericUAUbuntu {
		t.Fatalf("Ubuntu preset GenericUA = %q, want GenericUAUbuntu %q", ubuntu.GenericUA, model.GenericUAUbuntu)
	}
	if debian.GenericUA == ubuntu.GenericUA {
		t.Fatalf("the two presets must carry DISTINCT generic UAs, both = %q", debian.GenericUA)
	}
	if debian.Seed == ubuntu.Seed {
		t.Fatalf("the two presets must carry DISTINCT device seeds, both = %q", debian.Seed)
	}
	if debian.CodexUserAgent == ubuntu.CodexUserAgent {
		t.Fatalf("the two presets must carry DISTINCT codex UAs (distro token), both = %q", debian.CodexUserAgent)
	}
}

// TestFingerprintProfileConvergesLegacyBuiltinInOneStep is the core of the redesign:
// a row left over from a MUCH older build — legacy "Linux 真机" name, claude 2.1.186,
// headless codex_exec/0.142.0, node v24.3.0, no GenericUA — lands on the CURRENT
// identity in a SINGLE refresh. The old code walked a chain of per-version rewrites
// (2.1.186→2.1.198→2.1.212, 0.142.0→0.142.5→cli_rs→0.144.1→0.145.0, rename, UA
// backfill); this asserts the one-step result with NO stale value surviving.
func TestFingerprintProfileConvergesLegacyBuiltinInOneStep(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	legacy := &model.FingerprintProfile{
		Name:                 "Linux 真机",
		Seed:                 "legacy-device-seed",
		ClaudeUserAgent:      "claude-cli/2.1.186 (external, sdk-cli)",
		ClaudePackageVersion: "0.94.0",
		ClaudeRuntimeVersion: "v24.3.0",
		ClaudeOS:             "Linux",
		ClaudeArch:           "x64",
		ClaudeTimeout:        "600",
		CodexUserAgent:       "codex_exec/0.142.0 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.142.0)",
		CodexOriginator:      "codex_exec",
		CodexBetaFeatures:    "remote_compaction_v2",
		// GenericUA left empty: the field postdates this row.
	}
	if err := db.GetDB().WithContext(ctx).Create(legacy).Error; err != nil {
		t.Fatalf("seed legacy built-in: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	got, ok := byName[builtinDebianPresetName]
	if !ok {
		t.Fatalf("legacy row must be converged to %q, got %+v", builtinDebianPresetName, byName)
	}
	if got.ID != legacy.ID {
		t.Fatalf("convergence must rewrite the SAME row in place: id %d -> %d", legacy.ID, got.ID)
	}
	// Spelled out field by field so a regression names the exact stale value.
	if got.ClaudeUserAgent != "claude-cli/2.1.296 (external, sdk-cli)" {
		t.Fatalf("claude UA = %q, want the current 2.1.296", got.ClaudeUserAgent)
	}
	if got.ClaudeRuntimeVersion != "v26.3.0" {
		t.Fatalf("claude runtime = %q, want v26.3.0", got.ClaudeRuntimeVersion)
	}
	// 2026-10-10 实测对齐后的当前身份(真 Codex CLI 抓包: codex_exec/0.161.0)。平台段沿用旧值。
	if got.CodexUserAgent != "codex_exec/0.161.0 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.161.0)" {
		t.Fatalf("codex UA = %q, want the current codex_exec 0.161.0 Debian UA", got.CodexUserAgent)
	}
	if got.CodexOriginator != "codex_exec" {
		t.Fatalf("codex originator = %q, want codex_exec (must match the UA's first token)", got.CodexOriginator)
	}
	if got.GenericUA != model.DefaultGenericUA {
		t.Fatalf("generic UA = %q, want DefaultGenericUA %q", got.GenericUA, model.DefaultGenericUA)
	}
	// ...and the whole struct, so no OTHER field is left on a stale value either.
	assertConvergedTo(t, got, builtinLinuxPresets()[0])
	// The row's own device seed is the one thing convergence must not touch.
	if got.Seed != "legacy-device-seed" {
		t.Fatalf("seed = %q, want the row's original seed preserved", got.Seed)
	}
	// The 2nd built-in is backfilled next to it; the retired macOS preset is not.
	if _, ok := byName[builtinUbuntuPresetName]; !ok {
		t.Fatalf("2nd built-in must be backfilled, got %+v", byName)
	}
	if _, ok := byName[retiredMacOSPresetName]; ok {
		t.Fatalf("retired macOS preset must not be backfilled, got %+v", byName)
	}
}

// TestFingerprintProfileRenamesLegacyMacOSBuiltin pins the 2026-10-17 change: the
// legacy "macOS · Chrome" row is a first-class built-in again, so startup RENAMES it
// in place to the canonical macOS name and converges every identity field, instead of
// deleting it. The row id survives, which keeps any channel that had selected it
// pointing at the SAME identity (no silent drop back to the global default).
func TestFingerprintProfileRenamesLegacyMacOSBuiltin(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	presets := builtinLinuxPresets()
	debian := *presets[0]
	ubuntu := *presets[1]
	debian.Seed = "keep-debian-seed"
	ubuntu.Seed = "keep-ubuntu-seed"
	if err := db.GetDB().WithContext(ctx).Create(&debian).Error; err != nil {
		t.Fatalf("seed debian: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&ubuntu).Error; err != nil {
		t.Fatalf("seed ubuntu: %v", err)
	}
	// The legacy 3rd built-in with a now-stale Chrome-on-macOS identity.
	macrow := &model.FingerprintProfile{
		Name:                 "macOS · Chrome",
		Seed:                 "mac-seed",
		ClaudeUserAgent:      "claude-cli/2.1.212 (external, sdk-cli)",
		ClaudePackageVersion: "0.94.0",
		ClaudeRuntimeVersion: "v26.3.0",
		ClaudeOS:             "MacOS",
		ClaudeArch:           "x64",
		ClaudeTimeout:        "600",
		CodexUserAgent:       "codex_cli_rs/0.145.0 (macOS 10.15.7; x86_64) unknown (codex_cli_rs; 0.145.0)",
		CodexOriginator:      "codex_cli_rs",
		CodexBetaFeatures:    "remote_compaction_v2",
		GenericUA:            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}
	if err := db.GetDB().WithContext(ctx).Create(macrow).Error; err != nil {
		t.Fatalf("seed macos: %v", err)
	}
	channel := &model.Channel{
		Name:  "channel-with-legacy-macos-profile",
		Cloak: model.ChannelCloak{Mode: "never", ProfileID: macrow.ID},
	}
	if err := db.GetDB().WithContext(ctx).Create(channel).Error; err != nil {
		t.Fatalf("seed channel pointing at legacy profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	if len(byName) != 3 {
		t.Fatalf("expected all three built-ins kept, got %d: %+v", len(byName), byName)
	}
	if byName[builtinDebianPresetName].Seed != "keep-debian-seed" {
		t.Fatalf("existing Debian seed must be preserved")
	}
	if byName[builtinUbuntuPresetName].Seed != "keep-ubuntu-seed" {
		t.Fatalf("existing Ubuntu seed must be preserved")
	}
	// The legacy name is gone, the canonical name is present, and it is the SAME row.
	if _, ok := byName[retiredMacOSPresetName]; ok {
		t.Fatalf("legacy macOS name must be renamed away, got %+v", byName)
	}
	mac, ok := byName[builtinMacOSPresetName]
	if !ok {
		t.Fatalf("macOS built-in must exist under its canonical name, got %+v", byName)
	}
	if mac.ID != macrow.ID {
		t.Fatalf("rename must keep the row id: got %d want %d", mac.ID, macrow.ID)
	}
	// The stale Chrome identity converged to the current CherryStudio one.
	assertConvergedTo(t, mac, presets[2])
	if mac.GenericUA != model.GenericUAMacOS {
		t.Fatalf("macOS generic UA must be the CherryStudio identity, got %q", mac.GenericUA)
	}
	if strings.Contains(mac.GenericUA, "Chrome/131") {
		t.Fatalf("legacy bare-Chrome macOS UA must be gone, got %q", mac.GenericUA)
	}
	// The channel still points at the same, now-canonical, identity.
	if _, ok := FingerprintProfileGet(macrow.ID); !ok {
		t.Fatalf("old macOS profile id %d must still resolve after the rename", macrow.ID)
	}
	var persistedChannel model.Channel
	if err := db.GetDB().WithContext(ctx).First(&persistedChannel, channel.ID).Error; err != nil {
		t.Fatalf("reload channel pointing at legacy profile: %v", err)
	}
	if persistedChannel.Cloak.ProfileID != macrow.ID {
		t.Fatalf("refresh must not rewrite channel profile id: got %d want %d", persistedChannel.Cloak.ProfileID, macrow.ID)
	}
}

// TestFingerprintProfileRefreshIsIdempotentAtThreePresets: a second refresh must not
// error, duplicate, or resurrect anything — three built-ins stay three.
func TestFingerprintProfileRefreshIsIdempotentAtThreePresets(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	preset := *builtinLinuxPresets()[0]
	preset.Seed = "keep-debian-seed"
	if err := db.GetDB().WithContext(ctx).Create(&preset).Error; err != nil {
		t.Fatalf("seed debian: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("second refresh must be a no-op, got: %v", err)
	}
	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("third refresh must be a no-op, got: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	if len(byName) != 3 {
		t.Fatalf("refresh must remain idempotent at three presets, got %d: %+v", len(byName), byName)
	}
	if byName[builtinDebianPresetName].Seed != "keep-debian-seed" {
		t.Fatalf("Debian seed must be preserved across idempotent refreshes")
	}
}

// TestFingerprintProfileKeepsOtherNamedMacOSProfile: a custom macOS-flavoured profile
// under a NON-canonical name must be preserved alongside the three built-ins — the
// rename matches the exact legacy name only, never any other name.
func TestFingerprintProfileKeepsOtherNamedMacOSProfile(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	presets := builtinLinuxPresets()
	debian := *presets[0]
	ubuntu := *presets[1]
	if err := db.GetDB().WithContext(ctx).Create(&debian).Error; err != nil {
		t.Fatalf("seed debian: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&ubuntu).Error; err != nil {
		t.Fatalf("seed ubuntu: %v", err)
	}

	custom := &model.FingerprintProfile{
		Name:                 "我的 Mac 真机",
		Seed:                 "mac-custom-seed",
		ClaudeUserAgent:      "claude-cli/2.1.212 (external, sdk-cli)",
		ClaudePackageVersion: "0.94.0",
		ClaudeRuntimeVersion: "v26.3.0",
		ClaudeOS:             "MacOS",
		ClaudeArch:           "x64",
		ClaudeTimeout:        "600",
		CodexUserAgent:       "codex_cli_rs/0.145.0 (macOS 10.15.7; x86_64) unknown (codex_cli_rs; 0.145.0)",
		CodexOriginator:      "codex_cli_rs",
		CodexBetaFeatures:    "remote_compaction_v2",
		GenericUA:            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) CustomAgent/1.0",
	}
	if err := db.GetDB().WithContext(ctx).Create(custom).Error; err != nil {
		t.Fatalf("seed custom macos profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	// Three built-ins plus the custom Mac one.
	if len(byName) != 4 {
		t.Fatalf("built-ins + custom macOS profile must survive, got %d: %+v", len(byName), byName)
	}
	got, ok := byName[custom.Name]
	if !ok {
		t.Fatalf("custom macOS profile under a non-canonical name must survive, got %+v", byName)
	}
	if got.Seed != "mac-custom-seed" || got.ClaudeOS != "MacOS" || got.ClaudeUserAgent != custom.ClaudeUserAgent || got.GenericUA != custom.GenericUA {
		t.Fatalf("custom macOS profile must be preserved byte-for-byte, got %+v", got)
	}
}

// TestFingerprintProfileConvergesEditedLegacyMacOSRow: an EDITED legacy-named row is
// renamed and then converged like any other built-in — the name identifies the built-in,
// and a built-in's identity fields are system-managed.
func TestFingerprintProfileConvergesEditedLegacyMacOSRow(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	edited := &model.FingerprintProfile{
		Name:            "macOS · Chrome",
		Seed:            "edited-mac-seed",
		ClaudeUserAgent: "claude-cli/9.9.9 (external, sdk-cli)",
		ClaudeOS:        "MacOS",
		GenericUA:       "Mozilla/5.0 (operator-pinned) CustomAgent/1.0",
	}
	if err := db.GetDB().WithContext(ctx).Create(edited).Error; err != nil {
		t.Fatalf("seed edited macos row: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	mac, ok := FingerprintProfileGet(edited.ID)
	if !ok {
		t.Fatalf("edited legacy row must survive the rename under the same id %d", edited.ID)
	}
	if mac.Name != builtinMacOSPresetName {
		t.Fatalf("legacy name must converge to %q, got %q", builtinMacOSPresetName, mac.Name)
	}
	if mac.ClaudeUserAgent == "claude-cli/9.9.9 (external, sdk-cli)" {
		t.Fatalf("operator edit to a built-in must be converged away, got %q", mac.ClaudeUserAgent)
	}
	// The row's OWN seed is the one thing a built-in convergence keeps.
	if mac.Seed != "edited-mac-seed" {
		t.Fatalf("row seed must be preserved, got %q", mac.Seed)
	}
}

// TestFingerprintProfileConvergeForcesCustomisedBuiltin pins the DELIBERATE behaviour
// change of the force-converge design: the built-in presets are SYSTEM-MANAGED, so an
// operator edit to a built-in's identity fields (here a pinned GenericUA + a frozen
// claude UA) is overwritten on the next restart — a custom identity belongs in a NEW
// profile. This replaces the older expectation that a customised built-in field was
// preserved, which is what forced every version bump to add another exact-match hop.
// A profile under ANY OTHER name stays untouched.
func TestFingerprintProfileConvergeForcesCustomisedBuiltin(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	const pinnedUA = "Mozilla/5.0 (operator-pinned) CustomAgent/1.0"
	if err := db.GetDB().WithContext(ctx).Create(&model.FingerprintProfile{
		Name:            builtinDebianPresetName,
		Seed:            "seed-debian",
		ClaudeUserAgent: "claude-cli/2.1.198 (external, sdk-cli)",
		ClaudeOS:        "Linux",
		GenericUA:       pinnedUA,
	}).Error; err != nil {
		t.Fatalf("seed customised built-in: %v", err)
	}
	// An operator-owned profile: not a built-in name, so nothing here may touch it.
	mine := &model.FingerprintProfile{
		Name:            "我的自定义",
		Seed:            "seed-mine",
		ClaudeUserAgent: "claude-cli/1.0.0 (external, sdk-cli)",
		GenericUA:       pinnedUA,
	}
	if err := db.GetDB().WithContext(ctx).Create(mine).Error; err != nil {
		t.Fatalf("seed operator profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	assertConvergedTo(t, byName[builtinDebianPresetName], builtinLinuxPresets()[0])
	if got := byName[builtinDebianPresetName].Seed; got != "seed-debian" {
		t.Fatalf("built-in seed = %q, want the row's own seed kept", got)
	}
	got, ok := byName["我的自定义"]
	if !ok {
		t.Fatalf("operator profile must survive, got %+v", byName)
	}
	if got.ClaudeUserAgent != mine.ClaudeUserAgent || got.GenericUA != pinnedUA || got.Seed != "seed-mine" {
		t.Fatalf("operator profile under a non-built-in name must be untouched, got %+v", got)
	}
}

// TestFingerprintProfileConvergeFillsOnlyEmptySeed: an existing built-in row whose
// Seed was never persisted gets the canonical derived seed, while a row that HAS a
// seed keeps it (covered above). Without the fill such a row would derive its
// device_id from an empty seed.
func TestFingerprintProfileConvergeFillsOnlyEmptySeed(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.FingerprintProfile{
		Name: builtinDebianPresetName,
	}).Error; err != nil {
		t.Fatalf("seed built-in without a seed: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	want := builtinLinuxPresets()[0]
	if got := byName[builtinDebianPresetName].Seed; got != want.Seed || got == "" {
		t.Fatalf("empty seed must be filled with the derived seed, got %q want %q", got, want.Seed)
	}
}

// TestFingerprintProfileRefreshRespectsDeletedBuiltins: once an operator deletes the
// 1st built-in, NEITHER preset is resurrected on restart — force-converge only rewrites
// rows that are still there, it never re-creates a deliberately removed profile.
func TestFingerprintProfileRefreshRespectsDeletedBuiltins(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	only := &model.FingerprintProfile{Name: "我的自定义", Seed: "seed-mine", ClaudeOS: "Linux"}
	if err := db.GetDB().WithContext(ctx).Create(only).Error; err != nil {
		t.Fatalf("seed operator profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	byName := loadFingerprintProfilesByName(t, ctx)
	if len(byName) != 1 {
		t.Fatalf("deleted built-ins must not be resurrected, got %+v", byName)
	}
	if _, ok := byName["我的自定义"]; !ok {
		t.Fatalf("operator profile must survive, got %+v", byName)
	}
}
