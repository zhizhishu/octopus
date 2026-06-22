package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const (
	defaultClaudeUserAgent      = dbmodel.DefaultClaudeHeaderUserAgent
	defaultClaudePackageVersion = "0.94.0"
	defaultClaudeRuntimeVersion = "v24.3.0"
	defaultClaudeOS             = "Windows"
	defaultClaudeArch           = "x64"
	defaultClaudeTimeout        = "600"
	defaultClaudeOneMillionBeta = model.AnthropicOneMillionBeta
	defaultCodexUserAgent       = dbmodel.DefaultCodexHeaderUserAgent
	defaultCodexBetaFeatures    = "terminal_resize_reflow"
	defaultCodexOriginator      = "codex_exec"
)

func (ra *relayAttempt) applyHeaderDefaults(req *http.Request) {
	if ra == nil || req == nil || ra.channel == nil {
		return
	}
	if !shouldApplyChannelCloak(ra.channel.Cloak) {
		// cloak=never: don't dress as claude. But the downstream client's Anthropic-Beta
		// (copied through by copyHeaders) would otherwise LEAK to the upstream — for a
		// domestic Anthropic-compatible upstream (GLM/DeepSeek) that is a stray claude-code
		// fingerprint we never intended to send. Strip it so the downstream's beta cannot
		// pollute the upstream shape. (When cloak applies, applyClaudeHeaderDefaults rebuilds
		// the canonical beta set instead.)
		if ra.channel.Type == outbound.OutboundTypeAnthropic {
			req.Header.Del("Anthropic-Beta")
		}
		return
	}
	switch ra.channel.Type {
	case outbound.OutboundTypeAnthropic:
		ra.applyClaudeHeaderDefaults(req)
	case outbound.OutboundTypeOpenAIResponse:
		applyCodexHeaderDefaults(req, ra.internalRequest)
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		if ra.inboundType == inbound.InboundTypeOpenAIResponse {
			applyCodexHeaderDefaults(req, ra.internalRequest)
		}
	}
}

func shouldApplyChannelCloak(cloak dbmodel.ChannelCloak) bool {
	switch strings.ToLower(strings.TrimSpace(cloak.Mode)) {
	case "", "auto", "always":
		return true
	case "never", "off", "false", "disabled":
		return false
	default:
		return true
	}
}

// claudeClientVersion holds the version-bearing headers a genuine claude-cli
// DOWNSTREAM client reported. octopus strips the client's User-Agent / X-Stainless-*
// (hop-by-hop) and re-emits its own; pinning a single hard-coded version across ALL
// traffic is itself a weak tell and goes stale as clients upgrade. When the inbound
// IS a genuine claude-cli, we mirror its real version values instead — the canonical
// header SET + ORDER (the part AnyRouter shape-checks) is unchanged, only the version
// strings track the real client. Non-claude clients yield an empty struct, so the
// static settings (prior behaviour) apply byte-for-byte.
type claudeClientVersion struct {
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	OS             string
	Arch           string
}

func (ra *relayAttempt) inboundClaudeClientVersion() claudeClientVersion {
	// UNIFORM-UA requirement: octopus must present the SAME claude-cli UA/version to
	// upstreams for ALL traffic — an upstream must NOT see a different device/version
	// just because a different downstream client relayed through octopus. We therefore
	// deliberately do NOT mirror the downstream claude-cli's version here (this
	// supersedes the earlier per-request version adoption). Returning an empty struct
	// makes every applyClaudeHeaderDefaults call site fall through to the single
	// configured static default, so all upstream traffic shares one fingerprint.
	return claudeClientVersion{}
}

func firstNonEmptyHeader(client, fallback string) string {
	if strings.TrimSpace(client) != "" {
		return client
	}
	return fallback
}

