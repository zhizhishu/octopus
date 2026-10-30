package op

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/cache"
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
	alignCodexOriginatorWithUserAgent(profile)
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
	alignCodexOriginatorWithUserAgent(&current)

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
// builtinPresets() on every boot. Presets 2 and 3 are backfilled while Debian is still
// present; deleting Debian resurrects none.
//
// All three present real devices with three unrelated identities: two Linux machines
// and one Apple Silicon Mac, each running the same two CLI products plus the same
// desktop chat client at different versions. "macOS · Chrome" is the third preset's
// OLD name — see retiredMacOSPresetName for how an older deployment converges.
const (
	builtinDebianPresetName = "Linux · Debian"
	builtinUbuntuPresetName = "Linux · Ubuntu"
	builtinMacOSPresetName  = "macOS · CherryStudio"
	// retiredMacOSPresetName is the OLD canonical name of the third built-in. It used a
	// bare Chrome-on-macOS generic UA and therefore presented a browser on a chat API
	// endpoint, which is itself the tell. It is NOT deleted any more (see the rename
	// block in fingerprintProfileRefreshCache): a row still carrying this name is
	// RENAMED in place to builtinMacOSPresetName and rewritten from the canonical
	// definition, so a deployment upgrading from an older build keeps any channel that
	// had selected it pointing at the same row id.
	retiredMacOSPresetName = "macOS · Chrome"
)

// builtinPresetLegacyNames maps a built-in preset's canonical name to the earlier
// names the SAME row may still carry on a deployment upgrading from an older build
// ("Linux 真机" / "Linux 真机 2 (Ubuntu)" / "macOS · Chrome"). Migration 009 renumbers
// those rows to ids 1/2 resolving them by the same name set, but refreshCache must NOT
// assume 009 has already run — the NAME is the stable identity of a built-in row, so
// resolve by it.
var builtinPresetLegacyNames = map[string][]string{
	builtinDebianPresetName: {"Linux 真机"},
	builtinUbuntuPresetName: {"Linux 真机 2 (Ubuntu)"},
	builtinMacOSPresetName:  {retiredMacOSPresetName},
}

