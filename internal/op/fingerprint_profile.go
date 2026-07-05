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
	// One-time refresh of the seeded "Linux 真机" preset. An earlier build seeded it with an
	// older captured identity (claude-cli/2.1.186 + codex_exec/0.142.0, node v24.3.0). If a
	// deployment still carries EXACTLY that tuple (i.e. the operator never edited the preset),
	// converge it to the current captured identity (2.1.198 / 0.142.5 / node v26.3.0) so live
	// deployments pick up the refreshed fingerprint on restart without a manual UI edit. The
	// full old tuple is matched, so an operator-customised preset never matches and is left
	// untouched. Package/OS/Arch/Timeout/BetaFeatures are unchanged between the two captures.
	for i := range profiles {
		p := &profiles[i]
		if p.Name == "Linux 真机" &&
			p.ClaudeUserAgent == "claude-cli/2.1.186 (external, sdk-cli)" &&
			p.ClaudeRuntimeVersion == "v24.3.0" &&
			p.CodexUserAgent == "codex_exec/0.142.0 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.142.0)" {
			p.ClaudeUserAgent = "claude-cli/2.1.198 (external, sdk-cli)"
			p.ClaudeRuntimeVersion = "v26.3.0"
			p.CodexUserAgent = "codex_exec/0.142.5 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.142.5)"
			if err := db.GetDB().WithContext(ctx).Save(p).Error; err != nil {
				return fmt.Errorf("failed to refresh Linux 真机 fingerprint profile: %w", err)
			}
		}
	}
	// Converge the built-in Linux presets from the headless codex_exec identity to the
	// interactive codex_cli_rs identity (UA + Originator). anyrouter accepts both, but some
	// upstreams (e.g. muyuan.do) only admit the interactive codex_cli_rs; presenting cli_rs
	// uniformly is the operator's chosen default. Only rows still carrying the EXACT seeded
	// codex_exec/0.142.5 values are rewritten, so an operator-customised codex identity is
	// never overwritten. Runs after the 0.142.0->0.142.5 refresh above so a stale deployment
	// converges in a single restart.
	for i := range profiles {
		p := &profiles[i]
		if p.CodexOriginator == "codex_exec" &&
			p.CodexUserAgent == "codex_exec/0.142.5 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.142.5)" {
			p.CodexUserAgent = "codex_cli_rs/0.142.5 (Debian 12.0.0; x86_64) unknown (codex_cli_rs; 0.142.5)"
			p.CodexOriginator = "codex_cli_rs"
			if err := db.GetDB().WithContext(ctx).Save(p).Error; err != nil {
				return fmt.Errorf("failed to migrate Debian codex identity to codex_cli_rs: %w", err)
			}
		}
		if p.CodexOriginator == "codex_exec" &&
			p.CodexUserAgent == "codex_exec/0.142.5 (Ubuntu 24.04.1; x86_64) unknown (codex_exec; 0.142.5)" {
			p.CodexUserAgent = "codex_cli_rs/0.142.5 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.142.5)"
			p.CodexOriginator = "codex_cli_rs"
			if err := db.GetDB().WithContext(ctx).Save(p).Error; err != nil {
				return fmt.Errorf("failed to migrate Ubuntu codex identity to codex_cli_rs: %w", err)
			}
		}
	}
	// Note: "默认(Windows)" is intentionally NOT a row — the channel dropdown's
	// ProfileID=0 option already IS that identity (per-instance seed + global header
	// settings, byte-for-byte the pre-profile behaviour). An earlier build seeded a
	// redundant all-empty "默认(Windows)" row (making the dropdown show THREE entries);
	// the cleanup above drops it. Each built-in preset's seed is DETERMINISTICALLY
	// derived from the instance seed but DIFFERENT from the global default's and from
	// each other, so every device gets unrelated, stable-across-restart device_id /
	// installation ids that never collide. The claude anthropic-beta SET is
	// intentionally not part of any profile — every profile reuses
	// BuildClaudeCodeBetaOrder's canonical order; only the version-bearing header
	// strings + the device seed differ.
	// One-time rename of the two built-in Linux presets to clearer, unambiguous
	// names. Earlier builds named them "Linux 真机" / "Linux 真机 2 (Ubuntu)"; the
	// "真机" wording read like a third Linux profile sitting next to the dropdown's
	// ProfileID=0 "follow global" entry and confused operators. Rename EXISTING rows
	// in place so already-seeded deployments converge on restart. This MUST run
	// before the hasProfileName seed check below — otherwise the renamed presets look
	// "missing" and get re-seeded as duplicates. Only the exact old preset name is
	// matched, and the rename is skipped if an operator already hand-created a row
	// under the new name (never collide / clobber).
	renamePresets := []struct{ from, to string }{
		{"Linux 真机", "Linux · Debian"},
		{"Linux 真机 2 (Ubuntu)", "Linux · Ubuntu"},
	}
	nameExists := func(name string) bool {
		for i := range profiles {
			if profiles[i].Name == name {
				return true
			}
		}
		return false
	}
	for _, rp := range renamePresets {
		if nameExists(rp.to) {
			continue
		}
		for i := range profiles {
			p := &profiles[i]
			if p.Name == rp.from {
				p.Name = rp.to
				if err := db.GetDB().WithContext(ctx).Model(&model.FingerprintProfile{}).
					Where("id = ?", p.ID).Update("name", rp.to).Error; err != nil {
					return fmt.Errorf("failed to rename fingerprint profile %q->%q: %w", rp.from, rp.to, err)
				}
			}
		}
	}
	// TWO built-in Linux presets — two distinct devices that both track the
	// captured-latest versions (bumped in the migration block above + these seed
	// values on each release). Same claude/codex versions, DIFFERENT deterministic
	// seeds (deriveProfileSeed 2 vs 3) => unrelated, stable device_id / installation
	// ids, and a different Linux distro token in the codex UA (Debian vs Ubuntu) so
	// they read as two separate machines. Assign each to a different channel /
	// upstream key to keep two accounts uncorrelated. The ProfileID=0 "follow global"
	// identity stays a non-row. Seeding rule: a FRESH deployment (no profiles) gets
	// both; a deployment that already has the 1st built-in ("Linux · Debian") but not
	// the 2nd gets the 2nd backfilled on restart; if an operator deleted the 1st we
	// resurrect neither (respect the deletion); an operator-customised same-name
	// profile is never overwritten.
	stabilize := true
	debian := &model.FingerprintProfile{
		Name:                 "Linux · Debian",
		Seed:                 deriveProfileSeed(2),
		ClaudeUserAgent:      "claude-cli/2.1.198 (external, sdk-cli)",
		ClaudePackageVersion: "0.94.0",
		ClaudeRuntimeVersion: "v26.3.0",
		ClaudeOS:             "Linux",
		ClaudeArch:           "x64",
		ClaudeTimeout:        "600",
		ClaudeStabilize:      &stabilize,
		CodexUserAgent:       "codex_cli_rs/0.142.5 (Debian 12.0.0; x86_64) unknown (codex_cli_rs; 0.142.5)",
		CodexOriginator:      "codex_cli_rs",
		CodexBetaFeatures:    "remote_compaction_v2",
	}
	ubuntu := &model.FingerprintProfile{
		Name:                 "Linux · Ubuntu",
		Seed:                 deriveProfileSeed(3),
		ClaudeUserAgent:      "claude-cli/2.1.198 (external, sdk-cli)",
		ClaudePackageVersion: "0.94.0",
		ClaudeRuntimeVersion: "v26.3.0",
		ClaudeOS:             "Linux",
		ClaudeArch:           "x64",
		ClaudeTimeout:        "600",
		ClaudeStabilize:      &stabilize,
		CodexUserAgent:       "codex_cli_rs/0.142.5 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.142.5)",
		CodexOriginator:      "codex_cli_rs",
		CodexBetaFeatures:    "remote_compaction_v2",
	}
	hasProfileName := func(name string) bool {
		for i := range profiles {
			if profiles[i].Name == name {
				return true
			}
		}
		return false
	}
	var toSeed []*model.FingerprintProfile
	if len(profiles) == 0 {
		toSeed = []*model.FingerprintProfile{debian, ubuntu}
	} else if hasProfileName(debian.Name) && !hasProfileName(ubuntu.Name) {
		toSeed = []*model.FingerprintProfile{ubuntu}
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
