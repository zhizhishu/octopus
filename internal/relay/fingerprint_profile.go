package relay

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// resolvedFingerprint is the per-attempt view of which device identity + header
// values to present upstream. It is resolved once from the channel's selected
// fingerprint profile (ChannelCloak.ProfileID) and consulted by the claude /
// codex / generic header paths so they all agree on one device.
//
// hasProfile=false means the channel selected no profile (ProfileID 0) OR the
// selected profile was deleted (dangling id) — both fall back to the GLOBAL
// default: the per-instance seed and the global header settings, byte-for-byte
// the pre-profile behaviour. Every per-field getter likewise falls back to the
// global setting default when the profile leaves that field empty, so a profile
// only overrides the fields it explicitly sets.
type resolvedFingerprint struct {
	hasProfile bool
	profile    dbmodel.FingerprintProfile
}

func (ra *relayAttempt) fingerprint() resolvedFingerprint {
	if ra == nil {
		return resolvedFingerprint{}
	}
	return resolveFingerprintForChannel(ra.channel)
}

// resolveFingerprintForChannel resolves the channel's selected profile. It is the
// single source used by both the relayAttempt method and the free raw-protocol
// path so they agree on one device. A nil channel, ProfileID 0, or a dangling id
// all yield the global-default fingerprint.
func resolveFingerprintForChannel(channel *dbmodel.Channel) resolvedFingerprint {
	if channel == nil {
		return resolvedFingerprint{}
	}
	id := channel.Cloak.ProfileID
	if id <= 0 {
		return resolvedFingerprint{}
	}
	profile, ok := op.FingerprintProfileGet(id)
	if !ok {
		return resolvedFingerprint{}
	}
	return resolvedFingerprint{hasProfile: true, profile: profile}
}

// claudeDeviceID returns the 64-hex claude device id for this fingerprint. The
// profile seed (or the global instance seed when no profile) drives derivation,
// so two profiles yield unrelated devices and the no-profile path is unchanged.
func (f resolvedFingerprint) claudeDeviceID() string {
	if !f.hasProfile {
		return op.ClaudeFingerprintDeviceID()
	}
	return op.ClaudeFingerprintDeviceIDForSeed(f.profile.Seed)
}

// codexInstallationID mirrors claudeDeviceID for the codex install id.
func (f resolvedFingerprint) codexInstallationID() string {
	if !f.hasProfile {
		return op.CodexInstallationID()
	}
	return op.CodexInstallationIDForSeed(f.profile.Seed)
}

// claudeHeader values: profile field if non-empty, else the global setting
// (settingString already applies the static/legacy fallbacks).
func (f resolvedFingerprint) claudeUserAgent() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudeUserAgent }),
		dbmodel.SettingKeyClaudeHeaderUserAgent, defaultClaudeUserAgent)
}

func (f resolvedFingerprint) claudePackageVersion() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudePackageVersion }),
		dbmodel.SettingKeyClaudeHeaderPackage, defaultClaudePackageVersion)
}

func (f resolvedFingerprint) claudeRuntimeVersion() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudeRuntimeVersion }),
		dbmodel.SettingKeyClaudeHeaderRuntime, defaultClaudeRuntimeVersion)
}

func (f resolvedFingerprint) claudeOS() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudeOS }),
		dbmodel.SettingKeyClaudeHeaderOS, defaultClaudeOS)
}

func (f resolvedFingerprint) claudeArch() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudeArch }),
		dbmodel.SettingKeyClaudeHeaderArch, defaultClaudeArch)
}

func (f resolvedFingerprint) claudeTimeout() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.ClaudeTimeout }),
		dbmodel.SettingKeyClaudeHeaderTimeout, defaultClaudeTimeout)
}

// claudeStabilize: profile tri-state override (nil => global setting).
func (f resolvedFingerprint) claudeStabilize() bool {
	if f.hasProfile && f.profile.ClaudeStabilize != nil {
		return *f.profile.ClaudeStabilize
	}
	return settingBool(dbmodel.SettingKeyClaudeHeaderStabilize, true)
}

func (f resolvedFingerprint) codexUserAgent() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.CodexUserAgent }),
		dbmodel.SettingKeyCodexHeaderUserAgent, defaultCodexUserAgent)
}

func (f resolvedFingerprint) codexBetaFeatures() string {
	return f.orSetting(f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.CodexBetaFeatures }),
		dbmodel.SettingKeyCodexHeaderBetaFeatures, defaultCodexBetaFeatures)
}

// codexOriginator: profile override else the hard-coded default (there is no
// global setting for Originator today, matching applyCodexHeaderDefaults).
func (f resolvedFingerprint) codexOriginator() string {
	if v := f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.CodexOriginator }); v != "" {
		return v
	}
	return defaultCodexOriginator
}

// genericUA is the unified UA for non claude/codex channels — empty unless a
// profile sets it (current behaviour: no UA at all on those channels).
func (f resolvedFingerprint) genericUA() string {
	return f.profileValue(func(p dbmodel.FingerprintProfile) string { return p.GenericUA })
}

func (f resolvedFingerprint) profileValue(get func(dbmodel.FingerprintProfile) string) string {
	if !f.hasProfile {
		return ""
	}
	return strings.TrimSpace(get(f.profile))
}

func (f resolvedFingerprint) orSetting(profileValue string, key dbmodel.SettingKey, fallback string) string {
	if profileValue != "" {
		return profileValue
	}
	return settingString(key, fallback)
}
