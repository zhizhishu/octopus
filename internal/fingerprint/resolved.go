package fingerprint

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const (
	defaultClaudeArchitecture = "x64"
	defaultClaudeTimeout      = "600"
	defaultCodexOriginator    = "codex_cli_rs"
)

// Resolved is the immutable upstream identity selected for one channel. An
// unresolved or deleted profile intentionally falls back to the instance-wide
// identity and settings, matching the historical relay behavior.
type Resolved struct {
	hasProfile bool
	profile    dbmodel.FingerprintProfile
}

func Resolve(channel *dbmodel.Channel) Resolved {
	if channel == nil || channel.Cloak.ProfileID <= 0 {
		return Resolved{}
	}

	profile, exists := op.FingerprintProfileGet(channel.Cloak.ProfileID)
	if !exists {
		return Resolved{}
	}

	return Resolved{
		hasProfile: true,
		profile:    profile,
	}
}

func (resolved Resolved) HasProfile() bool {
	return resolved.hasProfile
}

func (resolved Resolved) ProfileID() int {
	if !resolved.hasProfile {
		return 0
	}
	return resolved.profile.ID
}

func (resolved Resolved) ProfileName() string {
	if !resolved.hasProfile {
		return ""
	}
	return resolved.profile.Name
}

func (resolved Resolved) ClaudeDeviceID() string {
	if !resolved.hasProfile {
		return op.ClaudeFingerprintDeviceID()
	}
	return op.ClaudeFingerprintDeviceIDForSeed(resolved.profile.Seed)
}

func (resolved Resolved) CodexInstallationID() string {
	if !resolved.hasProfile {
		return op.CodexInstallationID()
	}
	return op.CodexInstallationIDForSeed(resolved.profile.Seed)
}

func (resolved Resolved) ClaudeUserAgent() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudeUserAgent },
		dbmodel.SettingKeyClaudeHeaderUserAgent,
		dbmodel.DefaultClaudeHeaderUserAgent,
	)
}

func (resolved Resolved) ClaudePackageVersion() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudePackageVersion },
		dbmodel.SettingKeyClaudeHeaderPackage,
		dbmodel.DefaultClaudeHeaderPackageVersion,
	)
}

func (resolved Resolved) ClaudeRuntimeVersion() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudeRuntimeVersion },
		dbmodel.SettingKeyClaudeHeaderRuntime,
		dbmodel.DefaultClaudeHeaderRuntimeVersion,
	)
}

func (resolved Resolved) ClaudeOS() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudeOS },
		dbmodel.SettingKeyClaudeHeaderOS,
		dbmodel.DefaultClaudeHeaderOS,
	)
}

func (resolved Resolved) ClaudeArch() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudeArch },
		dbmodel.SettingKeyClaudeHeaderArch,
		defaultClaudeArchitecture,
	)
}

func (resolved Resolved) ClaudeTimeout() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.ClaudeTimeout },
		dbmodel.SettingKeyClaudeHeaderTimeout,
		defaultClaudeTimeout,
	)
}

func (resolved Resolved) ClaudeStabilize() bool {
	if resolved.hasProfile && resolved.profile.ClaudeStabilize != nil {
		return *resolved.profile.ClaudeStabilize
	}
	return settingBool(dbmodel.SettingKeyClaudeHeaderStabilize, true)
}

func (resolved Resolved) CodexUserAgent() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.CodexUserAgent },
		dbmodel.SettingKeyCodexHeaderUserAgent,
		dbmodel.DefaultCodexHeaderUserAgent,
	)
}

func (resolved Resolved) CodexBetaFeatures() string {
	return resolved.profileOrSetting(
		func(profile dbmodel.FingerprintProfile) string { return profile.CodexBetaFeatures },
		dbmodel.SettingKeyCodexHeaderBetaFeatures,
		dbmodel.DefaultCodexHeaderBetaFeatures,
	)
}

func (resolved Resolved) CodexOriginator() string {
	if profileValue := resolved.profileValue(
		func(profile dbmodel.FingerprintProfile) string { return profile.CodexOriginator },
	); profileValue != "" {
		return profileValue
	}
	return defaultCodexOriginator
}

func (resolved Resolved) GenericUA() string {
	return resolved.profileValue(
		func(profile dbmodel.FingerprintProfile) string { return profile.GenericUA },
	)
}

func (resolved Resolved) profileOrSetting(
	valueSelector func(dbmodel.FingerprintProfile) string,
	settingKey dbmodel.SettingKey,
	fallback string,
) string {
	if profileValue := resolved.profileValue(valueSelector); profileValue != "" {
		return profileValue
	}
	return settingString(settingKey, fallback)
}

func (resolved Resolved) profileValue(
	valueSelector func(dbmodel.FingerprintProfile) string,
) string {
	if !resolved.hasProfile {
		return ""
	}
	return strings.TrimSpace(valueSelector(resolved.profile))
}

func settingString(settingKey dbmodel.SettingKey, fallback string) string {
	value, err := op.SettingGetString(settingKey)
	if err != nil {
		return fallback
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func settingBool(settingKey dbmodel.SettingKey, fallback bool) bool {
	value, err := op.SettingGetBool(settingKey)
	if err != nil {
		return fallback
	}
	return value
}
