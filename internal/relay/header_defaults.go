package relay

import (
	"net/http"
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
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
	// defaultCodexOriginator is the last-resort Originator. It is now consistent with the
	// default UA family (codex_cli_rs): an upstream pairs the two, so a fallback in another
	// family would defeat the pairing the rest of this file enforces.
	defaultCodexOriginator = "codex_cli_rs"
)

// ChannelWireHeaderOptions contains the request context needed to apply the
// same upstream header contract from both production relay traffic and
// synthetic channel tests.
type ChannelWireHeaderOptions struct {
	Channel         *dbmodel.Channel
	InboundType     inbound.InboundType
	InternalRequest *model.InternalLLMRequest
	ClaudeSessionID string
	// ClientUsedBearerAuth reports whether the downstream client is a claude-CLI-shaped
	// client (Anthropic-Beta carries the claude-code flag) that authenticated with an
	// Authorization: Bearer header — the CLI auth-token style. Set only by the relay
	// path (which sees the client request); synthetic callers (modeltest) and plain
	// non-CLI clients leave it false and keep the historical both-headers auth.
	ClientUsedBearerAuth bool
	// ClientUsedAPIKeyAuth is the mirror signal: the downstream client authenticated with
	// an x-api-key and sent NO Authorization — the CLI's api-key style (a plain Anthropic
	// SDK client looks the same on the wire). A relay-shaped Anthropic upstream currently
	// receives BOTH credential headers because applyAnthropicAuthHeaders adds
	// `Authorization: Bearer` for any non-official base, which is a shape deviation: the
	// client that arrived with one credential header must leave with one. Gated the same
	// way as the Bearer mirror (relay path only), and the delete only happens while the
	// outbound still carries X-API-Key, so a request can never leave credential-less.
	ClientUsedAPIKeyAuth bool
	// InboundUserAgent / InboundOriginator carry the DOWNSTREAM client's own codex
	// Originator+User-Agent pair when it declares one. The relay path fills them from the
	// client request (copyHeaders filters the client's User-Agent as hop-by-hop, so it never
	// reaches the outbound request and must be handed over explicitly); synthetic callers
	// (modeltest) and non-CLI clients leave them empty and keep the profile pair. When the UA
	// is a genuine codex_* family the pair is echoed verbatim, so the upstream sees the same
	// bytes a direct connection would have carried (see codexIdentityPair).
	InboundUserAgent  string
	InboundOriginator string
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
			applyAnthropicCloakOffBeta(req.Header, options.InternalRequest)
		case outbound.OutboundTypeOpenAIResponse:
			stripCodexClientHeaders(req.Header)
		}
		setHeaderForce(req.Header, "User-Agent", genericUAForChannel(options.Channel))
	}

	// Originator and User-Agent are one Codex identity pair. An operator header
	// may customize either value, but must not split the pair seen upstream. A genuine
	// Codex CLI downstream carries its OWN self-consistent pair; that exact pair is echoed
	// verbatim (see codexIdentityPair) so the upstream sees the same bytes a direct
	// connection would have carried, while a non-CLI downstream still gets the profile's
	// codex_exec pair (sub2api #3901: originator↔UA-first-token must pair).
	if shouldUseCodexHeaderDefaults(options.Channel, options.InboundType) {
		fingerprint := resolveFingerprintForChannel(options.Channel)
		codexUserAgent, codexOriginator := codexIdentityPair(options.InboundUserAgent, fingerprint)
		setHeaderForce(req.Header, "Originator", codexOriginator)
		setHeaderForce(req.Header, "User-Agent", codexUserAgent)
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
	clientUsedBearer := false
	clientUsedAPIKey := false
	inboundUserAgent := ""
	inboundOriginator := ""
	if ra.c != nil && ra.c.Request != nil {
		// Gate on the claude-code beta so only a CLI-shaped Bearer client gets the
		// Authorization-only mirror; a plain Bearer client (no claude-code beta)
		// keeps the historical both-headers upstream auth.
		clientUsedBearer = strings.TrimSpace(ra.c.Request.Header.Get("Authorization")) != "" &&
			strings.Contains(ra.c.Request.Header.Get("Anthropic-Beta"), "claude-code")
		// The api-key mirror is the same rule from the other side: the client presented
		// exactly one credential header (x-api-key) and no Authorization, so the outbound
		// must present exactly one too.
		clientUsedAPIKey = strings.TrimSpace(ra.c.Request.Header.Get("X-Api-Key")) != "" &&
			strings.TrimSpace(ra.c.Request.Header.Get("Authorization")) == ""
		// The client's own Originator+User-Agent pair for the codex passthrough. User-Agent
		// is read here because copyHeaders filters it as hop-by-hop, so it is absent from the
		// outbound request and can only be restored from the client request.
		inboundUserAgent = ra.c.Request.UserAgent()
		inboundOriginator = ra.c.Request.Header.Get("Originator")
	}
	ApplyChannelWireHeaders(req, ChannelWireHeaderOptions{
		Channel:              ra.channel,
		InboundType:          ra.inboundType,
		InternalRequest:      ra.internalRequest,
		ClaudeSessionID:      ra.claudeFingerprintSessionID(),
		ClientUsedBearerAuth: clientUsedBearer,
		ClientUsedAPIKeyAuth: clientUsedAPIKey,
		InboundUserAgent:     inboundUserAgent,
		InboundOriginator:    inboundOriginator,
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
		//      copyHeaders — Claude betas on the Anthropic path, Originator/X-Codex-* on the
		//      codex path. Keep only context-1m when the request separately asks for the
		//      channel's 1M capability; that beta is runtime capability, not CLI identity.
		//   2. Apply the unified generic UA on EVERY type — including the two CLI-capable
		//      ones. Leaving them bare emitted Go's default "Go-http-client/1.1", which flags
		//      the caller as a bot/script; the operator picked "no shape", not "no identity".
		//      The generic UA follows the selected profile (Debian Chrome / Ubuntu
		//      Firefox / macOS Chrome), falling back to DefaultGenericUA.
		switch channel.Type {
		case outbound.OutboundTypeAnthropic:
			applyAnthropicCloakOffBeta(req.Header, options.InternalRequest)
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
		// Mirror the client's auth style: a downstream Bearer client (claude CLI
		// auth-token mode) must produce an Authorization-only outbound, the exact
		// wire shape a genuine CLI sends to a router/proxy base (packet-verified
		// 2026-09-24: golden carries Authorization and no X-Api-Key). The outbound
		// sets both for upstream compatibility; trim the extra X-Api-Key only when
		// the outbound actually carries a Bearer (official api.anthropic.com keeps
		// X-Api-Key and never gets Authorization, so it is untouched).
		// 2026-10-10 大领导拍板(真 CLI 抓包对照): 真 CLI 指向代理/转发时**只发 authorization**,
		// 我们出站两个都发 —— 上游可识别的非 CLI 特征。规则收窄成"出站只要带着 Authorization
		// (那正是渠道 key: applyAnthropicAuthHeaders 对每个非官方 base 都注入 Bearer), 就绝不再发
		// X-Api-Key", 不再要求调用方自己也用 Bearer。为什么不会断渠道: 被删的 X-Api-Key 是
		// copyHeadersToUpstream 复制来的**调用方**那个头, 而 Authorization 里是渠道 key —— 删掉它
		// 不会让这次尝试变成"无凭证"。官方 api.anthropic.com 那条路不注入 Authorization(只发
		// X-Api-Key), 因此完全不受影响。
		if strings.TrimSpace(req.Header.Get("Authorization")) != "" {
			req.Header.Del("X-Api-Key")
		} else if options.ClientUsedAPIKeyAuth && strings.TrimSpace(req.Header.Get("X-API-Key")) != "" {
			// Mirror image of the rule above, same packet-level principle: a client that
			// authenticated with an API key only must produce an api-key-only outbound.
			// applyAnthropicAuthHeaders adds `Authorization: Bearer` for every non-official
			// base "for upstream compatibility", so on a relay-shaped upstream Octopus sent
			// two credential headers where the real CLI sends one. The guard keeps the
			// request authenticated: X-API-Key is already present and carries the channel
			// key, so dropping the Bearer copy cannot leave the attempt credential-less.
			req.Header.Del("Authorization")
		}
	case outbound.OutboundTypeOpenAIResponse:
		applyCodexHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint, options.InboundUserAgent)
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		// 2026-10-14 (user decision, "channel determines shape"): a Chat channel's
		// outbound defaults to the generic chat identity, regardless of which client
		// protocol came in. The codex CLI headers that the old branch applied when a
		// codex (Responses) client hit a Chat channel leaked originator / thread-id /
		// x-codex-* upstream and fingerprinted the request as converter traffic.
		//
		// A channel-level cloak shape may now explicitly opt INTO a CLI identity while
		// keeping the chat body/format unchanged: shape "claude" dresses the outbound
		// in the Claude CLI header set, shape "codex" in the codex CLI header set. Any
		// other value (including the "" zero value) keeps the generic identity,
		// byte-for-byte identical to the pre-shape behaviour.
		switch chatChannelShape(channel) {
		case "claude":
			// Operator explicitly dresses the chat outbound as a claude CLI. Strip any
			// codex headers a codex inbound forwarded first — a claude-dressed request
			// must not carry codex identity bytes.
			if options.InboundType == inbound.InboundTypeOpenAIResponse {
				stripCodexClientHeaders(req.Header)
			}
			applyClaudeHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint, options.ClaudeSessionID)
		case "codex":
			applyCodexHeaderDefaultsWithFingerprint(req, options.InternalRequest, fingerprint, options.InboundUserAgent)
		default: // "" or unknown: generic chat identity, byte-identical to pre-shape behaviour
			if options.InboundType == inbound.InboundTypeOpenAIResponse {
				stripCodexClientHeaders(req.Header)
			}
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

// chatChannelShape normalizes a Chat channel's cloak shape selector into the
// canonical lowercase form. Returns "claude", "codex", or "" for anything else
// (including the zero value), so a stray/foreign value fails safe to the generic
// chat identity instead of producing an undefined outbound shape.
func chatChannelShape(channel *dbmodel.Channel) string {
	if channel == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(channel.Cloak.Shape))
}

// channelTLSFace resolves the TLS face a channel's outbound must present, or ""
// when the channel has no CLI identity to protect.
//
// A channel has one when either:
//   - its type IS that protocol — an Anthropic channel speaks the Claude CLI's
//     protocol and a Responses channel speaks the Codex CLI's, so their identity is
//     not optional and there is no shape field to set; or
//   - it is a Chat channel that explicitly opted in via cloak.shape.
//
// Cloak mode "never"/"off" disables shaping for a channel in both cases (the
// operator's switch wins), which is why the cloak gate is checked first.
//
// This is deliberately separate from chatChannelShape: shape is a Chat-only field,
// so deriving the face from it alone would leave the two channels that most need a
// CLI handshake — the codex and claude ones — without one.
func channelTLSFace(channel *dbmodel.Channel) string {
	if channel == nil || !shouldApplyChannelCloak(channel.Cloak) {
		return ""
	}
	switch channel.Type {
	case outbound.OutboundTypeOpenAIResponse:
		return "codex"
	case outbound.OutboundTypeAnthropic:
		return "claude"
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		switch chatChannelShape(channel) {
		case "claude":
			return "claude"
		case "codex":
			return "codex"
		}
	}
	return ""
}

// ChannelTLSFace exposes channelTLSFace to the model-test/probe path.
//
// Why an exported wrapper instead of exporting the function itself: the relay's own
// call sites stay byte-identical, and there is exactly ONE definition of "what face
// does this channel wear". A probe that resolved its own face could drift from the
// real forward path, and the whole point of the probe is that the upstream cannot
// tell them apart. Read-only: it never mutates the channel.
func ChannelTLSFace(channel *dbmodel.Channel) string {
	return channelTLSFace(channel)
}

// LowercaseAnthropicCLIHeaderNames exposes lowercaseAnthropicCLIHeaderNames to the
// model-test/probe path, for the same test==real-traffic reason as ChannelTLSFace:
// a real Claude CLI (node/undici) puts these header NAMES on the wire in all
// lowercase, while Go's http.Header.Set canonicalizes them to `Anthropic-Beta` etc.
// That casing alone is an upstream-visible "not a CLI" tell, so a probe that skips
// this step is detectable even when every other byte matches.
func LowercaseAnthropicCLIHeaderNames(h http.Header) {
	lowercaseAnthropicCLIHeaderNames(h)
}

// chatChannelTLSFace maps a Chat channel's cloak shape to the TLS face family the
// outbound should present. Empty string = no override (path-based default).
// Retained for the Chat-specific callers and tests; channelTLSFace covers the
// protocol-implied cases too.
func chatChannelTLSFace(channel *dbmodel.Channel) string {
	if channel == nil || channel.Type != outbound.OutboundTypeOpenAIChat && channel.Type != outbound.OutboundTypeCustomOpenAIChat {
		return ""
	}
	if !shouldApplyChannelCloak(channel.Cloak) {
		return ""
	}
	switch chatChannelShape(channel) {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return ""
	}
}

func shouldUseCodexHeaderDefaults(channel *dbmodel.Channel, inboundType inbound.InboundType) bool {
	if channel == nil || !shouldApplyChannelCloak(channel.Cloak) {
		return false
	}
	if channel.Type == outbound.OutboundTypeOpenAIResponse {
		return true
	}
	// 2026-10-14 (user decision, "channel determines shape"): Chat channels no
	// longer borrow the codex identity when a codex (Responses) client calls in.
	// The old return here applied codex UA/Originator to a Chat channel's outbound,
	// fingerprinting the request as codex-converter traffic on a chat endpoint.
	// A Chat channel's outbound is always the generic chat identity.
	return false
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
	header.Del("Thread-Id")
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
	// Fallback only: a caller that declared its own x-stainless-timeout keeps it verbatim
	// (clientTimeoutHeaders is no longer a drop list), so this fills the gap for callers
	// that sent none. Never overwrite a caller's declaration here.
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

func applyAnthropicCloakOffBeta(headers http.Header, internalRequest *model.InternalLLMRequest) {
	if headers == nil {
		return
	}
	headers.Del("Anthropic-Beta")
	if model.AnthropicRequestWantsOneMillionBeta(internalRequest) {
		headers.Set("Anthropic-Beta", model.AnthropicOneMillionBeta)
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
	inboundUserAgent := ""
	if ra != nil && ra.c != nil && ra.c.Request != nil {
		inboundUserAgent = ra.c.Request.UserAgent()
	}
	applyCodexHeaderDefaultsWithFingerprint(req, internalRequest, ra.fingerprint(), inboundUserAgent)
}

// applyCodexHeaderDefaultsWithFingerprint is the shared codex header writer used by
// both the relayAttempt method and the free raw-protocol path. fp resolves the
// channel's selected fingerprint profile; with no profile every getter returns the
// global setting value, so this is byte-for-byte the prior behaviour. A selected
// profile overrides only the codex fields it sets.
//
// inboundUserAgent is the DOWNSTREAM client's own User-Agent when it has one. A genuine
// Codex CLI sends a self-consistent UA+Originator pair, so its exact UA is echoed back and
// the Originator is derived from that same UA's leading token — the upstream then sees what
// a direct connection would have carried, and the two headers can never disagree. A non-CLI
// downstream (empty or foreign UA) keeps the profile's pair, preserving the sub2api #3901
// originator↔UA-first-token pairing rule.
func applyCodexHeaderDefaultsWithFingerprint(req *http.Request, internalRequest *model.InternalLLMRequest, fp resolvedFingerprint, inboundUserAgent string) {
	// NB: a genuine Codex CLI (reqwest/hyper) sends NO `Connection` header on its plaintext
	// HTTP/1.1 leg (2026-10-13 berobox packet capture, direct-00003.req). Keep-alive is the
	// HTTP/1.1 default, so oct must not add one either — the old synthesized
	// `Connection: Keep-Alive` here was an extra header the real CLI never emits. (The
	// claude-cli HTTP/1.1 leg is unaffected: its 21-item wire table re-adds `Connection` at
	// the byte layer, see claudeHTTP1WireHeaderOrder.)
	setHeaderIfMissing(req.Header, "Content-Type", "application/json")
	// FORCE the codex identity (Originator + UA), OVERRIDING whatever a downstream client
	// leaked through. sub2api/new-api-style upstreams require the originator to pair with the
	// User-Agent's leading token and only accept the codex_exec client — a mismatch (e.g. a
	// leaked codex_exec originator against oct's codex_exec UA) is rejected (sub2api issue
	// #3901: originator↔UA-first-token must pair + version≥0.144.0). setHeaderIfMissing let a
	// copied-through client Originator survive while the UA fell back to oct's default,
	// producing exactly that mismatch on the wire. codexIdentityPair echoes a genuine Codex
	// CLI's own pair verbatim and otherwise uses the resolved fingerprint's pair, so a
	// selected profile still overrides them.
	codexUserAgent, codexOriginator := codexIdentityPair(inboundUserAgent, fp)
	setHeaderForce(req.Header, "Originator", codexOriginator)
	setHeaderForce(req.Header, "User-Agent", codexUserAgent)
	setHeaderIfMissing(req.Header, "X-Codex-Beta-Features", fp.CodexBetaFeatures())
	applyCodexResponsesLiteHeader(req.Header, internalRequest)
	applyCodexSessionHeaders(req.Header, internalRequest, fp.CodexInstallationID())
}

// isCodexFamilyUserAgent reports whether ua is a genuine Codex CLI user-agent, judging by
// its lowercase leading product token. The three captured families are codex_exec/ (headless
// exec), codex-tui/ (interactive TUI) and codex_cli_rs/ (Rust CLI). Prefix-only: the leading
// token is exactly what an upstream (sub2api #3901) pairs against the Originator.
func isCodexFamilyUserAgent(ua string) bool {
	lower := strings.ToLower(strings.TrimSpace(ua))
	return strings.HasPrefix(lower, "codex_exec/") ||
		strings.HasPrefix(lower, "codex-tui/") ||
		strings.HasPrefix(lower, "codex_cli_rs/")
}

// deprecatedCodexTUIUserAgentPrefix is the interactive-TUI product token. An upstream pairs
// the UA's leading token against the Originator header, so letting this family through
// would publish an identity the project has decided never to emit. It is rewritten to the
// Rust CLI pair below instead.
const deprecatedCodexTUIUserAgentPrefix = "codex-tui/"

// codexOriginatorFromUserAgent derives the Originator an upstream will pair a UA against:
// the UA's own leading product token. Callers use it to keep the two headers inseparable,
// because an unpaired Originator is exactly what a fingerprint check rejects.
func codexOriginatorFromUserAgent(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	token, _, _ := strings.Cut(lower, "/")
	return token
}

// isCodexFamilyOriginator reports whether an Originator header names a Codex CLI product
// token. The Originator is a bare product name (no version suffix), so it cannot be judged
// by the UA-prefix helper; the same three families are recognised here, which keeps inbound
// detection working no matter which family a client announces itself with.
func isCodexFamilyOriginator(originator string) bool {
	switch strings.ToLower(strings.TrimSpace(originator)) {
	case "codex_exec", "codex_cli_rs", "codex-tui", "codex-tui-rs":
		return true
	}
	return false
}

// codexIdentityPair selects the Originator + User-Agent pair to force on a codex outbound.
// A codex-family downstream (a genuine CLI) carries a self-consistent pair, so its EXACT
// bytes are echoed verbatim, and any Originator that is empty or disagrees with the UA's
// leading token is corrected to that token: emitting rs UA + exec Originator is precisely
// the split this rule exists to prevent. The interactive-TUI family is never echoed — it is
// rewritten to the profile's pair. Any other downstream (no CLI, or a foreign UA) maps to
// the profile's pair. Returning the pair from one place keeps Originator and User-Agent
// from ever being split across the two branches.
func codexIdentityPair(inboundUserAgent string, fp resolvedFingerprint) (userAgent, originator string) {
	inboundUserAgent = strings.TrimSpace(inboundUserAgent)
	if isCodexFamilyUserAgent(inboundUserAgent) {
		if strings.HasPrefix(strings.ToLower(inboundUserAgent), deprecatedCodexTUIUserAgentPrefix) {
			return fp.CodexUserAgent(), fp.CodexOriginator()
		}
		return inboundUserAgent, codexOriginatorFromUserAgent(inboundUserAgent)
	}
	return fp.CodexUserAgent(), fp.CodexOriginator()
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
// self-consistent codex_exec pair must reach the upstream regardless of what the caller sent.
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
