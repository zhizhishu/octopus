package model

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type SettingKey string

const (
	SettingKeyProxyURL                  SettingKey = "proxy_url"
	SettingKeyStatsSaveInterval         SettingKey = "stats_save_interval"          // 将统计信息写入数据库的周期(分钟)
	SettingKeyStatsTimezone             SettingKey = "stats_timezone"               // 统计"当天/小时"按此 IANA 时区(如 America/Los_Angeles)划分自然日; 空=跟随容器本地时区(向后兼容)。修复容器时区领先用户时区时"今天"统计被提前清零
	SettingKeyModelInfoUpdateInterval   SettingKey = "model_info_update_interval"   // 模型信息更新间隔(小时)
	SettingKeySyncLLMInterval           SettingKey = "sync_llm_interval"            // LLM 同步间隔(小时)
	SettingKeyRelayLogKeepPeriod        SettingKey = "relay_log_keep_period"        // 日志保存时间范围(天)
	SettingKeyRelayLogKeepEnabled       SettingKey = "relay_log_keep_enabled"       // 是否保留历史日志
	SettingKeyRelayLogMaxStorageGB      SettingKey = "relay_log_max_storage_gb"     // 日志最大保存容量(GB), 0 表示不限制
	SettingKeyAnthropicAutoCacheControl SettingKey = "anthropic_auto_cache_control" // Anthropic 出站自动为稳定长前缀注入 cache_control
	SettingKeyRelayStreamKeepaliveSec   SettingKey = "relay_stream_keepalive_interval_seconds"
	SettingKeyOpenAIAutoPromptCacheKey  SettingKey = "openai_auto_prompt_cache_key"
	SettingKeyUpstreamUTLSFingerprint   SettingKey = "upstream_utls_fingerprint" // 直连上游用 Chrome uTLS ClientHello (JA3); opt-in, 默认关, 启用前须过 the relay 复验
	SettingKeyRelayStreamDataTimeoutSec SettingKey = "relay_stream_data_interval_timeout_seconds"
	SettingKeyResponsesSessionTTL       SettingKey = "responses_session_ttl_seconds"
	SettingKeyClaudeHeaderUserAgent     SettingKey = "claude_header_defaults_user_agent"
	SettingKeyClaudeHeaderPackage       SettingKey = "claude_header_defaults_package_version"
	SettingKeyClaudeHeaderRuntime       SettingKey = "claude_header_defaults_runtime_version"
	SettingKeyClaudeHeaderOS            SettingKey = "claude_header_defaults_os"
	SettingKeyClaudeHeaderArch          SettingKey = "claude_header_defaults_arch"
	SettingKeyClaudeHeaderTimeout       SettingKey = "claude_header_defaults_timeout"
	SettingKeyClaudeHeaderStabilize     SettingKey = "claude_header_defaults_stabilize_device_profile"
	SettingKeyClaudeCLIAutoCompact      SettingKey = "claude_cli_auto_compact"
	SettingKeyClaudeCLIReasoningEffort  SettingKey = "claude_cli_reasoning_effort"
	// SettingKeyClaudeBetaStripFlags is an OPT-IN escape hatch (default empty = OFF).
	// When empty, octopus faithfully forwards the downstream claude-cli's anthropic-beta
	// verbatim (unchanged behaviour). When set to a comma-separated list of beta flags,
	// those flags are stripped from the outbound anthropic-beta — used to drop a flag that
	// trips an upstream (e.g. the relay's intermittent 520 on prompt-caching-scope-2026-01-05)
	// without disturbing the rest of the genuine beta shape.
	SettingKeyClaudeBetaStripFlags SettingKey = "claude_beta_strip_flags"
	// SettingKeyFingerprintInstanceID is a per-deployment random seed (generated once,
	// persisted) from which octopus derives a SINGLE upstream device fingerprint
	// (claude device_id / codex installation id) for ALL relayed traffic — uniform
	// across users/api-keys/upstreams so an upstream never sees multiple devices just
	// because requests pass through octopus.
	SettingKeyFingerprintInstanceID     SettingKey = "fingerprint_instance_id"
	SettingKeyCodexHeaderUserAgent      SettingKey = "codex_header_defaults_user_agent"
	SettingKeyCodexHeaderBetaFeatures   SettingKey = "codex_header_defaults_beta_features"
	SettingKeyCodexFastMode             SettingKey = "codex_fast_mode"
	SettingKeyUserRegistrationEnabled   SettingKey = "user_registration_enabled"    // 是否允许无邀请码直接注册
	SettingKeyCORSAllowOrigins          SettingKey = "cors_allow_origins"           // 跨域白名单(逗号分隔, 如 "example.com,example2.com"). 为空不允许跨域, "*"允许所有
	SettingKeyCircuitBreakerThreshold   SettingKey = "circuit_breaker_threshold"    // 熔断触发阈值（连续失败次数）
	SettingKeyCircuitBreakerCooldown    SettingKey = "circuit_breaker_cooldown"     // 熔断基础冷却时间（秒）
	SettingKeyCircuitBreakerMaxCooldown SettingKey = "circuit_breaker_max_cooldown" // 熔断最大冷却时间（秒），指数退避上限
	SettingKeyDebugLoadBalancer         SettingKey = "debug_load_balancer"          // 开启后每次选路打印候选排序决策(tier/rank/容量输入)，便于排障"为啥走这条/为啥慢"
	SettingKeySessionKeepTimeDefault    SettingKey = "session_keep_time_default"    // 分组会话保持时间全局默认(秒)，分组级为0时回退用它，0=不启用全局粘性
	SettingKeyFirstTokenTimeOutDefault  SettingKey = "first_token_time_out_default" // 分组首字超时全局默认(秒)，分组级为0时回退用它，0=不启用全局默认
	// SettingKeyFirstByteKeepaliveDelaySeconds defaults to "20" (ON). When >0, stream
	// requests that have not yet received the first upstream response byte start injecting
	// ignorable SSE comment heartbeats (":\n\n") to the downstream client after this many
	// seconds, continuing every relay_stream_keepalive_interval_seconds until the upstream
	// response headers arrive. This keeps a downstream client (e.g. Cursor's ~60s idle
	// timeout) connected through a slow-first-token upstream, and stops a front-end reverse
	// proxy from dropping the idle connection during long upstream TTFB. Fast upstreams
	// (<delay to first byte) never trigger it, so their behaviour is byte-identical. It is
	// downstream-only and never touches the codex/claude/gemini upstream fingerprint. Set
	// to "0" to disable entirely.
	SettingKeyFirstByteKeepaliveDelaySeconds SettingKey = "first_byte_keepalive_delay_seconds"
	SettingKeyRouteModeOverride              SettingKey = "route_mode_override" // 路由模式全局覆盖: ""=跟随分组各自模式, "spread"=强制轮询, "fill_first"=强制优先填充
	SettingKeyPromptOverrideSystem           SettingKey = "prompt_override_system"
	SettingKeyPromptOverrideMode             SettingKey = "prompt_override_mode"
	SettingKeyUpstreamErrorStatusPass        SettingKey = "upstream_error_status_passthrough"
	SettingKeyUpstreamErrorBodyMode          SettingKey = "upstream_error_body_mode"
	SettingKeyUpstreamErrorCustom            SettingKey = "upstream_error_custom_message"
	SettingKeyUpstreamErrorPublicCode        SettingKey = "upstream_error_public_code"
	SettingKeyCheckInEnabled                 SettingKey = "checkin_enabled"
	SettingKeyCheckInRewardMode              SettingKey = "checkin_reward_mode"
	SettingKeyCheckInRewardAmount            SettingKey = "checkin_reward_amount"
	SettingKeyCheckInRewardMin               SettingKey = "checkin_reward_min"
	SettingKeyCheckInRewardMax               SettingKey = "checkin_reward_max"
	SettingKeyEmailVerificationEnabled       SettingKey = "email_verification_enabled"
	SettingKeyEmailSMTPHost                  SettingKey = "email_smtp_host"
	SettingKeyEmailSMTPPort                  SettingKey = "email_smtp_port"
	SettingKeyEmailSMTPUser                  SettingKey = "email_smtp_user"
	SettingKeyEmailSMTPPassword              SettingKey = "email_smtp_password"
	SettingKeyEmailSMTPFrom                  SettingKey = "email_smtp_from"
	SettingKeyEmailSMTPFromName              SettingKey = "email_smtp_from_name"
	SettingKeyEmailSMTPSSL                   SettingKey = "email_smtp_ssl"
	SettingKeyEmailProvider                  SettingKey = "email_provider" // "smtp" | "http"
	SettingKeyEmailHTTPBaseURL               SettingKey = "email_http_base_url"
	SettingKeyEmailHTTPFrom                  SettingKey = "email_http_from"
	SettingKeyEmailHTTPAdminAuth             SettingKey = "email_http_admin_auth" // secret
	SettingKeyEmailHTTPSiteAuth              SettingKey = "email_http_site_auth"  // secret
	// SettingKeyAdminToken is an OPTIONAL long-lived admin access token for
	// automation/CLI that cannot hold a short-lived login JWT. Empty (default) =
	// disabled; when set it grants full admin via the Auth() middleware fallback.
	// Stored masked (secret) and injectable from the <APP>_ADMIN_TOKEN env at runtime
	// so a public-repo deployment never hardcodes it.
	SettingKeyAdminToken SettingKey = "admin_access_token" // secret
)

