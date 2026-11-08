package modeltest

import (
	"fmt"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// Only explicit channel tests switch protocols; saved configuration and audit probes keep their contract.
func channelForTestEndpoint(channel *dbmodel.Channel, value string) (*dbmodel.Channel, error) {
	endpoint, err := normalizeEndpoint(value)
	if err != nil {
		return nil, err
	}
	var channelType outbound.OutboundType
	switch endpoint.name {
	case "openai_chat":
		channelType = outbound.OutboundTypeOpenAIChat
	case "openai_responses":
		channelType = outbound.OutboundTypeOpenAIResponse
	case "anthropic_messages":
		channelType = outbound.OutboundTypeAnthropic
	case "gemini_generate_content":
		channelType = outbound.OutboundTypeGemini
	default:
		return channel, nil
	}
	if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		if channelType != outbound.OutboundTypeOpenAIChat {
			return nil, fmt.Errorf("自定义渠道使用完整地址，不能通过切换测试接口改写地址；请使用聊天测试，或先配置对应接口的渠道")
		}
		return channel, nil
	}
	if !outbound.IsChatChannelType(channel.Type) || channel.Type == channelType {
		return channel, nil
	}
	copy := *channel
	copy.Type = channelType
	return &copy, nil
}
