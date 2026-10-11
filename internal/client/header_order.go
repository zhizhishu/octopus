package client

import "strings"

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