type Setting struct {
	Key   SettingKey `json:"key" gorm:"primaryKey"`
	Value string     `json:"value" gorm:"not null"`
}

// SettingSecretMaskValue is returned by the settings API in place of stored
// secrets (e.g. SMTP password) and is treated as "keep existing" on write.
const SettingSecretMaskValue = "__OCTOPUS_SECRET_KEPT__"

const (
	// DefaultCodexHeaderUserAgent: a clean, self-consistent codex_cli_rs identity on a Linux
	// box. Packet-verified 2026-07-10 against real codex 0.144.1 on /backend-api/codex/responses:
	// a genuine codex_cli_rs client carries codex_cli_rs in BOTH the leading token and the
	// trailing (codex_cli_rs; <ver>) — the asymmetric (codex_exec) trailing produced by forcing
	// CODEX_INTERNAL_ORIGINATOR_OVERRIDE is a "ran exec + overrode originator" tell, not this.
	// Only the version advanced 0.142.5 -> 0.144.1; the header set/order was otherwise unchanged.
	DefaultCodexHeaderUserAgent = "codex_cli_rs/0.144.1 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.144.1)"

	// DefaultClaudeCLIVersion is the named Claude Code CLI version used to build the
	// user-agent below. The Anthropic outbound billing-header cc_version carries the
	// same version (authropic.ClaudeCLIVersion); TestClaudeFingerprintVersionConsistency
	// asserts the two never drift apart.
	DefaultClaudeCLIVersion = "2.1.198"

	// DefaultClaudeHeaderUserAgent is the locally packet-verified Claude Code CLI
	// user-agent (claude-cli/<DefaultClaudeCLIVersion>).
	DefaultClaudeHeaderUserAgent = "claude-cli/" + DefaultClaudeCLIVersion + " (external, sdk-cli)"

	DefaultRelayStreamDataIntervalTimeoutSeconds       = "900"
	LegacyDefaultRelayStreamDataIntervalTimeoutSeconds = "180"

	// LegacyDefaultCodexHeaderUserAgent0133 was briefly shipped as the Codex
	// header default. Treat it as a product default, not as an administrator's
	// custom value, so upgraded deployments converge to the locally verified
	// Codex CLI fingerprint without requiring a manual settings edit.
	LegacyDefaultCodexHeaderUserAgent0133 = "codex_exec/0.133.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.133.0)"

	// LegacyDefaultClaudeHeaderUserAgent2168 was the previous Claude header default
	// (claude-cli/2.1.168). Treat it as a product default, not an admin custom value,
	// so upgraded deployments converge to the current fingerprint without a manual edit.
	LegacyDefaultClaudeHeaderUserAgent2168 = "claude-cli/2.1.168 (external, sdk-cli)"

	// LegacyDefaultClaudeHeaderUserAgent2126 was an even older Claude header default
	// (claude-cli/2.1.126, the claude-vscode agent-sdk build). Treat it as a product
	// default so upgraded deployments converge to DefaultClaudeHeaderUserAgent via the DB
	// legacy-upgrade map (single authority), not only via a read-time patch.
	LegacyDefaultClaudeHeaderUserAgent2126 = "claude-cli/2.1.126 (external, claude-vscode, agent-sdk/0.2.126)"

	// LegacyDefaultClaudeHeaderUserAgent2178 was the previous Claude header default
	// (claude-cli/2.1.178). 2.1.198 superseded it (it dropped the billing cch field and
	// the advisor-tool/structured-outputs betas); converge upgraded deployments to the
	// current UA via the DB legacy-upgrade map.
	LegacyDefaultClaudeHeaderUserAgent2178 = "claude-cli/2.1.178 (external, sdk-cli)"

	// DefaultClaudeHeaderPackageVersion is the current X-Stainless-Package-Version
	// default (the value seeded into SettingKeyClaudeHeaderPackage). Unchanged from the
	// 2.1.178 wire: 2.1.198 still reports 0.94.0.
	DefaultClaudeHeaderPackageVersion = "0.94.0"

	// DefaultClaudeHeaderRuntimeVersion is the current X-Stainless-Runtime-Version default
	// — the bundled node runtime a genuine claude-cli/2.1.198 reports on the wire.
	DefaultClaudeHeaderRuntimeVersion = "v26.3.0"

	// LegacyDefaultClaudeHeaderRuntimeV2430 was the X-Stainless-Runtime-Version paired with
	// the 2.1.178 UA (node v24.3.0). 2.1.198 bundles node v26.3.0; converge upgraded
	// deployments so the runtime version does not drift apart from the UA.
	LegacyDefaultClaudeHeaderRuntimeV2430 = "v24.3.0"

	// LegacyDefaultClaudeHeaderPackage0810 was the X-Stainless-Package-Version paired
	// with the 2.1.126 UA above; migrate it to DefaultClaudeHeaderPackageVersion.
	LegacyDefaultClaudeHeaderPackage0810 = "0.81.0"

	// DefaultClaudeHeaderOS is the X-Stainless-OS default (seeded into
	// SettingKeyClaudeHeaderOS). Flipped Windows->Linux so the global-default machine is a
	// coherent Linux box — matching the codex default (codex_cli_rs on Ubuntu) and the two
	// built-in Linux real-machine profiles, so a channel with no pinned profile no longer
	// presents a lone Windows fingerprint. The X-Stainless-OS value is not part of the
	// header set/order an upstream shape-checks, and Linux claude-cli is already proven on
	// the relay via the Linux profiles.
	DefaultClaudeHeaderOS = "Linux"

	// LegacyDefaultClaudeHeaderOSWindows was the previous X-Stainless-OS seed default.
	// Deployments still holding the exact old seed value converge to DefaultClaudeHeaderOS
	// via the DB at startup. Because "Windows" doubles as a value an admin could pick on
	// purpose, this only auto-flips the untouched seed; any OTHER stored value (a custom
	// edit) is preserved. If an admin re-selects Windows in the editor it will re-flip on
	// next restart — remove this entry to make Windows sticky again.
	LegacyDefaultClaudeHeaderOSWindows = "Windows"

	// LegacyDefaultCodexHeaderUserAgentCliRs0114 was an early Codex header default
	// (codex_cli_rs/0.114.0 on macOS) shipped before the locally packet-verified
	// codex_exec Windows fingerprint. Deployments seeded with it stayed pinned to the
	// stale macOS UA across image updates because it was never on the upgrade list;
	// treat it as a product default so it converges to DefaultCodexHeaderUserAgent.
	LegacyDefaultCodexHeaderUserAgentCliRs0114 = "codex_cli_rs/0.114.0 (Mac OS 14.2.0; x86_64) vscode/1.111.0"

	// LegacyDefaultCodexHeaderUserAgent0132 was the previous Codex header default
	// (codex_exec/0.132.0 on Windows). 0.142.5 superseded it; converge upgraded deployments.
	LegacyDefaultCodexHeaderUserAgent0132 = "codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)"

	// LegacyDefaultCodexHeaderUserAgentExec0142Win was the Codex header default before the
	// switch to the interactive codex_cli_rs identity (codex_exec/0.142.5 on Windows). Treat
	// it as a product default so upgraded deployments converge to the current codex_cli_rs
	// default instead of staying pinned to the headless codex_exec UA.
	LegacyDefaultCodexHeaderUserAgentExec0142Win = "codex_exec/0.142.5 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.142.5)"

	// LegacyDefaultCodexHeaderUserAgentCliRs0142 was the codex_cli_rs default before the
	// 0.144.1 version bump (2026-07-10). Kept so already-seeded deployments converge to the
	// current default on restart via op.settingLegacyDefaultUpgrades.
	LegacyDefaultCodexHeaderUserAgentCliRs0142 = "codex_cli_rs/0.142.5 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.142.5)"

	// DefaultCodexHeaderBetaFeatures is the current Codex beta-feature header value
	// (codex_exec 0.142.5, packet-verified on the wire).
	DefaultCodexHeaderBetaFeatures = "remote_compaction_v2"

	// LegacyDefaultCodexHeaderBetaFeaturesMultiAgent was the Codex beta-feature
	// default paired with the early macOS UA above; migrate it to the current value.
	LegacyDefaultCodexHeaderBetaFeaturesMultiAgent = "multi_agent"

	// LegacyDefaultCodexHeaderBetaFeaturesTerminalResizeReflow was the Codex beta-feature
	// default paired with the codex_exec 0.132.0 UA; 0.142.5 replaced it with
	// remote_compaction_v2. Converge upgraded deployments to the current value.
	LegacyDefaultCodexHeaderBetaFeaturesTerminalResizeReflow = "terminal_resize_reflow"
)

