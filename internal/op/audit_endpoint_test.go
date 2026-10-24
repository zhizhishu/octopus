package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 「同一个渠道做检测时该探哪个端点」这件事只有一份判据：op.AuditEndpointForChannel。
// handlers 的手动检测与 task 的定时检测都直呼它（见 audit_endpoint_single_source_test.go）。
// 下面逐格钉死它的返回值 —— 改这张表等于改语义，必须是有意的决定。
func TestAuditEndpointForChannelIsPinned(t *testing.T) {
	cases := []struct {
		name string
		typ  outbound.OutboundType
		want string
	}{
		{"openai chat", outbound.OutboundTypeOpenAIChat, "openai_chat"},
		{"custom openai chat", outbound.OutboundTypeCustomOpenAIChat, "openai_chat"},
		{"openai responses", outbound.OutboundTypeOpenAIResponse, "openai_responses"},
		{"anthropic", outbound.OutboundTypeAnthropic, "anthropic_messages"},
		{"gemini", outbound.OutboundTypeGemini, "gemini_generate_content"},
		// 嵌入渠道没有自己的检测协议：落到 default 的 openai_chat。
		// 能不能测不归这里判 —— 由 modeltest.NewProbeSender 的准入负责拒
		// （它按渠道类型/能力位拒绝），这里只回"该探的协议名"。
		{"openai embedding", outbound.OutboundTypeOpenAIEmbedding, "openai_chat"},
		// 4 号位是退役的 Volcengine，编号永久空着。未知类型不许 panic 也不许猜协议。
		{"retired volcengine slot 4", outbound.OutboundType(4), "openai_chat"},
		{"零值", outbound.OutboundType(0), "openai_chat"},
	}
	for _, tc := range cases {
		got := AuditEndpointForChannel(model.Channel{Type: tc.typ})
		if got != tc.want {
			t.Errorf("%s (type=%d) 检测端点 = %q, want %q", tc.name, int(tc.typ), got, tc.want)
		}
	}
}

// 未知类型（以及任何越界编号）都必须落 openai_chat 且不 panic：
// 库里的 channels.type 可能被手工改过，一条看不懂的类型不许把检测打成崩溃。
func TestAuditEndpointForChannelUnknownTypesFallBack(t *testing.T) {
	known := map[string]bool{
		"openai_chat":             true,
		"openai_responses":        true,
		"anthropic_messages":      true,
		"gemini_generate_content": true,
	}
	for i := -5; i <= 20; i++ {
		typ := outbound.OutboundType(i)
		got := AuditEndpointForChannel(model.Channel{Type: typ})
		if got == "" {
			t.Fatalf("type=%d 的检测端点返回空串", i)
		}
		if !known[got] {
			t.Fatalf("type=%d 的检测端点 = %q，不在已知四值之内", i, got)
		}
	}
	// 明确钉一下"未知编号"这一格，避免上面的循环被改成空集也绿。
	if got := AuditEndpointForChannel(model.Channel{Type: outbound.OutboundType(4)}); got != "openai_chat" {
		t.Fatalf("退役的 4 号位 = %q, want openai_chat", got)
	}
	if got := AuditEndpointForChannel(model.Channel{Type: outbound.OutboundType(99)}); got != "openai_chat" {
		t.Fatalf("未知 type=99 = %q, want openai_chat", got)
	}
}
