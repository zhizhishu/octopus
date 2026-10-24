package outbound

import (
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/authropic"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/gemini"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

type OutboundType int

// 🔴 OutboundType 是 iota 下标编号，前端的 ChannelType 与库里的 channels.type
// 都按这个下标存。4 号位是退役的 Volcengine 渠道类型（整条线已砍掉），编号
// **永久保留空洞，绝不复用给别的类型**：删掉/挪动这一行，embedding 会变 4、
// custom 会变 5，库里所有存量渠道立刻错位（做嵌入的渠道被当成自定义 chat
// 发出去），那是数据事故而不是代码问题。
const (
	OutboundTypeOpenAIChat OutboundType = iota
	OutboundTypeOpenAIResponse
	OutboundTypeAnthropic
	OutboundTypeGemini
	// 4 号位：退役的 Volcengine。用空白标识符占位，编号永久空着。
	_obsoleteVolcengineSlot
	OutboundTypeOpenAIEmbedding
	OutboundTypeCustomOpenAIChat
)

// EmbeddingChannelTypes 定义支持 embedding 请求的 channel 类型集合
var EmbeddingChannelTypes = map[OutboundType]bool{
	OutboundTypeOpenAIEmbedding: true,
}

// ChatChannelTypes 定义支持 chat 请求的 channel 类型集合
var ChatChannelTypes = map[OutboundType]bool{
	OutboundTypeOpenAIChat:       true,
	OutboundTypeOpenAIResponse:   true,
	OutboundTypeAnthropic:        true,
	OutboundTypeGemini:           true,
	OutboundTypeCustomOpenAIChat: true,
}

// IsEmbeddingChannelType 判断 channel 类型是否支持 embedding 请求
func IsEmbeddingChannelType(channelType OutboundType) bool {
	return EmbeddingChannelTypes[channelType]
}

// IsChatChannelType 判断 channel 类型是否支持 chat 请求
func IsChatChannelType(channelType OutboundType) bool {
	return ChatChannelTypes[channelType]
}

var outboundFactories = map[OutboundType]func() model.Outbound{
	OutboundTypeOpenAIChat:       func() model.Outbound { return &openai.ChatOutbound{} },
	OutboundTypeOpenAIResponse:   func() model.Outbound { return &openai.ResponseOutbound{} },
	OutboundTypeOpenAIEmbedding:  func() model.Outbound { return &openai.EmbeddingOutbound{} },
	OutboundTypeCustomOpenAIChat: func() model.Outbound { return &openai.CustomChatOutbound{} },
	OutboundTypeAnthropic:        func() model.Outbound { return &authropic.MessageOutbound{} },
	OutboundTypeGemini:           func() model.Outbound { return &gemini.MessagesOutbound{} },
}

func Get(outboundType OutboundType) model.Outbound {
	if factory, ok := outboundFactories[outboundType]; ok {
		return factory()
	}
	return nil
}
