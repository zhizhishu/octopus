package relay

import (
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
	defaultClaudePackageVersion = dbmodel.DefaultClaudeHeaderPackageVersion
	defaultClaudeRuntimeVersion = dbmodel.DefaultClaudeHeaderRuntimeVersion
	defaultClaudeOS             = dbmodel.DefaultClaudeHeaderOS
	defaultClaudeArch           = "x64"
	defaultClaudeTimeout        = "600"
	defaultClaudeOneMillionBeta = model.AnthropicOneMillionBeta
	defaultCodexUserAgent       = dbmodel.DefaultCodexHeaderUserAgent
	defaultCodexBetaFeatures    = dbmodel.DefaultCodexHeaderBetaFeatures
	defaultCodexOriginator      = "codex_cli_rs"
)

func (ra *relayAttempt) applyHeaderDefaults(req *http.Request) {
	if ra == nil || req == nil || ra.channel == nil {
		return
	}
	// Only on the codex (Responses inbound) -> claude (Anthropic outbound) path, strip the
	// codex / OpenAI-Responses-specific client headers a codex downstream forwarded through
	// copyHeaders. A genuine claude-cli never emits these, so carrying them upstream dresses
	// one request as BOTH claude-cli and codex — a contradictory fingerprint. Gated on the
	// codex inbound so a non-codex client's own header (notably the generic "Originator") is
	// never removed from a chat->claude / claude->claude request.
	if ra.channel.Type == outbound.OutboundTypeAnthropic && ra.inboundType == inbound.InboundTypeOpenAIResponse {
		stripCodexClientHeaders(req.Header)
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
		ra.applyCodexHeaderDefaults(req, ra.internalRequest)
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		if ra.inboundType == inbound.InboundTypeOpenAIResponse {
			ra.applyCodexHeaderDefaults(req, ra.internalRequest)
		} else {
			ra.applyGenericHeaderDefaults(req)
		}
	default:
		// Non claude/codex channels (Gemini/Volcengine/plain OpenAI-chat) get no
		// fingerprint UA today. A selected profile may pin a unified GenericUA so
		// these channels present a consistent client identity too; with no profile
		// (or an empty GenericUA) this is a no-op, preserving current behaviour.
		ra.applyGenericHeaderDefaults(req)
	}
}

// applyGenericHeaderDefaults sets a unified User-Agent on channels that have no
// claude/codex fingerprint (Gemini / Volcengine / plain OpenAI-chat). The selected
// profile's GenericUA wins; when it is empty we fall back to model.DefaultGenericUA
// so these channels present a stable Linux desktop identity instead of leaking Go's
// "Go-http-client/1.1". Never touches claude/codex channels (different switch arms).
func (ra *relayAttempt) applyGenericHeaderDefaults(req *http.Request) {
	ua := ra.fingerprint().genericUA()
	if ua == "" {
		ua = dbmodel.DefaultGenericUA
	}
	setHeaderIfMissing(req.Header, "User-Agent", ua)
}

