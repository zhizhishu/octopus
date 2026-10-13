package client

import (
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// chromePseudoHeaderOrder is the HTTP/2 pseudo-header order presented on the uTLS
// (Chrome ClientHello) path. It is set on EVERY fhttp request so fhttp never falls
// back to its linked-transport PseudoHeaderOrder (which would be nil here).
var chromePseudoHeaderOrder = []string{":method", ":authority", ":scheme", ":path"}

// claudeCanonicalHeaderOrder is the exact regular-header order a genuine
// claude-cli 2.1.198 emits, captured on the wire (2026-07-03). Lowercase: fhttp
// lowercases each actual header key before matching against this list and before
// hpack-encoding it, so this list drives the on-wire HTTP/2 HEADERS-frame field
// order. Headers present but not listed are emitted after, in fhttp's default order.
var claudeCanonicalHeaderOrder = []string{
	"accept",
	"authorization",
	"content-type",
	"user-agent",
	"x-claude-code-session-id",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"x-stainless-timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	// 2026-10-10 保序抓包(同一跳原始字节证明代理没重排): 真 CLI 自己发的业务头只有 1-17,
	// 14 anthropic-beta / 15 anthropic-dangerous-... / 16 anthropic-version / **17 x-app** ——
	// **没有 x-api-key**(真 CLI 指向代理/转发时只发 authorization)。这条死条目删掉; 我们出站自
	// 184ce53 起也不再发它, 于是"表 = 真 CLI 业务头顺序"。
	"x-app",
	// 下面这个是真 CLI 的 HTTP 栈自己追加的(18-21: Connection/Host/Accept-Encoding/Content-Length),
	// 不是 CLI 业务头, 也不模仿 —— 只把 accept-encoding 留在表尾, 免得它被排到业务头前面。
	"accept-encoding",
}

// codexCanonicalHeaderOrder is the exact regular-header order a genuine codex
// 0.144.1 emits on /backend-api/codex/responses, packet-verified on the wire
// (2026-07-10). 0.144.x added x-openai-internal-codex-responses-lite at position
// 4; the set/order is identical across the codex_exec / codex-tui / codex_cli_rs
// identities (only the originator header + UA product token differ between them).
var codexCanonicalHeaderOrder = []string{
	"x-codex-beta-features",
	"x-codex-window-id",
	"x-codex-turn-metadata",
	"x-openai-internal-codex-responses-lite",
	"x-client-request-id",
	"session-id",
	"thread-id",
	"accept",
	"content-type",
	"authorization",
	"originator",
	"user-agent",
}

// claudeHTTP1WireHeaderOrder is the byte-exact header block a genuine claude-cli emits
// on its PLAINTEXT HTTP/1.1 leg, captured and compared byte-by-byte (2026-10-10). Items
// 1-17 are the CLI's own business headers; 18-21 are appended by the CLI's own HTTP
// stack (Connection / Host / Accept-Encoding / Content-Length).
//
// This SUPERSEDES the older "the trailing four belong to our HTTP stack, we do not
// imitate them" reading (still recorded on claudeCanonicalHeaderOrder above, which is a
// DIFFERENT table for a DIFFERENT layer): the 2026-10-10 ruling is that byte-exactness
// includes header order, so this 21-item sequence is the target for HTTP/1.1.
//
// Casing is part of the bytes and is written to the wire verbatim, which is why
// `anthropic-beta` / `anthropic-version` / `x-app` are lowercase while `Accept`,
// `Content-Type` and `X-Stainless-OS` are not.
//
// Consumed ONLY by the plaintext HTTP/1.1 path (ordered_conn.go). It is NOT
// claudeCanonicalHeaderOrder: that list is lowercase and drives fhttp's h2 HPACK field
// order (utls_transport.go), which must not change here.
var claudeHTTP1WireHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	// 18-21: the CLI's own HTTP stack. `Host` / `Accept-Encoding` / `Content-Length`
	// already exist in Go's serialisation and are moved here; `Connection` does NOT
	// exist and is ADDED (see synthesizedHTTP1HeaderLine) — Go's net/http never emits it.
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

// codexHTTP1WireHeaderOrder is the byte-exact header block a genuine Codex CLI (0.161.0)
// emits on its PLAINTEXT HTTP/1.1 leg to /v1/responses, captured and compared on the wire
// (2026-10-13 berobox leg capture; source request file `direct-00003.req`, where direct =
// the real CLI straight to the upstream). The real CLI's HTTP stack is reqwest/hyper, which
// writes EVERY header name in lowercase — so this list is lowercase and, because
// ordered_conn.go's rewriter writes the table's spelling verbatim, that lowercase spelling
// IS the on-wire byte. Exactly 14 items, `host` / `content-length` closing the block.
//
// Consumed ONLY by the plaintext HTTP/1.1 path (ordered_conn.go). It is NOT
// codexCanonicalHeaderOrder: that list is a DIFFERENT contract for a DIFFERENT layer and
// drives fhttp's h2 HPACK field order (utls_transport.go), which must not change here.
var codexHTTP1WireHeaderOrder = []string{
	"x-codex-beta-features",
	"x-codex-window-id",
	"x-codex-turn-metadata",
	"x-openai-internal-codex-responses-lite",
	"x-client-request-id",
	"session-id",
	"thread-id",
	"accept",
	"content-type",
	"authorization",
	"originator",
	"user-agent",
	"host",
	"content-length",
}

// http1WireHeaderOrderForTarget maps one HTTP/1.1 request target to its captured wire
// order. Deliberately pure — no settings — so tests can pin the byte sequence.
//
// Both captured CLI baselines are covered: the Anthropic /v1/messages leg (claude-cli) and
// the OpenAI-Responses /v1/responses leg (codex CLI). Any other target returns nil, i.e. its
// block is passed through byte-for-byte exactly as Go serialised it.
func http1WireHeaderOrderForTarget(requestTarget string) []string {
	lower := strings.ToLower(requestTarget)
	switch {
	case strings.Contains(lower, "/responses"):
		return codexHTTP1WireHeaderOrder
	case strings.Contains(lower, "/messages"):
		return claudeHTTP1WireHeaderOrder
	default:
		return nil
	}
}

// http1RequestHeaderOrder is the production hook handed to every plaintext dial. The
// switch is read PER REQUEST rather than baked into the transport, because the
// transports themselves are cached process-wide: this way turning the setting off
// takes effect immediately, including on connections already sitting in the pool. Off
// (or unset) returns nil == the header block is left exactly as Go serialised it, which
// is byte-for-byte today's outbound request.
func http1RequestHeaderOrder(method, requestTarget string) []string {
	if !upstreamHTTP1HeaderOrderEnabled() {
		return nil
	}
	return http1WireHeaderOrderForTarget(requestTarget)
}

// upstreamHTTP1HeaderOrderEnabled reads the switch for the plaintext HTTP/1.1
// wire-order rewrite. Default on: a missing or unreadable setting means on, so an
// existing database that never stored the key still gets the captured order. Set the
// key to false to fall back to Go's own header order.
func upstreamHTTP1HeaderOrderEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyUpstreamHTTP1HeaderOrder)
	if err != nil {
		return true
	}
	return enabled
}

// canonicalHeaderOrderForPath picks the genuine-CLI regular-header order for an
// upstream request by API path: Anthropic /v1/messages -> claude-cli order, the
// OpenAI-Responses /v1/responses (codex) path -> codex_exec order. Other paths
// (gemini / images / plain openai-chat) return nil: we have no captured CLI header
// order to match for them, so fhttp uses its default ordering.
func canonicalHeaderOrderForPath(path string) []string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/messages"):
		return claudeCanonicalHeaderOrder
	case strings.Contains(p, "/responses"):
		return codexCanonicalHeaderOrder
	default:
		return nil
	}
}