func DefaultSettings() []Setting {
	return []Setting{
		{Key: SettingKeyProxyURL, Value: ""},
		{Key: SettingKeyStatsSaveInterval, Value: "10"},           // 默认10分钟保存一次统计信息
		{Key: SettingKeyStatsTimezone, Value: ""},                 // 空=跟随容器本地时区(向后兼容); 设 IANA 名(如 America/Los_Angeles)则统计按该时区划分自然日, 修复"服务器跨天导致今天统计清零"
		{Key: SettingKeyCORSAllowOrigins, Value: ""},              // CORS 默认不允许跨域，设置为 "*" 才允许所有来源
		{Key: SettingKeyModelInfoUpdateInterval, Value: "24"},     // 默认24小时更新一次模型信息
		{Key: SettingKeySyncLLMInterval, Value: "24"},             // 默认24小时同步一次LLM
		{Key: SettingKeyRelayLogKeepPeriod, Value: "30"},          // 默认日志保存30天(与日志页“1个月”快捷范围对齐；旧版7天会让“1个月”只查到7天)
		{Key: SettingKeyRelayLogKeepEnabled, Value: "true"},       // 默认保留历史日志
		{Key: SettingKeyRelayLogMaxStorageGB, Value: "0"},         // 默认不按容量裁剪，避免升级后意外删除日志
		{Key: SettingKeyAnthropicAutoCacheControl, Value: "true"}, // 默认开启稳定前缀缓存断点，提升 provider 原生命中率
		{Key: SettingKeyRelayStreamKeepaliveSec, Value: defaultRelayStreamKeepaliveIntervalSeconds()},
		{Key: SettingKeyOpenAIAutoPromptCacheKey, Value: "true"},
		{Key: SettingKeyUpstreamUTLSFingerprint, Value: "false"}, // 默认关：改 TLS 指纹影响所有上游，须先过 the relay 复验再开
		{Key: SettingKeyRelayStreamDataTimeoutSec, Value: defaultRelayStreamDataIntervalTimeoutSeconds()},
		{Key: SettingKeyResponsesSessionTTL, Value: "3600"},
		{Key: SettingKeyClaudeHeaderUserAgent, Value: DefaultClaudeHeaderUserAgent},
		{Key: SettingKeyClaudeHeaderPackage, Value: DefaultClaudeHeaderPackageVersion},
		{Key: SettingKeyClaudeHeaderRuntime, Value: DefaultClaudeHeaderRuntimeVersion},
		{Key: SettingKeyClaudeHeaderOS, Value: DefaultClaudeHeaderOS},
		{Key: SettingKeyClaudeHeaderArch, Value: "x64"},
		{Key: SettingKeyClaudeHeaderTimeout, Value: "600"},
		{Key: SettingKeyClaudeHeaderStabilize, Value: "true"},
		{Key: SettingKeyClaudeCLIAutoCompact, Value: "false"},
		{Key: SettingKeyClaudeCLIReasoningEffort, Value: "auto"},
		{Key: SettingKeyClaudeBetaStripFlags, Value: ""}, // 默认空=关：忠实透传 claude-cli 的 anthropic-beta；配置了才剥离指定 flag（the relay 抽风逃生）
		{Key: SettingKeyCodexHeaderUserAgent, Value: DefaultCodexHeaderUserAgent},
		{Key: SettingKeyCodexHeaderBetaFeatures, Value: DefaultCodexHeaderBetaFeatures},
		{Key: SettingKeyCodexFastMode, Value: "false"},
		{Key: SettingKeyUserRegistrationEnabled, Value: "false"},                                        // 默认只允许邀请码注册
		{Key: SettingKeyCircuitBreakerThreshold, Value: "10"},                                           // 默认连续失败10次触发熔断
		{Key: SettingKeyCircuitBreakerCooldown, Value: "30"},                                            // 默认基础冷却30秒
		{Key: SettingKeyCircuitBreakerMaxCooldown, Value: "600"},                                        // 默认最大冷却600秒（10分钟）
		{Key: SettingKeyDebugLoadBalancer, Value: "false"},                                              // 默认关闭选路决策日志
		{Key: SettingKeySessionKeepTimeDefault, Value: "0"},                                             // 默认0=不启用全局粘性(向后兼容); 管理员设为如3600才全局开, 分组级 SessionKeepTime 仍优先
		{Key: SettingKeyFirstTokenTimeOutDefault, Value: "0"},                                           // 默认0=不启用全局默认(向后兼容); 分组级 FirstTokenTimeOut 仍优先
		{Key: SettingKeyFirstByteKeepaliveDelaySeconds, Value: defaultFirstByteKeepaliveDelaySeconds()}, // 默认20=开启: 上游首字节>20s才向下游注入SSE心跳(防前置反代/客户端60s空闲掐断); 0=关闭
		{Key: SettingKeyRouteModeOverride, Value: ""},                                                   // 默认空=跟随分组各自模式(向后兼容); 设为 spread/fill_first 则强制覆盖所有分组
		{Key: SettingKeyPromptOverrideSystem, Value: ""},
		{Key: SettingKeyPromptOverrideMode, Value: string(PromptOverrideModeAppendSystem)},
		{Key: SettingKeyUpstreamErrorStatusPass, Value: "false"},
		{Key: SettingKeyUpstreamErrorBodyMode, Value: "redacted_upstream"},
		{Key: SettingKeyUpstreamErrorCustom, Value: "Upstream request failed. Please try again later."},
		{Key: SettingKeyUpstreamErrorPublicCode, Value: "service_busy"},
		{Key: SettingKeyCheckInEnabled, Value: "false"},
		{Key: SettingKeyCheckInRewardMode, Value: "fixed"},
		{Key: SettingKeyCheckInRewardAmount, Value: "100"},
		{Key: SettingKeyCheckInRewardMin, Value: "100"},
		{Key: SettingKeyCheckInRewardMax, Value: "200"},
		{Key: SettingKeyEmailVerificationEnabled, Value: "false"},
		{Key: SettingKeyEmailSMTPHost, Value: ""},
		{Key: SettingKeyEmailSMTPPort, Value: "587"},
		{Key: SettingKeyEmailSMTPUser, Value: ""},
		{Key: SettingKeyEmailSMTPPassword, Value: ""},
		{Key: SettingKeyEmailSMTPFrom, Value: ""},
		{Key: SettingKeyEmailSMTPFromName, Value: "Octopus"},
		{Key: SettingKeyEmailSMTPSSL, Value: "false"},
		{Key: SettingKeyEmailProvider, Value: "smtp"},
		{Key: SettingKeyEmailHTTPBaseURL, Value: ""},
		{Key: SettingKeyEmailHTTPFrom, Value: ""},
		{Key: SettingKeyEmailHTTPAdminAuth, Value: ""},
		{Key: SettingKeyEmailHTTPSiteAuth, Value: ""},
		{Key: SettingKeyAdminToken, Value: ""},
	}
}

