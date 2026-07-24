package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// TestShouldForceAnthropicStreamUpstream pins the gate that makes octopus stream to
// the upstream for Claude-Code-cloaked Anthropic channels (so non-stream clients are
// served by aggregating the SSE back). the relay and similar relays refuse non-stream
// requests on gated models, so this must stay on by default for Anthropic channels
// and only opt out when cloak is explicitly disabled.
func TestShouldForceAnthropicStreamUpstream(t *testing.T) {
	cases := []struct {
		name  string
		typ   outbound.OutboundType
		cloak dbmodel.ChannelCloak
		want  bool
	}{
		{"anthropic default cloak", outbound.OutboundTypeAnthropic, dbmodel.ChannelCloak{}, true},
		{"anthropic cloak auto", outbound.OutboundTypeAnthropic, dbmodel.ChannelCloak{Mode: "auto"}, true},
		{"anthropic cloak always", outbound.OutboundTypeAnthropic, dbmodel.ChannelCloak{Mode: "always"}, true},
		{"anthropic cloak off opts out", outbound.OutboundTypeAnthropic, dbmodel.ChannelCloak{Mode: "off"}, false},
		{"anthropic cloak never opts out", outbound.OutboundTypeAnthropic, dbmodel.ChannelCloak{Mode: "never"}, false},
		{"openai chat never forced", outbound.OutboundTypeOpenAIChat, dbmodel.ChannelCloak{}, false},
		{"openai responses never forced", outbound.OutboundTypeOpenAIResponse, dbmodel.ChannelCloak{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ra := &relayAttempt{channel: &dbmodel.Channel{Type: tc.typ, Cloak: tc.cloak}}
			if got := ra.shouldForceAnthropicStreamUpstream(); got != tc.want {
				t.Fatalf("shouldForceAnthropicStreamUpstream() = %v, want %v", got, tc.want)
			}
		})
	}

	var nilRA *relayAttempt
	if nilRA.shouldForceAnthropicStreamUpstream() {
		t.Fatal("nil relayAttempt must not force stream")
	}
	if (&relayAttempt{}).shouldForceAnthropicStreamUpstream() {
		t.Fatal("nil channel must not force stream")
	}
}
