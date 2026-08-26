package op

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
)

// fingerprintProfileCache mirrors channelCache: an in-memory copy refreshed at
// boot and kept in sync on writes, so the hot relay path resolves a channel's
// profile without a DB hit.
var fingerprintProfileCache = cache.New[int, model.FingerprintProfile](16)

// FingerprintProfileGet returns the profile for an id. id <= 0 (a channel that
// did not select a profile) and any unknown/dangling id resolve to a zero-value
// profile with ok=false, so callers fall back to the global default fingerprint
// rather than erroring — a deleted profile must never break relay.
func FingerprintProfileGet(id int) (model.FingerprintProfile, bool) {
	if id <= 0 {
		return model.FingerprintProfile{}, false
	}
	return fingerprintProfileCache.Get(id)
}

func FingerprintProfileList(ctx context.Context) ([]model.FingerprintProfile, error) {
	profiles := make([]model.FingerprintProfile, 0, fingerprintProfileCache.Len())
	for _, p := range fingerprintProfileCache.GetAll() {
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func FingerprintProfileCreate(profile *model.FingerprintProfile, ctx context.Context) error {
	if profile == nil {
		return fmt.Errorf("nil profile")
	}
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return fmt.Errorf("profile name is required")
	}
	if strings.TrimSpace(profile.Seed) == "" {
		profile.Seed = generateFingerprintSeed()
	}
	if err := db.GetDB().WithContext(ctx).Create(profile).Error; err != nil {
		return err
	}
	fingerprintProfileCache.Set(profile.ID, *profile)
	return nil
}

func FingerprintProfileUpdate(req *model.FingerprintProfileUpdateRequest, ctx context.Context) (*model.FingerprintProfile, error) {
	if req == nil || req.ID <= 0 {
		return nil, fmt.Errorf("invalid profile id")
	}
	current, ok := fingerprintProfileCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("fingerprint profile not found")
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("profile name cannot be empty")
		}
		current.Name = name
	}
	if req.Seed != nil {
		current.Seed = strings.TrimSpace(*req.Seed)
		if current.Seed == "" {
			current.Seed = generateFingerprintSeed()
		}
	}
	if req.ClaudeUserAgent != nil {
		current.ClaudeUserAgent = *req.ClaudeUserAgent
	}
	if req.ClaudePackageVersion != nil {
		current.ClaudePackageVersion = *req.ClaudePackageVersion
	}
	if req.ClaudeRuntimeVersion != nil {
		current.ClaudeRuntimeVersion = *req.ClaudeRuntimeVersion
	}
	if req.ClaudeOS != nil {
		current.ClaudeOS = *req.ClaudeOS
	}
	if req.ClaudeArch != nil {
		current.ClaudeArch = *req.ClaudeArch
	}
	if req.ClaudeTimeout != nil {
		current.ClaudeTimeout = *req.ClaudeTimeout
	}
	if req.ClaudeStabilize != nil {
		v := *req.ClaudeStabilize
		current.ClaudeStabilize = &v
	}
	if req.CodexUserAgent != nil {
		current.CodexUserAgent = *req.CodexUserAgent
	}
	if req.CodexOriginator != nil {
		current.CodexOriginator = *req.CodexOriginator
	}
	if req.CodexBetaFeatures != nil {
		current.CodexBetaFeatures = *req.CodexBetaFeatures
	}
	if req.GenericUA != nil {
		current.GenericUA = *req.GenericUA
	}

	// Save the whole merged row (Save writes all columns for a record that has a
	// primary key) so a field cleared back to its zero value also persists.
	if err := db.GetDB().WithContext(ctx).Save(&current).Error; err != nil {
		return nil, err
	}
	fingerprintProfileCache.Set(current.ID, current)
	return &current, nil
}