// IsSecretSettingKey reports whether a setting holds a credential that must be
// masked in the settings API and preserved (not overwritten) when the client
// sends back the mask sentinel.
func IsSecretSettingKey(key SettingKey) bool {
	switch key {
	case SettingKeyEmailSMTPPassword, SettingKeyEmailHTTPAdminAuth, SettingKeyEmailHTTPSiteAuth, SettingKeyAdminToken:
		return true
	}
	return false
}

// ProxyURLPasswordMask is the placeholder substituted for the password
// component of SettingKeyProxyURL in the settings API. Unlike a full secret,
// the proxy URL is only partially redacted: scheme/host/port/path/user stay
// visible so an admin can see and edit them, while the password is hidden.
const ProxyURLPasswordMask = "***"

// RedactProxyURLPassword returns raw with only the userinfo password component
// replaced by ProxyURLPasswordMask, preserving scheme/host/port/path/query and
// the username. If raw is empty, has no userinfo password, or fails to parse,
// it is returned unchanged.
func RedactProxyURLPassword(raw string) string {
	if raw == "" {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	if _, hasPassword := parsed.User.Password(); !hasPassword {
		return raw
	}
	// url.URL.String() percent-encodes characters in the userinfo password
	// (e.g. "***" becomes "%2A%2A%2A"). Rebuild the userinfo by hand off the
	// authority-less URL string so the mask stays literal, preserving the
	// (re-escaped) username plus scheme/host/port/path/query.
	username := url.User(parsed.User.Username()).String()
	parsed.User = nil
	withoutUser := parsed.String()
	scheme := parsed.Scheme + "://"
	if !strings.HasPrefix(withoutUser, scheme) {
		// Defensive: unexpected shape (e.g. opaque URL); fall back to the
		// encoded-but-correct userinfo form rather than emit a malformed URL.
		parsed.User = url.UserPassword(username, ProxyURLPasswordMask)
		return parsed.String()
	}
	return scheme + username + ":" + ProxyURLPasswordMask + "@" + withoutUser[len(scheme):]
}

// MergeProxyURLPassword reconciles an incoming proxy URL (which may carry the
// ProxyURLPasswordMask placeholder produced by RedactProxyURLPassword) with the
// currently stored value so the admin can edit host/user/path without retyping
// the password.
//
//   - If incoming's userinfo password is the placeholder and stored has a real
//     password, incoming is returned with the stored password substituted back in.
//   - If incoming's userinfo password is the placeholder but stored has no
//     password (or fails to parse), the placeholder password is stripped from
//     incoming (returned without a password).
//   - Otherwise (incoming has a real password, no password, or fails to parse)
//     incoming is returned unchanged.
func MergeProxyURLPassword(incoming, stored string) string {
	parsedIncoming, err := url.Parse(incoming)
	if err != nil || parsedIncoming.User == nil {
		return incoming
	}
	incomingPassword, hasIncomingPassword := parsedIncoming.User.Password()
	if !hasIncomingPassword || incomingPassword != ProxyURLPasswordMask {
		return incoming
	}

	username := parsedIncoming.User.Username()
	if parsedStored, errStored := url.Parse(stored); errStored == nil && parsedStored.User != nil {
		if storedPassword, hasStoredPassword := parsedStored.User.Password(); hasStoredPassword {
			parsedIncoming.User = url.UserPassword(username, storedPassword)
			return parsedIncoming.String()
		}
	}

	// No real stored password to restore: drop the placeholder so it is never persisted.
	parsedIncoming.User = url.User(username)
	return parsedIncoming.String()
}

func (s *Setting) Validate() error {
	switch s.Key {
	case SettingKeyModelInfoUpdateInterval, SettingKeySyncLLMInterval, SettingKeyRelayLogKeepPeriod,
		SettingKeyCircuitBreakerThreshold, SettingKeyCircuitBreakerCooldown, SettingKeyCircuitBreakerMaxCooldown:
		_, err := strconv.Atoi(s.Value)
		if err != nil {
			return fmt.Errorf("model info update interval must be an integer")
		}
		return nil
	case SettingKeyRelayStreamKeepaliveSec, SettingKeyRelayStreamDataTimeoutSec, SettingKeyResponsesSessionTTL,
		SettingKeySessionKeepTimeDefault, SettingKeyFirstTokenTimeOutDefault, SettingKeyFirstByteKeepaliveDelaySeconds:
		value, err := strconv.Atoi(s.Value)
		if err != nil || value < 0 {
			return fmt.Errorf("%s must be a non-negative integer", s.Key)
		}
		return nil
	case SettingKeyRouteModeOverride:
		switch strings.ToLower(strings.TrimSpace(s.Value)) {
		case "", "spread", "fill_first":
			return nil
		default:
			return fmt.Errorf("%s must be empty, spread, or fill_first", s.Key)
		}
	case SettingKeyRelayLogKeepEnabled, SettingKeyAnthropicAutoCacheControl, SettingKeyOpenAIAutoPromptCacheKey,
		SettingKeyClaudeHeaderStabilize, SettingKeyClaudeCLIAutoCompact, SettingKeyCodexFastMode,
		SettingKeyUserRegistrationEnabled, SettingKeyUpstreamErrorStatusPass, SettingKeyCheckInEnabled,
		SettingKeyDebugLoadBalancer, SettingKeyEmailVerificationEnabled, SettingKeyEmailSMTPSSL,
		SettingKeyUpstreamUTLSFingerprint:
		if s.Value != "true" && s.Value != "false" {
			return fmt.Errorf("%s must be true or false", s.Key)
		}
		return nil
	case SettingKeyEmailSMTPPort:
		value, err := strconv.Atoi(s.Value)
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("%s must be a port between 1 and 65535", s.Key)
		}
		return nil
	case SettingKeyEmailSMTPHost, SettingKeyEmailSMTPUser, SettingKeyEmailSMTPFrom, SettingKeyEmailSMTPFromName,
		SettingKeyEmailHTTPFrom:
		if strings.ContainsAny(s.Value, "\r\n") {
			return fmt.Errorf("%s must not contain newlines", s.Key)
		}
		return nil
	case SettingKeyEmailHTTPAdminAuth, SettingKeyEmailHTTPSiteAuth:
		if strings.ContainsAny(s.Value, "\r\n") {
			return fmt.Errorf("%s must not contain newlines", s.Key)
		}
		return nil
	case SettingKeyEmailProvider:
		switch s.Value {
		case "", "smtp", "http":
			return nil
		default:
			return fmt.Errorf("%s must be smtp or http", s.Key)
		}
	case SettingKeyEmailHTTPBaseURL:
		if s.Value == "" {
			return nil
		}
		parsedURL, err := url.Parse(s.Value)
		if err != nil {
			return fmt.Errorf("%s is invalid: %w", s.Key, err)
		}
		if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
			return fmt.Errorf("%s scheme must be http or https", s.Key)
		}
		if parsedURL.Host == "" {
			return fmt.Errorf("%s must have a host", s.Key)
		}
		return nil
	case SettingKeyClaudeHeaderUserAgent, SettingKeyClaudeHeaderPackage, SettingKeyClaudeHeaderRuntime,
		SettingKeyClaudeHeaderOS, SettingKeyClaudeHeaderArch, SettingKeyClaudeHeaderTimeout,
		SettingKeyCodexHeaderUserAgent, SettingKeyCodexHeaderBetaFeatures, SettingKeyClaudeBetaStripFlags:
		if strings.ContainsAny(s.Value, "\r\n") {
			return fmt.Errorf("%s must not contain newlines", s.Key)
		}
		return nil
	case SettingKeyClaudeCLIReasoningEffort:
		switch strings.ToLower(strings.TrimSpace(s.Value)) {
		case "", "auto", "off", "low", "medium", "high":
			return nil
		default:
			return fmt.Errorf("%s must be auto, off, low, medium, or high", s.Key)
		}
	case SettingKeyUpstreamErrorBodyMode:
		switch s.Value {
		case "", "redacted_upstream", "custom_message", "octopus_standard":
			return nil
		default:
			return fmt.Errorf("invalid upstream error body mode")
		}
	case SettingKeyUpstreamErrorCustom:
		return nil
	case SettingKeyUpstreamErrorPublicCode:
		if !isSafePublicErrorCode(s.Value) {
			return fmt.Errorf("upstream error public code may only contain letters, numbers, dots, underscores, and dashes")
		}
		return nil
	case SettingKeyCheckInRewardMode:
		switch s.Value {
		case "", "fixed", "random":
			return nil
		default:
			return fmt.Errorf("invalid check-in reward mode")
		}
	case SettingKeyCheckInRewardAmount, SettingKeyCheckInRewardMin, SettingKeyCheckInRewardMax:
		value, err := strconv.ParseFloat(s.Value, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s must be a non-negative number", s.Key)
		}
		return nil
	case SettingKeyRelayLogMaxStorageGB:
		value, err := strconv.ParseFloat(s.Value, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("relay log max storage must be a non-negative number")
		}
		return nil
	case SettingKeyPromptOverrideMode:
		switch PromptOverrideMode(s.Value) {
		case "", PromptOverrideModeAppendSystem, PromptOverrideModeReplaceSystem:
			return nil
		default:
			return fmt.Errorf("invalid prompt override mode")
		}
	case SettingKeyProxyURL:
		if s.Value == "" {
			return nil
		}
		parsedURL, err := url.Parse(s.Value)
		if err != nil {
			return fmt.Errorf("proxy URL is invalid: %w", err)
		}
		validSchemes := map[string]bool{
			"http":   true,
			"https":  true,
			"socks":  true,
			"socks5": true,
		}
		if !validSchemes[parsedURL.Scheme] {
			return fmt.Errorf("proxy URL scheme must be http, https, socks, or socks5")
		}
		if parsedURL.Host == "" {
			return fmt.Errorf("proxy URL must have a host")
		}
		return nil
	case SettingKeyStatsTimezone:
		trimmed := strings.TrimSpace(s.Value)
		if trimmed == "" {
			return nil
		}
		if _, err := time.LoadLocation(trimmed); err != nil {
			return fmt.Errorf("%s must be empty or a valid IANA timezone (e.g. America/Los_Angeles): %w", s.Key, err)
		}
		return nil
	}

	return nil
}