// builtinPresets returns the built-in presets in their CURRENT canonical
// form. This is the SINGLE source of truth for EVERY field of each preset:
// it seeds a fresh deployment AND force-converges an existing built-in row
// (see fingerprintProfileRefreshCache). Bumping a captured CLI identity = editing
// the strings here; every deployment picks it up on its next restart, with no
// per-version migration bolted on.
//
// THREE presets = three distinct devices that all track the captured-latest versions.
// Same claude/codex product families, DIFFERENT deterministic seeds (deriveProfileSeed
// 2 / 3 / 4) => unrelated, stable device_id / installation ids, plus a per-machine codex
// platform segment and a per-machine CherryStudio build so they read as separate
// machines. Assign each to a different channel / upstream key to keep accounts
// uncorrelated.
// The claude anthropic-beta SET is intentionally NOT part of any profile — every
// profile reuses BuildClaudeCodeBetaOrder's canonical order; only the
// version-bearing header strings, the platform pair and the device seed differ.
func builtinPresets() []*model.FingerprintProfile {
	// Each preset gets its OWN *bool, so copying a preset onto a row never makes two
	// rows alias one shared value.
	stabilize := func() *bool { v := true; return &v }
	return []*model.FingerprintProfile{
		{
			Name:                 builtinDebianPresetName,
			Seed:                 deriveProfileSeed(2),
			ClaudeUserAgent:      "claude-cli/2.1.296 (external, sdk-cli)",
			ClaudePackageVersion: "0.128.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "Linux",
			ClaudeArch:           "x64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_exec/0.161.0 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.161.0)",
			CodexOriginator:      "codex_exec",
			CodexBetaFeatures:    "remote_compaction_v2",
			GenericUA:            model.DefaultGenericUA,
		},
		{
			Name:                 builtinUbuntuPresetName,
			Seed:                 deriveProfileSeed(3),
			ClaudeUserAgent:      "claude-cli/2.1.296 (external, sdk-cli)",
			ClaudePackageVersion: "0.128.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "Linux",
			ClaudeArch:           "x64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_exec/0.161.0 (Ubuntu 24.04.1; x86_64) unknown (codex_exec; 0.161.0)",
			CodexOriginator:      "codex_exec",
			CodexBetaFeatures:    "remote_compaction_v2",
			GenericUA:            model.GenericUAUbuntu,
		},
		{
			// Third built-in: the SAME two CLI identities and the SAME CherryStudio
			// desktop client, presented from macOS instead of Linux. Three unrelated
			// devices, not three flavours of one.
			//
			// The codex UA's platform segment is what makes it read as a Mac:
			// `(Mac OS X 10_15_7; arm64)`. Everything else — product token, version,
			// trailing pair — stays in lockstep with the two Linux presets, and the
			// Originator is the UA's leading token, exactly as codexIdentityPair
			// derives it for every other preset (an upstream pairs the two, and a
			// mismatch is itself the tell).
			//
			// Claude side: claude-cli reports its platform through X-Stainless-OS /
			// X-Stainless-Arch, not through the UA, so "macOS" here means Darwin +
			// arm64 in those headers while the UA keeps the identical
			// `claude-cli/2.1.296 (external, sdk-cli)` shape — that string carries no
			// platform token at all.
			Name:                 builtinMacOSPresetName,
			Seed:                 deriveProfileSeed(4),
			ClaudeUserAgent:      "claude-cli/2.1.296 (external, sdk-cli)",
			ClaudePackageVersion: "0.128.0",
			ClaudeRuntimeVersion: "v26.3.0",
			ClaudeOS:             "MacOS",
			ClaudeArch:           "arm64",
			ClaudeTimeout:        "600",
			ClaudeStabilize:      stabilize(),
			CodexUserAgent:       "codex_exec/0.161.0 (Mac OS X 10_15_7; arm64) unknown (codex_exec; 0.161.0)",
			CodexOriginator:      "codex_exec",
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
	// Rename the legacy "macOS · Chrome" built-in in place to its new canonical name.
	// It was the 3rd preset in earlier builds and, for a while, was deleted on every
	// boot; it is a first-class built-in again (2026-10-17) with a CherryStudio identity
	// instead of a bare browser UA. Renaming — rather than dropping and re-seeding —
	// keeps the row id stable, so a channel that had selected it keeps pointing at the
	// SAME identity after the upgrade instead of silently falling back to the global
	// default. Matching is by EXACT legacy name only, never by id / UA / OS / fuzzy
	// name, so a custom mac-flavoured profile under any other name is never touched.
	//
	// The rename happens BEFORE the convergence loop below, which then rewrites every
	// identity field of the (now canonically named) row and restores its own seed —
	// so an older build's Chrome-on-macOS strings converge in the same step.
	for i := range profiles {
		if profiles[i].Name != retiredMacOSPresetName {
			continue
		}
		if err := db.GetDB().WithContext(ctx).Model(&model.FingerprintProfile{}).
			Where("id = ?", profiles[i].ID).
			Update("name", builtinMacOSPresetName).Error; err != nil {
			return fmt.Errorf("failed to rename legacy fingerprint profile %q: %w", retiredMacOSPresetName, err)
		}
		profiles[i].Name = builtinMacOSPresetName
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
	// Instead the built-ins are SYSTEM-MANAGED — builtinPresets() is the single
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
	presets := builtinPresets()
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
	// Seeding rule (unchanged): a FRESH deployment (no profiles at all) gets ALL THREE
	// built-ins; a deployment that already has the 1st built-in gets any missing one
	// backfilled on restart; if an operator DELETED the 1st we resurrect NONE (the
	// deletion is respected). The legacy "macOS · Chrome" name is no longer a separate
	// concept — it is the SAME row as builtinMacOSPresetName and is renamed, not seeded,
	// above.
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

// alignCodexOriginatorWithUserAgent rewrites a Codex User-Agent / Originator pair
// using model.CodexOriginatorFromUserAgent — the same derivation fingerprint
// resolution and the relay use. The save is not rejected.
//
// codex_exec and codex_cli_rs: a non-empty Originator that is not the UA's
// leading token is replaced with that token. An empty Originator stays empty
// so resolution can derive it. A non-codex UA leaves Originator untouched.
// The deprecated codex-tui family is stored as the Rust CLI pair (default
// codex_cli_rs User-Agent, and Originator codex_cli_rs when one was set).
// An empty User-Agent is left alone so a profile that only pins Originator is
// not rewritten against a fallback UA it did not set.
func alignCodexOriginatorWithUserAgent(profile *model.FingerprintProfile) {
	if profile == nil {
		return
	}
	ua := strings.TrimSpace(profile.CodexUserAgent)
	if ua == "" {
		return
	}
	if model.CodexUserAgentIsDeprecatedTUI(ua) {
		profile.CodexUserAgent = model.DefaultCodexHeaderUserAgent
		ua = profile.CodexUserAgent
	}
	derived := model.CodexOriginatorFromUserAgent(ua)
	if derived == "" {
		return
	}
	originator := strings.TrimSpace(profile.CodexOriginator)
	if originator == "" || originator == derived {
		return
	}
	profile.CodexOriginator = derived
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
