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
	"x-api-key",
	"x-app",
	"accept-encoding",
}

// codexCanonicalHeaderOrder is the exact regular-header order a genuine
// codex_exec 0.142.5 emits, captured on the wire (2026-07-03).
var codexCanonicalHeaderOrder = []string{
	"x-codex-beta-features",
	"x-codex-window-id",
	"x-codex-turn-metadata",
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
