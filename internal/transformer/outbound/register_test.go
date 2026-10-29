package outbound

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// 🔴 编号铁律：OutboundType 是 iota 下标编号，前端的 ChannelType 逐位对应，
// 库里的 channels.type 也按这个下标存。
//
//	0 OpenAIChat   1 OpenAIResponse   2 Anthropic   3 Gemini
//	4 (退役的 Volcengine，编号永久空着)
//	5 OpenAIEmbedding   6 CustomOpenAIChat   7 SystemOne
//
// 这条测试的存在理由只有一个：钉死 5/6/7 三个数字。后人"顺手整理"删掉 4 号位那行
// 占位常量，embedding 会变 4、custom 会变 5 —— 库里做嵌入的存量渠道会被当成
// 自定义 chat 发出去。那不是代码问题，是数据事故。SystemOne 必须紧挨在
// CustomOpenAIChat 后面，不许插进 4 号空洞。
func TestOutboundTypeNumberingKeepsRetiredSlotFourEmpty(t *testing.T) {
	if got := int(OutboundTypeOpenAIChat); got != 0 {
		t.Fatalf("OutboundTypeOpenAIChat = %d, want 0", got)
	}
	if got := int(OutboundTypeOpenAIResponse); got != 1 {
		t.Fatalf("OutboundTypeOpenAIResponse = %d, want 1", got)
	}
	if got := int(OutboundTypeAnthropic); got != 2 {
		t.Fatalf("OutboundTypeAnthropic = %d, want 2", got)
	}
	if got := int(OutboundTypeGemini); got != 3 {
		t.Fatalf("OutboundTypeGemini = %d, want 3", got)
	}
	if got := int(OutboundTypeOpenAIEmbedding); got != 5 {
		t.Fatalf("OutboundTypeOpenAIEmbedding = %d, want 5 (4 号位永久空着，绝不许被挪走)", got)
	}
	if got := int(OutboundTypeCustomOpenAIChat); got != 6 {
		t.Fatalf("OutboundTypeCustomOpenAIChat = %d, want 6 (4 号位永久空着，绝不许被挪走)", got)
	}
	if got := int(OutboundTypeSystemOne); got != 7 {
		t.Fatalf("OutboundTypeSystemOne = %d, want 7 (必须接在 CustomOpenAIChat 后面，4 号位继续空着)", got)
	}
}

// 4 号位没有类型、没有出厂适配器：存量 type=4 的行读出来只能落到"未知类型"。
// 不崩、不静默当成别的类型用 —— 调用点的 default 分支负责 Skip 它。
func TestRetiredSlotFourHasNoOutbound(t *testing.T) {
	retired := OutboundType(4)
	if adapter := Get(retired); adapter != nil {
		t.Fatalf("Get(4) = %T, want nil (4 号位是退役类型，不许有出厂适配器)", adapter)
	}
	if IsChatChannelType(retired) {
		t.Fatalf("4 号位不许被当成 chat 渠道类型")
	}
	if IsEmbeddingChannelType(retired) {
		t.Fatalf("4 号位不许被当成 embedding 渠道类型")
	}
}

// 5/6 号位的出厂适配器同样不许被"顺手整理"改掉：它们才是库里存量行的真实语义。
func TestEmbeddingAndCustomChatFactoriesUnchanged(t *testing.T) {
	if _, ok := Get(OutboundTypeOpenAIEmbedding).(*openai.EmbeddingOutbound); !ok {
		t.Fatalf("type 5 must still build an EmbeddingOutbound")
	}
	if _, ok := Get(OutboundTypeCustomOpenAIChat).(*openai.CustomChatOutbound); !ok {
		t.Fatalf("type 6 must still build a CustomChatOutbound")
	}
	if adapter := Get(OutboundTypeSystemOne); adapter != nil {
		t.Fatalf("Get(7) = %T, want nil (SystemOne 不走出站聊天适配器)", adapter)
	}
	if IsChatChannelType(OutboundTypeSystemOne) {
		t.Fatal("type 7 must not be a chat channel type")
	}
	if IsEmbeddingChannelType(OutboundTypeSystemOne) {
		t.Fatal("type 7 must not be an embedding channel type")
	}
}
