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
	// profile (all header fields empty + Seed = the per-instance seed) that is
	// byte-for-byte identical to the dropdown's ProfileID=0 ("默认(Windows)") option,
	// so the dropdown showed THREE entries for the two identities. Delete ONLY that
	// exact auto-seeded row (the strict empty-field + instance-seed signature never
	// matches a user-customised profile) so already-seeded deployments collapse to
	// two. A channel that had selected it points at a now-missing id, which
	// FingerprintProfileGet resolves to the global default — the SAME identity — so
	// behaviour is byte-for-byte unchanged.
	instanceSeed := FingerprintInstanceID()
	for _, p := range profiles {
		if p.Name == "默认(Windows)" && p.Seed == instanceSeed &&
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
	// Seed ONE built-in profile ("Linux 真机") on first boot. The "default
	// (Windows)" identity is NOT a row: the channel dropdown's ProfileID=0 option
	// already IS that — it resolves to the per-instance seed + global header
	// settings, byte-for-byte the pre-profile behaviour. Seeding a redundant
	// "default" row made the dropdown show THREE entries (跟随全局 + 默认 + Linux)
	// for what the user wants as TWO identities, so we drop it. Result: the dropdown
	// is "默认(Windows)" (=ProfileID 0) + "Linux 真机". Only seed when empty so an
	// operator-edited deployment is never overwritten.
	//
	// "Linux 真机": the packet-captured second identity (claude-cli 2.1.186 /
	// codex_exec 0.142.0 on Linux/Debian). Its seed is DETERMINISTICALLY derived
	// from the instance seed but DIFFERENT from the global default's, so the two
	// devices get unrelated, stable-across-restart device_id / installation ids
	// that never collide. The claude anthropic-beta SET is intentionally not part of
	// a profile — both reuse BuildClaudeCodeBetaOrder's canonical order; only the
	// version-bearing header strings differ.
	if len(profiles) == 0 {
		stabilize := true
		presets := []*model.FingerprintProfile{
			{
				Name:                 "Linux 真机",
				Seed:                 deriveProfileSeed(2),
				ClaudeUserAgent:      "claude-cli/2.1.186 (external, sdk-cli)",
				ClaudePackageVersion: "0.94.0",
				ClaudeRuntimeVersion: "v24.3.0",
				ClaudeOS:             "Linux",
				ClaudeArch:           "x64",
				ClaudeTimeout:        "600",
				ClaudeStabilize:      &stabilize,
				CodexUserAgent:       "codex_exec/0.142.0 (Debian 12.0.0; x86_64) unknown (codex_exec; 0.142.0)",
				CodexOriginator:      "codex_exec",
				CodexBetaFeatures:    "remote_compaction_v2",
			},
		}
		for _, preset := range presets {
			if err := db.GetDB().WithContext(ctx).Create(preset).Error; err != nil {
				return fmt.Errorf("failed to seed fingerprint profile %q: %w", preset.Name, err)
			}
			profiles = append(profiles, *preset)
		}
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