// stripCodexClientHeaders removes the codex / OpenAI-Responses-specific request headers a
// codex CLI downstream sends. They are not hop-by-hop, so copyHeaders forwards them; on an
// Anthropic outbound they must be deleted because a genuine claude-cli never emits them.
// Additive to shouldForwardClientHeader, which already drops the session/trace variants.
func stripCodexClientHeaders(header http.Header) {
	header.Del("Originator")
	header.Del("X-Codex-Beta-Features")
	header.Del("X-Codex-Turn-Metadata")
	header.Del("X-Codex-Window-Id")
	header.Del("X-Openai-Internal-Codex-Responses-Lite")
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
// header SET + ORDER (the part the relay shape-checks) is unchanged, only the version
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
	// fp resolves the channel's selected fingerprint profile; with no profile every
	// getter returns exactly the global setting value, so this is byte-for-byte the
	// prior behaviour. A selected profile overrides only the fields it sets.
	fp := ra.fingerprint()
	ensureClaudeBetaQuery(req)
	setHeaderIfMissing(req.Header, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	setHeaderIfMissing(req.Header, "Anthropic-Version", "2023-06-01")
	setHeaderIfMissing(req.Header, "User-Agent", firstNonEmptyHeader(client.UserAgent, fp.claudeUserAgent()))
	setHeaderIfMissing(req.Header, "X-App", "cli")
	// NB: genuine claude-cli (2.1.168 and 2.1.178, captured on the wire) does NOT
	// send X-Client-Request-Id, so we must not synthesize one — an extra header the
	// real CLI never emits is a detectable non-CLI tell to the relay's shape check.
	setHeaderIfMissing(req.Header, "X-Claude-Code-Session-Id", ra.claudeFingerprintSessionID())
	setHeaderIfMissing(req.Header, "X-Stainless-Lang", "js")
	setHeaderIfMissing(req.Header, "X-Stainless-Retry-Count", "0")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime", "node")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime-Version", firstNonEmptyHeader(client.RuntimeVersion, fp.claudeRuntimeVersion()))
	setHeaderIfMissing(req.Header, "X-Stainless-Package-Version", firstNonEmptyHeader(client.PackageVersion, fp.claudePackageVersion()))
	setHeaderIfMissing(req.Header, "X-Stainless-Timeout", fp.claudeTimeout())
	// Always emit X-Stainless-OS / X-Stainless-Arch. A genuine claude-cli sends both on
	// every request, and the downstream client's own x-stainless-* were already stripped
	// as hop-by-hop (shouldForwardClientHeader), so OMITTING them here is itself a
	// non-CLI tell (a request missing two headers the real CLI always carries). The value
	// reuses the SAME stable default pair the pinned fingerprint already presents
	// (fp.claudeOS()/claudeArch() — Windows/x64 by default, profile/setting overridable),
	// so this introduces no new fingerprint value.
	// NB: claudeStabilize() (SettingKeyClaudeHeaderStabilize / profile.ClaudeStabilize) is
	// now INERT for this pair — it no longer suppresses these two headers. The setting,
	// the profile tri-state field, and the admin toggle are retained only for backward
	// compatibility. The modeltest channel/test path (modeltest/runner.go) mirrors this
	// unconditional emit so a channel test stays byte-for-byte identical to real traffic.
	setHeaderIfMissing(req.Header, "X-Stainless-OS", firstNonEmptyHeader(client.OS, fp.claudeOS()))
	setHeaderIfMissing(req.Header, "X-Stainless-Arch", firstNonEmptyHeader(client.Arch, fp.claudeArch()))
	// Decide the anthropic-beta header. A genuine claude-cli downstream already sent its
	// OWN anthropic-beta on the wire — copyHeaders forwards it onto this outbound request
	// (Anthropic-Beta is not hop-by-hop), so req.Header carries the client's real value
	// here. That set+order is the exact per-model, per-request-type shape the relay
	// shape-checks, and a 2026-07-02 wire A/B proved a fixed flagship 7-set on a haiku
	// request is rejected. So PRESERVE a real client's beta verbatim; only synthesize the
	// canonical claude-code order when no client beta is present (a non-claude downstream
	// under cloak, or the synthetic channel/model test). BuildClaudeCodeBetaHeader makes
	// that decision, and the channel/model test path calls the same helper so the two stay
	// byte-for-byte identical.
	var transformBetas []string
	betaModel := ""
	if ra != nil && ra.internalRequest != nil {
		transformBetas = ra.internalRequest.TransformOptions.AnthropicBetas
		betaModel = ra.internalRequest.Model
	}
	betas := model.BuildClaudeCodeBetaHeader(
		betaModel,
		shouldEnableClaudeOneMillionBeta(ra.internalRequest),
		strings.Split(req.Header.Get("Anthropic-Beta"), ","),
		transformBetas,
	)
	// Opt-in escape hatch: drop any beta flags an admin listed in
	// SettingKeyClaudeBetaStripFlags (default empty = no-op = faithful passthrough).
	// Lets a flag that trips an upstream — e.g. the relay's intermittent 520 on
	// prompt-caching-scope-2026-01-05 — be removed without touching the rest of the shape.
	betas = model.StripClaudeBetaFlags(betas, settingString(dbmodel.SettingKeyClaudeBetaStripFlags, ""))
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

func (ra *relayAttempt) applyCodexHeaderDefaults(req *http.Request, internalRequest *model.InternalLLMRequest) {
	applyCodexHeaderDefaultsWithFingerprint(req, internalRequest, ra.fingerprint())
}

// applyCodexHeaderDefaultsWithFingerprint is the shared codex header writer used by
// both the relayAttempt method and the free raw-protocol path. fp resolves the
// channel's selected fingerprint profile; with no profile every getter returns the
// global setting value, so this is byte-for-byte the prior behaviour. A selected
// profile overrides only the codex fields it sets.
func applyCodexHeaderDefaultsWithFingerprint(req *http.Request, internalRequest *model.InternalLLMRequest, fp resolvedFingerprint) {
	setHeaderIfMissing(req.Header, "Connection", "Keep-Alive")
	setHeaderIfMissing(req.Header, "Content-Type", "application/json")
	setHeaderIfMissing(req.Header, "Originator", fp.codexOriginator())
	setHeaderIfMissing(req.Header, "User-Agent", fp.codexUserAgent())
	setHeaderIfMissing(req.Header, "X-Codex-Beta-Features", fp.codexBetaFeatures())
	// codex 0.144.x always emits this static header on /responses (packet-verified 2026-07-10);
	// wire position (4th, after x-codex-turn-metadata) is driven by codexCanonicalHeaderOrder.
	setHeaderIfMissing(req.Header, "X-Openai-Internal-Codex-Responses-Lite", "true")
	applyCodexSessionHeaders(req.Header, internalRequest, fp.codexInstallationID())
}

func setHeaderIfMissing(headers http.Header, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || headers.Get(key) != "" {
		return
	}
	headers.Set(key, value)
}

// settingString reads a header default from the settings cache, falling back to the
// static default when unset/empty. Legacy default values (the old claude UA 2.1.126 /
// package 0.81.0 / codex UA 0.133.0, etc.) are converged to the current default IN THE
// DB at startup by op.settingLegacyDefaultUpgrades, so the cache never holds them by the
// time this reads — no read-time legacy patching is needed. Keeping the convergence in
// one place (the DB) makes the admin settings display equal to what is actually sent.
func settingString(key dbmodel.SettingKey, fallback string) string {
	value, err := op.SettingGetString(key)
	if err != nil {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
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
