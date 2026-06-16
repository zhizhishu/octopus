package relay

import (
	"context"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func nativeProtocolChannelIDs(ctx context.Context, inboundType inbound.InboundType, items []dbmodel.GroupItem) map[int]bool {
	nativeTypes := nativeOutboundTypes(inboundType)
	if len(nativeTypes) == 0 || len(items) == 0 {
		return nil
	}

	preferred := make(map[int]bool)
	seen := make(map[int]bool, len(items))
	for _, item := range items {
		if seen[item.ChannelID] {
			continue
		}
		seen[item.ChannelID] = true
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil || channel == nil {
			continue
		}
		if nativeTypes[channel.Type] {
			preferred[item.ChannelID] = true
		}
	}
	return preferred
}

func nativeOutboundTypes(inboundType inbound.InboundType) map[outbound.OutboundType]bool {
	switch inboundType {
	case inbound.InboundTypeOpenAIChat:
		return map[outbound.OutboundType]bool{
			outbound.OutboundTypeOpenAIChat:       true,
			outbound.OutboundTypeCustomOpenAIChat: true,
		}
	case inbound.InboundTypeOpenAIResponse:
		return map[outbound.OutboundType]bool{outbound.OutboundTypeOpenAIResponse: true}
	case inbound.InboundTypeAnthropic:
		return map[outbound.OutboundType]bool{outbound.OutboundTypeAnthropic: true}
	case inbound.InboundTypeGemini:
		return map[outbound.OutboundType]bool{outbound.OutboundTypeGemini: true}
	case inbound.InboundTypeOpenAIEmbedding:
		return map[outbound.OutboundType]bool{outbound.OutboundTypeOpenAIEmbedding: true}
	default:
		return nil
	}
}
