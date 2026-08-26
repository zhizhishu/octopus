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

// ChannelWireHeaderOptions contains the request context needed to apply the
// same upstream header contract from both production relay traffic and
// synthetic channel tests.
type ChannelWireHeaderOptions struct {
	Channel         *dbmodel.Channel
	InboundType     inbound.InboundType
	InternalRequest *model.InternalLLMRequest
	ClaudeSessionID string
}

// ApplyChannelWireHeaders applies protocol defaults, operator custom headers,
// and the invariants that must win after custom overrides. Keeping this order in
// one function prevents model tests from drifting away from production traffic.
func ApplyChannelWireHeaders(req *http.Request, options ChannelWireHeaderOptions) {
	if req == nil || options.Channel == nil {
		return
	}

	applyChannelWireHeaderDefaults(req, options)
	for _, customHeader := range options.Channel.CustomHeader {
		headerName := strings.TrimSpace(customHeader.HeaderKey)
		if headerName == "" {
			continue
		}
		req.Header.Set(headerName, customHeader.HeaderValue)
	}

	if !shouldApplyChannelCloak(options.Channel.Cloak) {
		switch options.Channel.Type {
		case outbound.OutboundTypeAnthropic:
			req.Header.Del("Anthropic-Beta")
		case outbound.OutboundTypeOpenAIResponse:
			stripCodexClientHeaders(req.Header)
		}
		setHeaderForce(req.Header, "User-Agent", genericUAForChannel(options.Channel))
	}

	// Originator and User-Agent are one Codex identity pair. An operator header
	// may customize either value, but must not split the pair seen upstream.
	if shouldUseCodexHeaderDefaults(options.Channel, options.InboundType) {
		fingerprint := resolveFingerprintForChannel(options.Channel)
		setHeaderForce(req.Header, "Originator", fingerprint.CodexOriginator())
		setHeaderForce(req.Header, "User-Agent", fingerprint.CodexUserAgent())
		applyCodexResponsesLiteHeader(req.Header, options.InternalRequest)
	}
	// Genuine Codex CLI sends no Accept-Encoding. Re-assert this after custom
	// headers because a channel preset can otherwise add it back.
	enforceCodexNoAcceptEncoding(options.Channel.Type, req.Header)
}

func (ra *relayAttempt) applyHeaderDefaults(req *http.Request) {
	if ra == nil || req == nil || ra.channel == nil {
		return
	}
	ApplyChannelWireHeaders(req, ChannelWireHeaderOptions{
		Channel:         ra.channel,
		InboundType:     ra.inboundType,
		InternalRequest: ra.internalRequest,
		ClaudeSessionID: ra.claudeFingerprintSessionID(),
	})
}

