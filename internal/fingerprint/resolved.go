package fingerprint

import (
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

const (
	defaultClaudeArchitecture = "x64"
	defaultClaudeTimeout      = "600"
	// The Originator must name the same identity family as the User-Agent's leading token:
	// an upstream pairs the two, and a mismatch is itself the tell (sub2api #3901). The default
	// UA is codex_cli_rs, so this fallback must be codex_cli_rs too — it previously said
	// codex_exec, which split the pair for every channel without a profile override.
	defaultCodexOriginator = "codex_cli_rs"
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

// CodexOriginator 取 Originator 头。**必须与 CodexUserAgent 配对**。
//
// 🔴 这里以前直接 `return defaultCodexOriginator`（写死 "codex_cli_rs"），
// 于是有两种把配对拆开的路径：
//
//	① 管理员把 codex_header_defaults_user_agent 改成非 codex_cli_rs 家族的值
//	   （仓库里就有历史值 codex_exec/0.163.0 …）；
//	② 某个 FingerprintProfile 只填了 CodexUserAgent、没填 CodexOriginator。
//
// 两种都让出站变成 `User-Agent: codex_exec/…` + `Originator: codex_cli_rs`。
// 上游的判据正是「Originator 必须等于 UA 首 token」（header_defaults.go 引的
// sub2api #3901）：配对一拆就被判为伪造指纹而拒绝，用户看到的是
// "这条 codex 渠道全部 400/403"，而且**只有改过 UA 的实例中招**，
// 默认实例完全正常——排查方向会被彻底带偏。
//
// 兜底改为从 UA 首 token 推导。这里不能反向 import relay（会造成循环依赖），
// 所以把那一行切分逻辑就地重写，规则与 relay.codexOriginatorFromUserAgent 一致。
func (resolved Resolved) CodexOriginator() string {
	if profileValue := resolved.profileValue(
		func(profile dbmodel.FingerprintProfile) string { return profile.CodexOriginator },
	); profileValue != "" {
		return profileValue
	}
	// 没显式配 originator → 跟随实际要发出去的 UA，保证两者同家族。
	if derived := codexOriginatorFromUserAgent(resolved.CodexUserAgent()); derived != "" {
		return derived
	}
	return defaultCodexOriginator
}

// codexOriginatorFromUserAgent 取 UA 的首个 "/" 之前那段（小写），
// 与 relay.codexOriginatorFromUserAgent 保持同一规则。
// Originator 是裸产品名、不带版本，所以这样切正好。
func codexOriginatorFromUserAgent(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return ""
	}
	token, _, _ := strings.Cut(lower, "/")
	return token
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
