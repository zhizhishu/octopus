package modeltest

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// resolvedFingerprint mirrors internal/relay's resolver so a CHANNEL/MODEL TEST
// presents exactly the device + headers the relay forward path would for that
// same channel. This upholds the test==real-traffic invariant: if the test used
// the global device while real traffic used the channel's profile device (or
// vice-versa), a strict upstream could see "one device when testing, another when
// serving" and flag it. Both paths resolve through the channel's ProfileID.
//
// hasProfile=false (ProfileID 0 or a dangling/deleted id) falls back to the global
// default — byte-for-byte the pre-profile behaviour, so existing channel tests are
// unchanged.
type resolvedFingerprint struct {
	hasProfile bool
	profile    dbmodel.FingerprintProfile
}

func resolveFingerprint(channel *dbmodel.Channel) resolvedFingerprint {
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

func (f resolvedFingerprint) claudeDeviceID() string {
	if !f.hasProfile {
		return op.ClaudeFingerprintDeviceID()
	}
	return op.ClaudeFingerprintDeviceIDForSeed(f.profile.Seed)
}

func (f resolvedFingerprint) codexInstallationID() string {
	if !f.hasProfile {
		return op.CodexInstallationID()
	}
	return op.CodexInstallationIDForSeed(f.profile.Seed)
}

func (f resolvedFingerprint) claudeUserAgent() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudeUserAgent }),
		dbmodel.SettingKeyClaudeHeaderUserAgent, defaultClaudeUserAgent)
}

func (f resolvedFingerprint) claudePackageVersion() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudePackageVersion }),
		dbmodel.SettingKeyClaudeHeaderPackage, defaultClaudePackageVersion)
}

func (f resolvedFingerprint) claudeRuntimeVersion() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudeRuntimeVersion }),
		dbmodel.SettingKeyClaudeHeaderRuntime, defaultClaudeRuntimeVersion)
}

func (f resolvedFingerprint) claudeOS() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudeOS }),
		dbmodel.SettingKeyClaudeHeaderOS, defaultClaudeOS)
}

func (f resolvedFingerprint) claudeArch() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudeArch }),
		dbmodel.SettingKeyClaudeHeaderArch, defaultClaudeArch)
}

func (f resolvedFingerprint) claudeTimeout() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.ClaudeTimeout }),
		dbmodel.SettingKeyClaudeHeaderTimeout, defaultClaudeTimeout)
}

func (f resolvedFingerprint) claudeStabilize() bool {
	if f.hasProfile && f.profile.ClaudeStabilize != nil {
		return *f.profile.ClaudeStabilize
	}
	return settingBool(dbmodel.SettingKeyClaudeHeaderStabilize, true)
}

func (f resolvedFingerprint) codexUserAgent() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.CodexUserAgent }),
		dbmodel.SettingKeyCodexHeaderUserAgent, defaultCodexUserAgent)
}

func (f resolvedFingerprint) codexBetaFeatures() string {
	return f.orSetting(f.value(func(p dbmodel.FingerprintProfile) string { return p.CodexBetaFeatures }),
		dbmodel.SettingKeyCodexHeaderBetaFeatures, defaultCodexBetaFeatures)
}

func (f resolvedFingerprint) codexOriginator() string {
	if v := f.value(func(p dbmodel.FingerprintProfile) string { return p.CodexOriginator }); v != "" {
		return v
	}
	return defaultCodexOriginator
}

func (f resolvedFingerprint) genericUA() string {
	return f.value(func(p dbmodel.FingerprintProfile) string { return p.GenericUA })
}

func (f resolvedFingerprint) value(get func(dbmodel.FingerprintProfile) string) string {
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
