package relay

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 火山（OutboundTypeVolcengine，4 号位）整条线已砍掉：它不再是 OpenAI wire
// 渠道类型。4 号位编号永久保留空洞，见 internal/transformer/outbound/register.go。
func TestIsOpenAIWireChannelTypeIncludesCustomChat(t *testing.T) {
	if !isOpenAIWireChannelType(outbound.OutboundTypeCustomOpenAIChat) {
		t.Fatalf("expected OutboundTypeCustomOpenAIChat to be supported for raw protocol")
	}
	if !isOpenAIWireChannelType(outbound.OutboundTypeOpenAIChat) {
		t.Fatalf("expected OutboundTypeOpenAIChat to be supported for raw protocol")
	}
	if isOpenAIWireChannelType(outbound.OutboundType(4)) {
		t.Fatalf("type 4 is the retired (obsolete) channel type; it must not be an OpenAI wire type")
	}
}