func FingerprintProfileDelete(id int, ctx context.Context) error {
	if id <= 0 {
		return fmt.Errorf("invalid profile id")
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.FingerprintProfile{}, id).Error; err != nil {
		return err
	}
	fingerprintProfileCache.Del(id)
	// NOTE: channels referencing a now-deleted profile keep ProfileID set, but
	// FingerprintProfileGet returns ok=false for it, so relay/modeltest fall back
	// to the global default fingerprint — never a hard error.
	return nil
}

// Canonical names of the built-in presets. A row carrying one of these names is
// SYSTEM-MANAGED: fingerprintProfileRefreshCache forces its identity back to
// builtinLinuxPresets() on every boot. Later presets (Ubuntu, macOS) are
// backfilled when Debian is still present; deleting Debian resurrects none.
const (
	builtinDebianPresetName = "Linux · Debian"
	builtinUbuntuPresetName = "Linux · Ubuntu"
	builtinMacOSPresetName  = "macOS · Chrome"
)

// builtinPresetLegacyNames maps a built-in preset's canonical name to the earlier
// names the SAME row may still carry on a deployment upgrading from an older build
// ("Linux 真机" / "Linux 真机 2 (Ubuntu)"). Migration 009 renumbers those rows to ids
// 1/2 resolving them by the same name set, but refreshCache must NOT assume 009 has
// already run — the NAME is the stable identity of a built-in row, so resolve by it.
var builtinPresetLegacyNames = map[string][]string{
	builtinDebianPresetName: {"Linux 真机"},
	builtinUbuntuPresetName: {"Linux 真机 2 (Ubuntu)"},
}

// builtinLinuxPresets returns the built-in presets in their CURRENT canonical
// form. This is the SINGLE source of truth for EVERY field of each preset:
// it seeds a fresh deployment AND force-converges an existing built-in row
// (see fingerprintProfileRefreshCache). Bumping a captured CLI identity = editing
// the strings here; every deployment picks it up on its next restart, with no
// per-version migration bolted on.
//
// TWO presets = two distinct devices that both track the captured-latest versions.
// Same claude/codex versions, DIFFERENT deterministic seeds (deriveProfileSeed 2 vs
// 3 vs 4) => unrelated, stable device_id / installation ids, and a different OS
// token in the generic UA / claude OS / codex UA so they read as separate machines.
// Assign each to a different channel / upstream key to keep accounts uncorrelated.
// The claude anthropic-beta SET is intentionally NOT part of any profile — every
// profile reuses BuildClaudeCodeBetaOrder's canonical order; only the
// version-bearing header strings + the device seed differ.
func builtinLinuxPresets() []*model.FingerprintProfile {
	// Each preset gets its OWN *bool, so copying a preset onto a row never makes two
	// rows alias one shared value.
	stabilize := func() *bool { v := true; return &v }
	return []*model.FingerprintProfile{
		{
			Name:                 builtinDebianPresetName,
			Seed:                 deriveProfileSeed(2),
			ClaudeUserAgent:      "claude-cli/2.1.212 (external, sdk-cli)",
			ClaudePackageVersion: "0.94.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "Linux",
			ClaudeArch:           "x64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_cli_rs/0.145.0 (Debian 12.0.0; x86_64) unknown (codex_cli_rs; 0.145.0)",
			CodexOriginator:      "codex_cli_rs",
			CodexBetaFeatures:    "remote_compaction_v2",
			GenericUA:            model.DefaultGenericUA,
		},
		{
			Name:                 builtinUbuntuPresetName,
			Seed:                 deriveProfileSeed(3),
			ClaudeUserAgent:      "claude-cli/2.1.212 (external, sdk-cli)",
			ClaudePackageVersion: "0.94.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "Linux",
			ClaudeArch:           "x64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_cli_rs/0.145.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.145.0)",
			CodexOriginator:      "codex_cli_rs",
			CodexBetaFeatures:    "remote_compaction_v2",
			GenericUA:            model.GenericUAUbuntu,
		},
		{
			Name:                 builtinMacOSPresetName,
			Seed:                 deriveProfileSeed(4),
			ClaudeUserAgent:      "claude-cli/2.1.212 (external, sdk-cli)",
			ClaudePackageVersion: "0.94.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "MacOS",
			ClaudeArch:           "x64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_cli_rs/0.145.0 (macOS 10.15.7; x86_64) unknown (codex_cli_rs; 0.145.0)",
			CodexOriginator:      "codex_cli_rs",
			CodexBetaFeatures:    "remote_compaction_v2",
			GenericUA:            model.GenericUAMacOS,
		},
	}
}

