package relay

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestIsOpenAIWireChannelTypeIncludesCustomChatAndVolcengine(t *testing.T) {
	if !isOpenAIWireChannelType(outbound.OutboundTypeCustomOpenAIChat) {
		t.Fatalf("expected OutboundTypeCustomOpenAIChat to be supported for raw protocol")
	}
	if !isOpenAIWireChannelType(outbound.OutboundTypeVolcengine) {
		t.Fatalf("expected OutboundTypeVolcengine to be supported for raw protocol")
	}
	if !isOpenAIWireChannelType(outbound.OutboundTypeOpenAIChat) {
		t.Fatalf("expected OutboundTypeOpenAIChat to be supported for raw protocol")
	}
}
