package relay

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestNativeOutboundTypesPreferMatchingProtocol(t *testing.T) {
	tests := []struct {
		name        string
		inboundType inbound.InboundType
		want        []outbound.OutboundType
		notWant     []outbound.OutboundType
	}{
		{
			name:        "openai chat prefers chat-compatible endpoints",
			inboundType: inbound.InboundTypeOpenAIChat,
			want:        []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat},
			notWant:     []outbound.OutboundType{outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeAnthropic, outbound.OutboundTypeGemini},
		},
		{
			name:        "openai responses prefers responses endpoint",
			inboundType: inbound.InboundTypeOpenAIResponse,
			want:        []outbound.OutboundType{outbound.OutboundTypeOpenAIResponse},
			notWant:     []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat, outbound.OutboundTypeAnthropic, outbound.OutboundTypeGemini},
		},
		{
			name:        "anthropic prefers messages endpoint",
			inboundType: inbound.InboundTypeAnthropic,
			want:        []outbound.OutboundType{outbound.OutboundTypeAnthropic},
			notWant:     []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeGemini},
		},
		{
			name:        "gemini prefers generate content endpoint",
			inboundType: inbound.InboundTypeGemini,
			want:        []outbound.OutboundType{outbound.OutboundTypeGemini},
			notWant:     []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeAnthropic},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nativeOutboundTypes(tt.inboundType)
			for _, typ := range tt.want {
				if !got[typ] {
					t.Fatalf("expected native outbound type %v to be preferred, got %#v", typ, got)
				}
			}
			for _, typ := range tt.notWant {
				if got[typ] {
					t.Fatalf("did not expect converted outbound type %v to be native-preferred, got %#v", typ, got)
				}
			}
		})
	}
}