// findBuiltinPresetIndex locates the row holding a built-in preset's identity: the
// canonical name first, then any legacy name it may still carry. Preferring the
// canonical name means a deployment that ALSO kept a stale legacy-named row
// converges the canonical one and leaves the other untouched — so converging can
// never collide with the UNIQUE name index. Returns -1 when the preset is absent.
func findBuiltinPresetIndex(profiles []model.FingerprintProfile, canonicalName string) int {
	for i := range profiles {
		if profiles[i].Name == canonicalName {
			return i
		}
	}
	for _, legacy := range builtinPresetLegacyNames[canonicalName] {
		for i := range profiles {
			if profiles[i].Name == legacy {
				return i
			}
		}
	}
	return -1
}

func fingerprintProfileRefreshCache(ctx context.Context) error {
	var profiles []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Find(&profiles).Error; err != nil {
		return fmt.Errorf("failed to load fingerprint profiles: %w", err)
	}
	// One-time upgrade cleanup: an earlier build seeded a redundant "默认(Windows)"
	// profile that is behaviourally identical to the dropdown's ProfileID=0
	// ("默认(Windows)") option, so the dropdown showed THREE entries for the two
	// identities the user wants. That auto-seeded row is uniquely identifiable by its
	// name + EVERY header field empty (it only ever set Name + Seed). Match it
	// SEED-AGNOSTICALLY (the per-instance seed is not reliably persisted, so a
	// seed-equality test could miss it) and delete it, so already-seeded deployments
	// collapse to two. An all-empty profile resolves to the global default regardless,
	// and a channel that had selected it points at a now-missing id which
	// FingerprintProfileGet also resolves to the global default — the SAME identity —
	// so removing it never changes behaviour, even in the unlikely case a user
	// hand-created an all-empty profile under this exact name.
	for _, p := range profiles {
		if p.Name == "默认(Windows)" &&
			p.ClaudeUserAgent == "" && p.ClaudePackageVersion == "" && p.ClaudeRuntimeVersion == "" &&
			p.ClaudeOS == "" && p.ClaudeArch == "" && p.ClaudeTimeout == "" && p.ClaudeStabilize == nil &&
			p.CodexUserAgent == "" && p.CodexOriginator == "" && p.CodexBetaFeatures == "" && p.GenericUA == "" {
			if err := db.GetDB().WithContext(ctx).Delete(&model.FingerprintProfile{}, p.ID).Error; err != nil {
				return fmt.Errorf("failed to drop redundant default fingerprint profile: %w", err)
			}
		}
	}
	// Reload after cleanup so the seed-if-empty check and cache reflect the deletion.
	if err := db.GetDB().WithContext(ctx).Find(&profiles).Error; err != nil {
		return fmt.Errorf("failed to reload fingerprint profiles: %w", err)
	}
	// Force the two built-in Linux presets back to their canonical definition. This
	// replaces what used to be a growing chain of per-version string-match migrations
	// (claude 2.1.186 -> 2.1.198 -> 2.1.212, codex_exec 0.142.0 -> 0.142.5 ->
	// codex_cli_rs 0.142.5 -> 0.144.1 -> 0.145.0, the "真机" rename, the GenericUA
	// backfill): every release bolted on another hop that could never be deleted, and a
	// deployment had to walk the whole chain in order to arrive at today's identity.
	// Instead the built-ins are SYSTEM-MANAGED — builtinLinuxPresets() is the single
	// source of truth and EVERY identity field of the matching row is rewritten from it
	// on each boot, so ANY older shape (legacy name, codex_exec originator, stale
	// claude/codex UA, empty GenericUA) converges in ONE step no matter which version
	// it came from.
	//
	// Deliberate trade-off: an operator edit to a BUILT-IN preset's identity fields is
	// overwritten on the next restart — a custom identity belongs in a NEW profile, and
	// any other name is never touched here. The row's own Seed is the ONE thing kept
	// (rewriting it would change the device_id / installation id the preset already
	// ships); only an EMPTY seed is filled in from the canonical derived seed.
	presets := builtinLinuxPresets()
	for _, canonical := range presets {
		idx := findBuiltinPresetIndex(profiles, canonical.Name)
		if idx < 0 {
			// Absent — the seeding rule below decides whether to create it.
			continue
		}
		p := &profiles[idx]
		id, seed := p.ID, strings.TrimSpace(p.Seed)
		if seed == "" {
			seed = canonical.Seed
		}
		// Copy the WHOLE struct so a newly added identity field converges by itself
		// instead of needing yet another hand-written migration, then restore the
		// primary key and the row's own seed.
		*p = *canonical
		p.ID = id
		p.Seed = seed
		// Save writes every column for a record that has a primary key, so a field the
		// canonical preset leaves empty is cleared rather than silently kept.
		if err := db.GetDB().WithContext(ctx).Save(p).Error; err != nil {
			return fmt.Errorf("failed to converge built-in fingerprint profile %q: %w", p.Name, err)
		}
	}
	// Note: "默认(Windows)" is intentionally NOT a row — the channel dropdown's
	// ProfileID=0 option already IS that identity (per-instance seed + global header
	// settings, byte-for-byte the pre-profile behaviour). An earlier build seeded a
	// redundant all-empty "默认(Windows)" row (making the dropdown show THREE entries);
	// the cleanup above drops it.
	//
	// Seeding rule (unchanged): a FRESH deployment (no profiles at all) gets BOTH
	// built-ins; a deployment that already has the 1st built-in but not the 2nd gets
	// the 2nd backfilled on restart; if an operator DELETED the 1st we resurrect
	// NEITHER (the deletion is respected).
	var toSeed []*model.FingerprintProfile
	switch {
	case len(profiles) == 0:
		toSeed = presets
	case findBuiltinPresetIndex(profiles, presets[0].Name) >= 0:
		for _, preset := range presets[1:] {
			if findBuiltinPresetIndex(profiles, preset.Name) < 0 {
				toSeed = append(toSeed, preset)
			}
		}
	}
	for _, preset := range toSeed {
		if err := db.GetDB().WithContext(ctx).Create(preset).Error; err != nil {
			return fmt.Errorf("failed to seed fingerprint profile %q: %w", preset.Name, err)
		}
		profiles = append(profiles, *preset)
	}
	snapshot := make(map[int]model.FingerprintProfile, len(profiles))
	for _, p := range profiles {
		snapshot[p.ID] = p
	}
	fingerprintProfileCache.ReplaceAll(snapshot)
	return nil
}

func generateFingerprintSeed() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// Extremely unlikely; derive a deterministic fallback off the instance seed
		// so it is at least unique per deployment.
		return "fp-fallback-" + FingerprintInstanceID()
	}
	return hex.EncodeToString(buf)
}

// deriveProfileSeed produces a stable 64-hex seed for a preset profile index,
// derived from the global instance seed so it is unique per deployment, stable
// across restarts, and DIFFERENT from the instance seed itself (and from other
// indices) — so preset profiles never share a device_id with the global default
// or each other.
func deriveProfileSeed(index int) string {
	sum := sha256.Sum256([]byte("octopus:fp-profile:" + FingerprintInstanceID() + ":" + strconv.Itoa(index)))
	return hex.EncodeToString(sum[:])
}
