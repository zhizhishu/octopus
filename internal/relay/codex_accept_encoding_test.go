package relay

import (
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// TestEnforceCodexNoAcceptEncodingStripsResponsesOverride locks the fingerprint
// invariant that a codex (OpenAI Responses) outbound never carries Accept-Encoding,
// even when an operator CustomHeader / the shipped openaiPython preset tried to set
// `Accept-Encoding: identity`. Without the guard that value would reach the relay and
// re-introduce exactly the tell the codex fix removed.
func TestEnforceCodexNoAcceptEncodingStripsResponsesOverride(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Encoding", "identity")
	h.Set("Authorization", "Bearer x")
	h.Set("Content-Type", "application/json")

	enforceCodexNoAcceptEncoding(outbound.OutboundTypeOpenAIResponse, h)

	if _, ok := h["Accept-Encoding"]; ok {
		t.Fatalf("codex outbound must carry NO Accept-Encoding on the wire, got %q", h.Get("Accept-Encoding"))
	}
	if h.Get("Authorization") == "" || h.Get("Content-Type") == "" {
		t.Fatalf("non-target headers must be preserved (Authorization=%q Content-Type=%q)", h.Get("Authorization"), h.Get("Content-Type"))
	}
}

// TestEnforceCodexNoAcceptEncodingLeavesClaudeUntouched proves the guard is codex-scoped:
// a claude (Anthropic) channel legitimately advertises its Accept-Encoding via the
// transformer and must not be stripped.
func TestEnforceCodexNoAcceptEncodingLeavesClaudeUntouched(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	enforceCodexNoAcceptEncoding(outbound.OutboundTypeAnthropic, h)

	if got := h.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
		t.Fatalf("claude Accept-Encoding must be preserved, got %q", got)
	}
}
