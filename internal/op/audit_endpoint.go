package op

import (
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// AuditEndpointForChannel 返回该渠道做模型检测时该探的端点名。
//
// 🔴 唯一来源。此前 handlers（手动检测）与 task（定时检测）各写一份一模一样的
// switch，改一份忘另一份 → 同一渠道按两种协议去探，一份绿一份红且单看都"正常"。
// 这种 bug 极难查：两边单看都正常，凑在一起才知道探的不是同一条路。
//
// 放在 op 是因为 handlers 与 task 都依赖它，不会成环；两处调用点直呼本函数，
// 不再各自留一个同名包装——留包装就等于还留着第二个名字，下次照样有人只改一处。
func AuditEndpointForChannel(ch model.Channel) string {
	switch ch.Type {
	case outbound.OutboundTypeAnthropic:
		return "anthropic_messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai_responses"
	case outbound.OutboundTypeGemini:
		return "gemini_generate_content"
	case outbound.OutboundTypeSystemOne:
		return "systemone"
	default:
		// 未知类型（含退役的 4 号位）与零值一律落 openai_chat：检测本身由
		// modeltest 的准入判定负责拒，这里不 panic、不猜协议。
		return "openai_chat"
	}
}

// CircuitProbeEndpointForScope maps the claimed relay scope to a model-test endpoint.
func CircuitProbeEndpointForScope(relayEndpoint, relayCapability string) (string, string, bool) {
	endpoint := strings.ToLower(strings.TrimSpace(relayEndpoint))
	capability := strings.ToLower(strings.TrimSpace(relayCapability))
	if endpoint == "embeddings" || endpoint == "openai_embeddings" {
		return "openai_embeddings", model.CapabilityEmbeddings, true
	}
	if strings.Contains(capability, "embedding") {
		return "", "", false
	}
	switch endpoint {
	case "chat", "chat_completions", "openai_chat":
		return "openai_chat", "", true
	case "responses", "openai_responses":
		return "openai_responses", "", true
	case "messages", "anthropic_messages":
		return "anthropic_messages", "", true
	case "gemini_generate_content", "generate_content":
		return "gemini_generate_content", "", true
	case "gemini_stream_generate_content", "stream_generate_content":
		return "gemini_stream_generate_content", "", true
	}
	return "", "", false
}