func applyChannelWireHeaderDefaults(req *http.Request, options ChannelWireHeaderOptions) {
	channel := options.Channel
	// Only on the codex (Responses inbound) -> claude (Anthropic outbound) path, strip the
	// codex / OpenAI-Responses-specific client headers a codex downstream forwarded through
	// copyHeaders. A genuine claude-cli never emits these, so carrying them upstream dresses
	// one request as BOTH claude-cli and codex — a contradictory fingerprint. Gated on the
	// codex inbound so a non-codex client's own header (notably the generic "Originator") is
	// never removed from a chat->claude / claude->claude request.
	if channel.Type == outbound.OutboundTypeAnthropic && options.InboundType == inbound.InboundTypeOpenAIResponse {
		stripCodexClientHeaders(req.Header)
	}
	if !shouldApplyChannelCloak(channel.Cloak) {
		// Shape OFF (cloak=never): do NOT synthesize the claude/codex CLI identity. But
		// "no CLI" must resolve to a CLEAN GENERIC identity, never a bare/leaky one:
		//   1. Strip the CLI-specific headers a downstream CLI client leaked through
		//      copyHeaders — Anthropic-Beta on the claude path, Originator/X-Codex-* on the
		//      codex path. On a plain Anthropic/OpenAI-compatible upstream (GLM/DeepSeek)
		//      those are a stray CLI fingerprint the operator explicitly opted out of.
		//   2. Apply the unified generic UA on EVERY type — including the two CLI-capable
		//      ones. Leaving them bare emitted Go's default "Go-http-client/1.1", which flags
		//      the caller as a bot/script; the operator picked "no shape", not "no identity".
		//      The generic UA follows the selected profile (Debian Chrome / Ubuntu Firefox),
		//      falling back to DefaultGenericUA.
		switch channel.Type {
		case outbound.OutboundTypeAnthropic:
			req.Header.Del("Anthropic-Beta")
		case outbound.OutboundTypeOpenAIResponse:
			stripCodexClientHeaders(req.Header)
		}
		applyGenericHeaderDefaultsForChannel(req, channel)
		return
	}
	fingerprint := resolveFingerprintForChannel(channel)
	switch channel.Type {
	case outbound.OutboundTypeAnthropic:
		applyClaudeHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint, options.ClaudeSessionID)
	case outbound.OutboundTypeOpenAIResponse:
		applyCodexHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint)
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		if options.InboundType == inbound.InboundTypeOpenAIResponse {
			applyCodexHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint)
		} else {
			applyGenericHeaderDefaultsForChannel(req, channel)
		}
	default:
		// Non claude/codex channels (Gemini/Volcengine/plain OpenAI-chat) get no
		// fingerprint UA today. A selected profile may pin a unified GenericUA so
		// these channels present a consistent client identity too; with no profile
		// (or an empty GenericUA) this is a no-op, preserving current behaviour.
		applyGenericHeaderDefaultsForChannel(req, channel)
	}
}

func shouldUseCodexHeaderDefaults(channel *dbmodel.Channel, inboundType inbound.InboundType) bool {
	if channel == nil || !shouldApplyChannelCloak(channel.Cloak) {
		return false
	}
	if channel.Type == outbound.OutboundTypeOpenAIResponse {
		return true
	}
	return inboundType == inbound.InboundTypeOpenAIResponse &&
		(channel.Type == outbound.OutboundTypeOpenAIChat || channel.Type == outbound.OutboundTypeCustomOpenAIChat)
}

// applyGenericHeaderDefaults sets a unified User-Agent on channels that have no
// claude/codex fingerprint (Gemini / Volcengine / plain OpenAI-chat). The selected
// profile's GenericUA wins; when it is empty we fall back to model.DefaultGenericUA
// so these channels present a stable Linux desktop identity instead of leaking Go's
// "Go-http-client/1.1". Never touches claude/codex channels (different switch arms).
func (ra *relayAttempt) applyGenericHeaderDefaults(req *http.Request) {
	applyGenericHeaderDefaultsForChannel(req, ra.channel)
}

func applyGenericHeaderDefaultsForChannel(req *http.Request, channel *dbmodel.Channel) {
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))
}

