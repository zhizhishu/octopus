package relay

import (
	"net/http"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// Test traffic must wear the SAME outbound identity as a real forward, otherwise the
// upstream can tell a probe from real traffic and simply refuses it — the operator
// then sees "this channel cannot be tested at all". These tests pin the two exported
// wrappers the model-test path uses, and deliberately assert they agree with the
// relay's own internal resolution rather than re-deriving the answer, so the two
// paths can never drift apart.

// TestChannelTLSFaceIsExportedAndMatchesRelay pins that the exported wrapper returns
// exactly what the relay forward path uses. If these ever diverge, probes silently
// stop matching real traffic.
func TestChannelTLSFaceIsExportedAndMatchesRelay(t *testing.T) {
	channels := []*dbmodel.Channel{
		{Type: outbound.OutboundTypeAnthropic},
		{Type: outbound.OutboundTypeOpenAIResponse},
		{Type: outbound.OutboundTypeOpenAIChat},
		{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
		{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "codex"}},
		{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "nonsense"}},
	}
	for _, ch := range channels {
		want := channelTLSFace(ch)
		got := ChannelTLSFace(ch)
		if got != want {
			t.Fatalf("ChannelTLSFace(type=%d shape=%q) = %q, want %q (relay uses %q); "+
				"a probe wearing a different face than real traffic is detectable and refused",
				ch.Type, ch.Cloak.Shape, got, want, want)
		}
	}
}

// TestChannelTLSFaceNilIsSafe guards the nil channel the probe helper may be handed.
func TestChannelTLSFaceNilIsSafe(t *testing.T) {
	if got := ChannelTLSFace(nil); got != "" {
		t.Fatalf("ChannelTLSFace(nil) = %q, want empty", got)
	}
}

// TestChannelTLSFacePerChannelIdentity documents what each channel shape must wear,
// so a future refactor cannot quietly make a codex channel present a Claude handshake
// (or none) — the exact mismatch that gets probes rejected.
func TestChannelTLSFacePerChannelIdentity(t *testing.T) {
	cases := []struct {
		name string
		ch   *dbmodel.Channel
		want string
		why  string
	}{
		{
			name: "anthropic channel is always claude",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
			want: "claude",
			why:  "an Anthropic channel speaks the Claude CLI protocol; its identity is not optional",
		},
		{
			name: "responses channel is always codex",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
			want: "codex",
			why:  "a Responses channel speaks the Codex CLI protocol",
		},
		{
			name: "chat channel defaults to no face",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
			want: "",
			why:  "a plain chat channel never claimed a CLI identity",
		},
		{
			name: "chat channel opted into claude",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
			want: "claude",
			why:  "the operator explicitly dressed this chat channel as a Claude CLI",
		},
		{
			name: "chat channel opted into codex",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "codex"}},
			want: "codex",
			why:  "the operator explicitly dressed this chat channel as a Codex CLI",
		},
		{
			name: "unknown shape fails safe to no face",
			ch:   &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "gemini-ish"}},
			want: "",
			why:  "a stray value must not invent an identity that was never asked for",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChannelTLSFace(tc.ch); got != tc.want {
				t.Fatalf("ChannelTLSFace = %q, want %q (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestChannelTLSFaceRespectsCloakOff pins the operator's switch: cloak "never" must
// disable shaping for every channel type, including the two CLI-implied ones.
func TestChannelTLSFaceRespectsCloakOff(t *testing.T) {
	for _, chType := range []outbound.OutboundType{
		outbound.OutboundTypeAnthropic,
		outbound.OutboundTypeOpenAIResponse,
		outbound.OutboundTypeOpenAIChat,
	} {
		ch := &dbmodel.Channel{Type: chType, Cloak: dbmodel.ChannelCloak{Mode: "never"}}
		if got := ChannelTLSFace(ch); got != "" {
			t.Fatalf("cloak=never must disable the TLS face for type %d, got %q", chType, got)
		}
	}
}

// TestLowercaseAnthropicCLIHeaderNamesMatchesRelay pins the casing fix. A genuine
// Claude CLI (node/undici) puts these NAMES on the wire in all lowercase; Go's
// canonicalization produces `Anthropic-Beta`, and that difference alone is an
// upstream-visible "not a CLI" tell.
func TestLowercaseAnthropicCLIHeaderNamesMatchesRelay(t *testing.T) {
	newHeaders := func() http.Header {
		h := http.Header{}
		h.Set("Anthropic-Beta", "claude-code-20250219")
		h.Set("Anthropic-Version", "2023-06-01")
		h.Set("X-App", "cli")
		return h
	}

	// The exported wrapper must produce byte-identical names to the relay's own
	// function, or a probe's header casing would differ from real traffic.
	wrapped := newHeaders()
	LowercaseAnthropicCLIHeaderNames(wrapped)

	internal := newHeaders()
	lowercaseAnthropicCLIHeaderNames(internal)

	if len(wrapped) != len(internal) {
		t.Fatalf("wrapper produced %d header names, relay produced %d", len(wrapped), len(internal))
	}
	for name, values := range internal {
		got, ok := wrapped[name]
		if !ok {
			t.Fatalf("wrapper lost header name %q that the relay emits", name)
		}
		if len(got) != len(values) {
			t.Fatalf("header %q has %d values via wrapper, %d via relay", name, len(got), len(values))
		}
	}

	// And the wire form must actually be lowercase, which is the whole point.
	for _, wire := range []string{"anthropic-beta", "anthropic-version", "x-app"} {
		if _, ok := wrapped[wire]; !ok {
			t.Fatalf("expected all-lowercase wire name %q, got names %v", wire, keysOf(wrapped))
		}
	}
	// The canonical (wrong) forms must be gone, not merely duplicated.
	for _, canonical := range []string{"Anthropic-Beta", "Anthropic-Version", "X-App"} {
		if _, ok := wrapped[canonical]; ok {
			t.Fatalf("canonical name %q must be removed, not duplicated", canonical)
		}
	}
}

func keysOf(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	return out
}

// TestLowercaseAnthropicCLIHeaderNamesIsNoOpWhenAbsent proves the wrapper cannot
// invent headers on a chat/codex request that never carried them.
func TestLowercaseAnthropicCLIHeaderNamesIsNoOpWhenAbsent(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Originator", "codex_cli_rs")

	LowercaseAnthropicCLIHeaderNames(h)

	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("unrelated header changed: Content-Type = %q", got)
	}
	if got := h.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("unrelated header changed: Originator = %q", got)
	}
	if len(h) != 2 {
		t.Fatalf("wrapper must not add headers, got %v", keysOf(h))
	}
}
