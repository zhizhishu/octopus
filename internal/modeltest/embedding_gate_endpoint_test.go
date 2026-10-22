package modeltest

import (
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 准入必须按"本次要测什么"判，不能是 OR。
//
// 旧的 OR 判据（!IsChatChannelType && !SupportsCapability）之下，任何 Chat 渠道
// 都能测嵌入端点 —— 但 relay 侧根本不会把嵌入流量调度给它（能力位没有
// embeddings），于是检测报告一条生产走不通的"可用"，是假绿灯。
//
// 反向同样要守住：没声明 embeddings 的 Chat 渠道测**聊天**端点必须照常放行，
// 这是存量行为，不能被这次改动误伤。
func TestEmbeddingEndpointOnChatChannelWithoutCapabilityIsRejected(t *testing.T) {
	ctx := setupModelTestDB(t)

	plainChat := dbmodel.Channel{
		Name:     "plain-chat",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: "https://chat.example.test"}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
	}

	// 测嵌入端点：必须被挡，理由是"没声明 embeddings 能力"。
	blocked, err := RunChannel(ctx, plainChat, dbmodel.ModelTestRequest{Model: "m", Endpoint: embeddingTestEndpointName})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if len(blocked.Results[0].Attempts) == 0 {
		t.Fatalf("应产生跳过记录, got %#v", blocked.Results[0])
	}
	msg := blocked.Results[0].Attempts[0].Msg
	if !strings.Contains(msg, "embeddings capability") {
		t.Fatalf("普通 Chat 渠道测嵌入端点必须被能力位挡掉, got %q", msg)
	}
	if strings.Contains(msg, "does not support model connectivity tests") {
		t.Fatalf("挡错了理由：这是能力位不足，不是渠道类型不支持, got %q", msg)
	}

	// 测聊天端点：存量行为，必须照常放行（不能出现上面那两种拒绝理由）。
	allowed, err := RunChannel(ctx, plainChat, dbmodel.ModelTestRequest{Model: "m", Endpoint: "openai_chat"})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if len(allowed.Results[0].Attempts) > 0 {
		m := allowed.Results[0].Attempts[0].Msg
		if strings.Contains(m, "embeddings capability") || strings.Contains(m, "does not support model connectivity tests") {
			t.Fatalf("普通 Chat 渠道测聊天端点被误挡: %q", m)
		}
	}
}
