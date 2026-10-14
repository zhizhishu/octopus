package openai

import (
	"net/url"
	"testing"
)

func TestMergeInboundQueryStripsBetaParam(t *testing.T) {
	// Cross-protocol: anthropic /v1/messages?beta=true -> chat /v1/chat/completions.
	// The beta param is anthropic-specific and must not leak into the outbound
	// chat URL (a real chat client never sends it; its presence fingerprints
	// converter traffic).
	inboundQuery := url.Values{"beta": {"true"}}
	outboundURL := &url.URL{Path: "/v1/chat/completions"}

	mergeInboundQuery(outboundURL, inboundQuery)

	if got := outboundURL.RawQuery; got != "" {
		t.Errorf("beta param leaked into chat outbound: RawQuery=%q, want empty", got)
	}
}

func TestMergeInboundQueryPreservesNonAnthropicParams(t *testing.T) {
	// Legit chat query params (e.g. a provider's custom switch) must still
	// pass through when the inbound is same-protocol chat.
	inboundQuery := url.Values{"session_id": {"abc123"}}
	outboundURL := &url.URL{Path: "/v1/chat/completions"}

	mergeInboundQuery(outboundURL, inboundQuery)

	want := "session_id=abc123"
	if got := outboundURL.RawQuery; got != want {
		t.Errorf("non-anthropic param dropped: RawQuery=%q, want %q", got, want)
	}
}

func TestMergeInboundQueryBetaCaseInsensitive(t *testing.T) {
	// "Beta" / "BETA" variants must also be stripped.
	inboundQuery := url.Values{"Beta": {"true"}}
	outboundURL := &url.URL{Path: "/v1/responses"}

	mergeInboundQuery(outboundURL, inboundQuery)

	if got := outboundURL.RawQuery; got != "" {
		t.Errorf("case-variant beta leaked: RawQuery=%q, want empty", got)
	}
}