func defaultRelayStreamKeepaliveIntervalSeconds() string {
	raw := strings.TrimSpace(os.Getenv("OCTOPUS_RELAY_STREAM_KEEPALIVE_INTERVAL_SECONDS"))
	if raw == "" {
		return "15"
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return "15"
	}
	if value < 0 {
		return "0"
	}
	return strconv.Itoa(value)
}

func defaultFirstByteKeepaliveDelaySeconds() string {
	raw := strings.TrimSpace(os.Getenv("OCTOPUS_RELAY_FIRST_BYTE_KEEPALIVE_DELAY_SECONDS"))
	if raw == "" {
		return "20"
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return "20"
	}
	if value < 0 {
		return "20"
	}
	return strconv.Itoa(value)
}

func defaultRelayStreamDataIntervalTimeoutSeconds() string {
	raw := strings.TrimSpace(os.Getenv("OCTOPUS_RELAY_STREAM_DATA_INTERVAL_TIMEOUT_SECONDS"))
	if raw == "" {
		return DefaultRelayStreamDataIntervalTimeoutSeconds
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultRelayStreamDataIntervalTimeoutSeconds
	}
	if value < 0 {
		return "0"
	}
	return strconv.Itoa(value)
}

func isSafePublicErrorCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if len(value) > 80 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}