func (ra *relayAttempt) applyClaudeHeaderDefaults(req *http.Request) {
	client := ra.inboundClaudeClientVersion()
	ensureClaudeBetaQuery(req)
	setHeaderIfMissing(req.Header, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	setHeaderIfMissing(req.Header, "Anthropic-Version", "2023-06-01")
	setHeaderIfMissing(req.Header, "User-Agent", firstNonEmptyHeader(client.UserAgent, settingString(dbmodel.SettingKeyClaudeHeaderUserAgent, defaultClaudeUserAgent)))
	setHeaderIfMissing(req.Header, "X-App", "cli")
	// NB: genuine claude-cli (2.1.168 and 2.1.178, captured on the wire) does NOT
	// send X-Client-Request-Id, so we must not synthesize one — an extra header the
	// real CLI never emits is a detectable non-CLI tell to AnyRouter's shape check.
	setHeaderIfMissing(req.Header, "X-Claude-Code-Session-Id", ra.claudeFingerprintSessionID())
	setHeaderIfMissing(req.Header, "X-Stainless-Lang", "js")
	setHeaderIfMissing(req.Header, "X-Stainless-Retry-Count", "0")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime", "node")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime-Version", firstNonEmptyHeader(client.RuntimeVersion, settingString(dbmodel.SettingKeyClaudeHeaderRuntime, defaultClaudeRuntimeVersion)))
	setHeaderIfMissing(req.Header, "X-Stainless-Package-Version", firstNonEmptyHeader(client.PackageVersion, settingString(dbmodel.SettingKeyClaudeHeaderPackage, defaultClaudePackageVersion)))
	setHeaderIfMissing(req.Header, "X-Stainless-Timeout", settingString(dbmodel.SettingKeyClaudeHeaderTimeout, defaultClaudeTimeout))
	if settingBool(dbmodel.SettingKeyClaudeHeaderStabilize, true) {
		setHeaderIfMissing(req.Header, "X-Stainless-OS", firstNonEmptyHeader(client.OS, settingString(dbmodel.SettingKeyClaudeHeaderOS, defaultClaudeOS)))
		setHeaderIfMissing(req.Header, "X-Stainless-Arch", firstNonEmptyHeader(client.Arch, settingString(dbmodel.SettingKeyClaudeHeaderArch, defaultClaudeArch)))
	}
	// REBUILD the anthropic-beta header so its ORDER exactly matches a genuine
	// claude-cli request (context-1m in its real slot, never stuck at position 1 —
	// a non-CLI tell). Built through the shared model.BuildClaudeCodeBetaOrder so the
	// relay forward path and the channel/model test path are byte-for-byte identical.
	var transformBetas []string
	if ra != nil && ra.internalRequest != nil {
		transformBetas = ra.internalRequest.TransformOptions.AnthropicBetas
	}
	betas := model.BuildClaudeCodeBetaOrder(
		shouldEnableClaudeOneMillionBeta(ra.internalRequest),
		strings.Split(req.Header.Get("Anthropic-Beta"), ","),
		transformBetas,
	)
	req.Header.Del("Anthropic-Beta")
	for _, beta := range betas {
		addAnthropicBetaHeader(req.Header, beta)
	}
}

func ensureClaudeBetaQuery(req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	q := req.URL.Query()
	if strings.TrimSpace(q.Get("beta")) == "" {
		q.Set("beta", "true")
		req.URL.RawQuery = q.Encode()
	}
}

func hashClaudeSessionHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("claude-session:" + value))
	return hex.EncodeToString(sum[:16])
}

func shouldEnableClaudeOneMillionBeta(internalRequest *model.InternalLLMRequest) bool {
	return model.AnthropicRequestWantsOneMillionBeta(internalRequest)
}

func addAnthropicBetaHeader(headers http.Header, beta string) {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return
	}
	existing := strings.Split(headers.Get("Anthropic-Beta"), ",")
	seen := map[string]bool{}
	values := make([]string, 0, len(existing)+1)
	for _, item := range existing {
		item = strings.TrimSpace(item)
		if item == "" || seen[strings.ToLower(item)] {
			continue
		}
		seen[strings.ToLower(item)] = true
		values = append(values, item)
	}
	if !seen[strings.ToLower(beta)] {
		values = append(values, beta)
	}
	headers.Set("Anthropic-Beta", strings.Join(values, ","))
}

func applyCodexHeaderDefaults(req *http.Request, internalRequest *model.InternalLLMRequest) {
	setHeaderIfMissing(req.Header, "Connection", "Keep-Alive")
	setHeaderIfMissing(req.Header, "Content-Type", "application/json")
	setHeaderIfMissing(req.Header, "Originator", defaultCodexOriginator)
	setHeaderIfMissing(req.Header, "User-Agent", settingString(dbmodel.SettingKeyCodexHeaderUserAgent, defaultCodexUserAgent))
	setHeaderIfMissing(req.Header, "X-Codex-Beta-Features", settingString(dbmodel.SettingKeyCodexHeaderBetaFeatures, defaultCodexBetaFeatures))
	applyCodexSessionHeaders(req.Header, internalRequest)
}

func setHeaderIfMissing(headers http.Header, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || headers.Get(key) != "" {
		return
	}
	headers.Set(key, value)
}

func settingString(key dbmodel.SettingKey, fallback string) string {
	value, err := op.SettingGetString(key)
	if err != nil {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if key == dbmodel.SettingKeyClaudeHeaderUserAgent && value == "claude-cli/2.1.126 (external, claude-vscode, agent-sdk/0.2.126)" {
		return fallback
	}
	if key == dbmodel.SettingKeyClaudeHeaderPackage && value == "0.81.0" {
		return fallback
	}
	if key == dbmodel.SettingKeyCodexHeaderUserAgent && value == dbmodel.LegacyDefaultCodexHeaderUserAgent0133 {
		return fallback
	}
	return value
}

func settingBool(key dbmodel.SettingKey, fallback bool) bool {
	value, err := op.SettingGetBool(key)
	if err != nil {
		return fallback
	}
	return value
}