// genericUAForChannel resolves the unified non-CLI User-Agent for a channel: the
// selected fingerprint profile's GenericUA (that is the "two UA profiles to pick
// from" — a channel picks a profile, the profile pins its UA), falling back to the
// built-in DefaultGenericUA when the profile leaves it empty or no profile is set.
// Channel-based (not relayAttempt-based) on purpose so the raw-protocol and image
// bridge paths — which have no relayAttempt — resolve the SAME unified UA as the
// main relay path, instead of leaking Go's default http client UA.
func genericUAForChannel(channel *dbmodel.Channel) string {
	if ua := resolveFingerprintForChannel(channel).GenericUA(); ua != "" {
		return ua
	}
	return dbmodel.DefaultGenericUA
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

// shouldApplyChannelCloak reports whether a channel's cloak (identity/fingerprint
// synthesis) is applied.
//
// NOTE on "auto": by design "auto" is treated identically to "always" — the cloak
// is ALWAYS applied, there is no client-aware detection (real CLI passthrough vs
// non-CLI synthesis). This is intentional under the UNIFORM-UA policy: the upstream
// must only ever see one uniform device/identity, never a per-client one. Some
// reference relays (e.g. CLIProxyAPI) make "auto" client-aware; octopus deliberately
// does not, to avoid leaking a per-client UA/identity upstream. Only "never" (and its
// aliases) disables the cloak.
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

// ShouldApplyChannelCloak exposes the relay's single cloak-mode policy to thin
// adapters such as modeltest without requiring them to duplicate its aliases.
func ShouldApplyChannelCloak(cloak dbmodel.ChannelCloak) bool {
	return shouldApplyChannelCloak(cloak)
}

func (ra *relayAttempt) applyClaudeHeaderDefaults(req *http.Request) {
	// fp resolves the channel's selected fingerprint profile; with no profile every
	// getter returns exactly the global setting value, so this is byte-for-byte the
	// prior behaviour. A selected profile overrides only the fields it sets.
	fp := ra.fingerprint()
	applyClaudeHeaderDefaultsWithFingerprint(req, ra.internalRequest, fp, ra.claudeFingerprintSessionID())
}

func applyClaudeHeaderDefaultsWithFingerprint(req *http.Request, internalRequest *model.InternalLLMRequest, fp resolvedFingerprint, sessionID string) {
	ensureClaudeBetaQuery(req)
	setHeaderIfMissing(req.Header, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	setHeaderIfMissing(req.Header, "Anthropic-Version", "2023-06-01")
	setHeaderIfMissing(req.Header, "User-Agent", fp.ClaudeUserAgent())
	setHeaderIfMissing(req.Header, "X-App", "cli")
	// NB: genuine claude-cli (2.1.168 and 2.1.178, captured on the wire) does NOT
	// send X-Client-Request-Id, so we must not synthesize one — an extra header the
	// real CLI never emits is a detectable non-CLI tell to the relay's shape check.
	setHeaderIfMissing(req.Header, "X-Claude-Code-Session-Id", sessionID)
	setHeaderIfMissing(req.Header, "X-Stainless-Lang", "js")
	setHeaderIfMissing(req.Header, "X-Stainless-Retry-Count", "0")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime", "node")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime-Version", fp.ClaudeRuntimeVersion())
	setHeaderIfMissing(req.Header, "X-Stainless-Package-Version", fp.ClaudePackageVersion())
	setHeaderIfMissing(req.Header, "X-Stainless-Timeout", fp.ClaudeTimeout())
	// Always emit X-Stainless-OS / X-Stainless-Arch. A genuine claude-cli sends both on
	// every request, and the downstream client's own x-stainless-* were already stripped
	// as hop-by-hop (shouldForwardClientHeader), so OMITTING them here is itself a
	// non-CLI tell (a request missing two headers the real CLI always carries). The value
	// reuses the SAME stable default pair the pinned fingerprint already presents
	// (fp.ClaudeOS()/ClaudeArch() — Windows/x64 by default, profile/setting overridable),
	// so this introduces no new fingerprint value.
	// NB: claudeStabilize() (SettingKeyClaudeHeaderStabilize / profile.ClaudeStabilize) is
	// now INERT for this pair — it no longer suppresses these two headers. The setting,
	// the profile tri-state field, and the admin toggle are retained only for backward
	// compatibility. The modeltest channel/test path (modeltest/runner.go) mirrors this
	// unconditional emit so a channel test stays byte-for-byte identical to real traffic.
	setHeaderIfMissing(req.Header, "X-Stainless-OS", fp.ClaudeOS())
	setHeaderIfMissing(req.Header, "X-Stainless-Arch", fp.ClaudeArch())
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
	if internalRequest != nil {
		transformBetas = internalRequest.TransformOptions.AnthropicBetas
		betaModel = internalRequest.Model
	}
	betas := model.BuildClaudeCodeBetaHeader(
		betaModel,
		shouldEnableClaudeOneMillionBeta(internalRequest),
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
	// FORCE the codex identity (Originator + UA) to a self-consistent codex_cli_rs pair,
	// OVERRIDING whatever a downstream client leaked through. sub2api/new-api-style upstreams
	// require the originator to pair with the User-Agent's leading token (both codex_cli_rs)
	// and only accept the codex_cli_rs client — a mismatch (e.g. a leaked codex_exec originator
	// against oct's codex_cli_rs UA) is rejected (sub2api issue #3901: originator↔UA-first-token
	// must pair + version≥0.144.0). setHeaderIfMissing let a copied-through client Originator
	// survive while the UA fell back to oct's default, producing exactly that mismatch on the
	// wire. Both come from the resolved fingerprint so a selected profile still overrides them.
	setHeaderForce(req.Header, "Originator", fp.CodexOriginator())
	setHeaderForce(req.Header, "User-Agent", fp.CodexUserAgent())
	setHeaderIfMissing(req.Header, "X-Codex-Beta-Features", fp.CodexBetaFeatures())
	applyCodexResponsesLiteHeader(req.Header, internalRequest)
	applyCodexSessionHeaders(req.Header, internalRequest, fp.CodexInstallationID())
}

// Models that reject or must avoid the Lite route upstream. The gpt-5.5 base
// slug is confirmed by a live 400 ("This model is not supported when using
// X-OpenAI-Internal-Codex-Responses-Lite"); the pro variants are family
// inference, not yet live-probed. Note sub2api's own no-Lite list targets
// gpt-5.6-* for custom-key providers (openai_codex_models_service.go) — a
// different concern, but a hint that Lite eligibility is per-model and shifts;
// revisit if a 5.6 model ever 400s on Lite. Mapping-renamed models never reach
// this check, so only unmapped client names land here.
var codexResponsesLiteDeniedModels = map[string]struct{}{
	"gpt-5.5":     {},
	"gpt-5.5-pro": {},
	"gpt-5-5-pro": {},
}

func applyCodexResponsesLiteHeader(headers http.Header, internalRequest *model.InternalLLMRequest) {
	// The gpt-5.5 family denylist rejects the Lite route before inference. Delete
	// rather than merely omit so a downstream client or operator custom header
	// cannot add the incompatible capability back after Octopus has selected the
	// actual upstream model.
	if internalRequest != nil && !codexResponsesLiteApplies(internalRequest.Model) {
		headers.Del("X-Openai-Internal-Codex-Responses-Lite")
		return
	}

	// Codex 0.144.x emits this header for Lite-capable models. Unknown models retain the
	// established wire contract; only an upstream-confirmed incompatibility is denied.
	setHeaderIfMissing(headers, "X-Openai-Internal-Codex-Responses-Lite", "true")
}

// codexResponsesLiteApplies reports whether the Lite capability header will be on the
// wire for this model. The codex shaper gates the body-side pairing fields
// (reasoning.context=all_turns, parallel_tool_calls=false) on the SAME decision, so a
// denylisted model never ships headless pairing constraints the header no longer justifies.
// One seam here; no duplicated list in the shaper.
func codexResponsesLiteApplies(modelName string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	_, denied := codexResponsesLiteDeniedModels[normalized]
	return !denied
}

func setHeaderIfMissing(headers http.Header, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || headers.Get(key) != "" {
		return
	}
	headers.Set(key, value)
}

// setHeaderForce sets the header from a non-empty value, OVERWRITING any existing (e.g. a
// downstream client's leaked-through value). Used for codex Originator/User-Agent where a
// self-consistent codex_cli_rs pair must reach the upstream regardless of what the caller sent.
func setHeaderForce(headers http.Header, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		headers.Set(key, value)
	}
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
