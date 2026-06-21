package model

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type SettingKey string

const (
	SettingKeyProxyURL                  SettingKey = "proxy_url"
	SettingKeyStatsSaveInterval         SettingKey = "stats_save_interval"          // 将统计信息写入数据库的周期(分钟)
	SettingKeyModelInfoUpdateInterval   SettingKey = "model_info_update_interval"   // 模型信息更新间隔(小时)
	SettingKeySyncLLMInterval           SettingKey = "sync_llm_interval"            // LLM 同步间隔(小时)
	SettingKeyRelayLogKeepPeriod        SettingKey = "relay_log_keep_period"        // 日志保存时间范围(天)
	SettingKeyRelayLogKeepEnabled       SettingKey = "relay_log_keep_enabled"       // 是否保留历史日志
	SettingKeyRelayLogMaxStorageGB      SettingKey = "relay_log_max_storage_gb"     // 日志最大保存容量(GB), 0 表示不限制
	SettingKeyAnthropicAutoCacheControl SettingKey = "anthropic_auto_cache_control" // Anthropic 出站自动为稳定长前缀注入 cache_control
	SettingKeyRelayStreamKeepaliveSec   SettingKey = "relay_stream_keepalive_interval_seconds"
	SettingKeyOpenAIAutoPromptCacheKey  SettingKey = "openai_auto_prompt_cache_key"
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
	SettingKeyCodexHeaderUserAgent      SettingKey = "codex_header_defaults_user_agent"
	SettingKeyCodexHeaderBetaFeatures   SettingKey = "codex_header_defaults_beta_features"
	SettingKeyCodexFastMode             SettingKey = "codex_fast_mode"
	SettingKeyUserRegistrationEnabled   SettingKey = "user_registration_enabled"    // 是否允许无邀请码直接注册
	SettingKeyCORSAllowOrigins          SettingKey = "cors_allow_origins"           // 跨域白名单(逗号分隔, 如 "example.com,example2.com"). 为空不允许跨域, "*"允许所有
	SettingKeyCircuitBreakerThreshold   SettingKey = "circuit_breaker_threshold"    // 熔断触发阈值（连续失败次数）
	SettingKeyCircuitBreakerCooldown    SettingKey = "circuit_breaker_cooldown"     // 熔断基础冷却时间（秒）
	SettingKeyCircuitBreakerMaxCooldown SettingKey = "circuit_breaker_max_cooldown" // 熔断最大冷却时间（秒），指数退避上限
	SettingKeyDebugLoadBalancer         SettingKey = "debug_load_balancer"          // 开启后每次选路打印候选排序决策(tier/rank/容量输入)，便于排障"为啥走这条/为啥慢"
	SettingKeyPromptOverrideSystem      SettingKey = "prompt_override_system"
	SettingKeyPromptOverrideMode        SettingKey = "prompt_override_mode"
	SettingKeyUpstreamErrorStatusPass   SettingKey = "upstream_error_status_passthrough"
	SettingKeyUpstreamErrorBodyMode     SettingKey = "upstream_error_body_mode"
	SettingKeyUpstreamErrorCustom       SettingKey = "upstream_error_custom_message"
	SettingKeyUpstreamErrorPublicCode   SettingKey = "upstream_error_public_code"
	SettingKeyCheckInEnabled            SettingKey = "checkin_enabled"
	SettingKeyCheckInRewardMode         SettingKey = "checkin_reward_mode"
	SettingKeyCheckInRewardAmount       SettingKey = "checkin_reward_amount"
	SettingKeyCheckInRewardMin          SettingKey = "checkin_reward_min"
	SettingKeyCheckInRewardMax          SettingKey = "checkin_reward_max"
)

type Setting struct {
	Key   SettingKey `json:"key" gorm:"primaryKey"`
	Value string     `json:"value" gorm:"not null"`
}

const (
	DefaultCodexHeaderUserAgent = "codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)"

	// DefaultClaudeHeaderUserAgent is the locally packet-verified Claude Code CLI
	// user-agent (claude-cli/2.1.178). Keep it in lockstep with the billing-header
	// cc_version in the Anthropic outbound transformer so the UA and the body billing
	// block never disagree on the client version.
	DefaultClaudeHeaderUserAgent = "claude-cli/2.1.178 (external, sdk-cli)"

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

	// LegacyDefaultCodexHeaderUserAgentCliRs0114 was an early Codex header default
	// (codex_cli_rs/0.114.0 on macOS) shipped before the locally packet-verified
	// codex_exec Windows fingerprint. Deployments seeded with it stayed pinned to the
	// stale macOS UA across image updates because it was never on the upgrade list;
	// treat it as a product default so it converges to DefaultCodexHeaderUserAgent.
	LegacyDefaultCodexHeaderUserAgentCliRs0114 = "codex_cli_rs/0.114.0 (Mac OS 14.2.0; x86_64) vscode/1.111.0"

	// DefaultCodexHeaderBetaFeatures is the current Codex beta-feature header value.
	DefaultCodexHeaderBetaFeatures = "terminal_resize_reflow"

	// LegacyDefaultCodexHeaderBetaFeaturesMultiAgent was the Codex beta-feature
	// default paired with the early macOS UA above; migrate it to the current value.
	LegacyDefaultCodexHeaderBetaFeaturesMultiAgent = "multi_agent"
)

