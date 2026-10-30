package relay

import (
	"net/http"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
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

	enforceCodexNoAcceptEncoding(&dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse}, h)

	if _, ok := h["Accept-Encoding"]; ok {
		t.Fatalf("codex outbound must carry NO Accept-Encoding on the wire, got %q", h.Get("Accept-Encoding"))
	}
	if h.Get("Authorization") == "" || h.Get("Content-Type") == "" {
		t.Fatalf("non-target headers must be preserved (Authorization=%q Content-Type=%q)", h.Get("Authorization"), h.Get("Content-Type"))
	}
}

// TestEnforceCodexNoAcceptEncodingStripsChatCodex is the same identity rule for a
// Chat channel that opted into shape=codex. The body stays chat; the headers wear
// the codex CLI, which sends no Accept-Encoding.
func TestEnforceCodexNoAcceptEncodingStripsChatCodex(t *testing.T) {
	for _, typ := range []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat} {
		h := http.Header{}
		h.Set("Accept-Encoding", "identity")
		enforceCodexNoAcceptEncoding(&dbmodel.Channel{
			Type:  typ,
			Cloak: dbmodel.ChannelCloak{Mode: "always", Shape: "codex"},
		}, h)
		if _, ok := h["Accept-Encoding"]; ok {
			t.Fatalf("type %d chat-codex must drop Accept-Encoding, got %q", typ, h.Get("Accept-Encoding"))
		}
	}
}

// TestEnforceCodexNoAcceptEncodingLeavesClaudeUntouched proves the guard is codex-scoped:
// a claude (Anthropic) channel legitimately advertises its Accept-Encoding via the
// transformer and must not be stripped. Chat+shape=claude and generic chat keep it too.
func TestEnforceCodexNoAcceptEncodingLeavesClaudeUntouched(t *testing.T) {
	cases := []*dbmodel.Channel{
		{Type: outbound.OutboundTypeAnthropic},
		{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
		{Type: outbound.OutboundTypeOpenAIChat},
		{Type: outbound.OutboundTypeCustomOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "unknown"}},
	}
	for _, ch := range cases {
		h := http.Header{}
		h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		enforceCodexNoAcceptEncoding(ch, h)
		if got := h.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
			t.Fatalf("type %d shape %q Accept-Encoding must be preserved, got %q", ch.Type, ch.Cloak.Shape, got)
		}
	}
}