func DefaultSettings() []Setting {
	return []Setting{
		{Key: SettingKeyProxyURL, Value: ""},
		{Key: SettingKeyStatsSaveInterval, Value: "10"},           // 默认10分钟保存一次统计信息
		{Key: SettingKeyCORSAllowOrigins, Value: ""},              // CORS 默认不允许跨域，设置为 "*" 才允许所有来源
		{Key: SettingKeyModelInfoUpdateInterval, Value: "24"},     // 默认24小时更新一次模型信息
		{Key: SettingKeySyncLLMInterval, Value: "24"},             // 默认24小时同步一次LLM
		{Key: SettingKeyRelayLogKeepPeriod, Value: "7"},           // 默认日志保存7天
		{Key: SettingKeyRelayLogKeepEnabled, Value: "true"},       // 默认保留历史日志
		{Key: SettingKeyRelayLogMaxStorageGB, Value: "0"},         // 默认不按容量裁剪，避免升级后意外删除日志
		{Key: SettingKeyAnthropicAutoCacheControl, Value: "true"}, // 默认开启稳定前缀缓存断点，提升 provider 原生命中率
		{Key: SettingKeyRelayStreamKeepaliveSec, Value: defaultRelayStreamKeepaliveIntervalSeconds()},
		{Key: SettingKeyOpenAIAutoPromptCacheKey, Value: "true"},
		{Key: SettingKeyRelayStreamDataTimeoutSec, Value: defaultRelayStreamDataIntervalTimeoutSeconds()},
		{Key: SettingKeyResponsesSessionTTL, Value: "3600"},
		{Key: SettingKeyClaudeHeaderUserAgent, Value: DefaultClaudeHeaderUserAgent},
		{Key: SettingKeyClaudeHeaderPackage, Value: "0.94.0"},
		{Key: SettingKeyClaudeHeaderRuntime, Value: "v24.3.0"},
		{Key: SettingKeyClaudeHeaderOS, Value: "Windows"},
		{Key: SettingKeyClaudeHeaderArch, Value: "x64"},
		{Key: SettingKeyClaudeHeaderTimeout, Value: "600"},
		{Key: SettingKeyClaudeHeaderStabilize, Value: "true"},
		{Key: SettingKeyClaudeCLIAutoCompact, Value: "false"},
		{Key: SettingKeyClaudeCLIReasoningEffort, Value: "auto"},
		{Key: SettingKeyCodexHeaderUserAgent, Value: DefaultCodexHeaderUserAgent},
		{Key: SettingKeyCodexHeaderBetaFeatures, Value: DefaultCodexHeaderBetaFeatures},
		{Key: SettingKeyCodexFastMode, Value: "false"},
		{Key: SettingKeyUserRegistrationEnabled, Value: "false"}, // 默认只允许邀请码注册
		{Key: SettingKeyCircuitBreakerThreshold, Value: "10"},    // 默认连续失败10次触发熔断
		{Key: SettingKeyCircuitBreakerCooldown, Value: "30"},     // 默认基础冷却30秒
		{Key: SettingKeyCircuitBreakerMaxCooldown, Value: "600"}, // 默认最大冷却600秒（10分钟）
		{Key: SettingKeyDebugLoadBalancer, Value: "false"},       // 默认关闭选路决策日志
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
	}
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
	case SettingKeyRelayStreamKeepaliveSec, SettingKeyRelayStreamDataTimeoutSec, SettingKeyResponsesSessionTTL:
		value, err := strconv.Atoi(s.Value)
		if err != nil || value < 0 {
			return fmt.Errorf("%s must be a non-negative integer", s.Key)
		}
		return nil
	case SettingKeyRelayLogKeepEnabled, SettingKeyAnthropicAutoCacheControl, SettingKeyOpenAIAutoPromptCacheKey,
		SettingKeyClaudeHeaderStabilize, SettingKeyClaudeCLIAutoCompact, SettingKeyCodexFastMode,
		SettingKeyUserRegistrationEnabled, SettingKeyUpstreamErrorStatusPass, SettingKeyCheckInEnabled,
		SettingKeyDebugLoadBalancer:
		if s.Value != "true" && s.Value != "false" {
			return fmt.Errorf("%s must be true or false", s.Key)
		}
		return nil
	case SettingKeyClaudeHeaderUserAgent, SettingKeyClaudeHeaderPackage, SettingKeyClaudeHeaderRuntime,
		SettingKeyClaudeHeaderOS, SettingKeyClaudeHeaderArch, SettingKeyClaudeHeaderTimeout,
		SettingKeyCodexHeaderUserAgent, SettingKeyCodexHeaderBetaFeatures:
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
