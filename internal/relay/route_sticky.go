package relay

import dbmodel "github.com/bestruirui/octopus/internal/model"

// routeStickyEnabled 决定某次请求是否参与「会话粘性」(sticky route)。
//
// 背景：sticky 会把「同一会话」的后续请求钉在最初命中的渠道上，以提升上游 prompt-cache
// 命中率。但在轮询/负载均衡分组里，只要客户端（或 oct 自造）带上稳定的会话键，sticky 就会
// 让同会话 100% 永远打同一个渠道——轮询彻底失效（真机日志实测 gpt-5.5 同会话全打一个渠道，
// 同优先级其它渠道一次都轮不到）。
//
// 分级策略：
//   - 填充优先(FillFirst/Failover)：本就要把流量集中在头部渠道换取 cache 命中，全程 sticky。
//   - 其余(轮询/随机/加权/智能)：只对「换渠道会破坏正确性」的会话来源保留 sticky——
//     previous_response_id（codex 上一轮响应绑定该渠道）、线程/追踪/会话级 header/metadata
//     （claude-code / codex 多轮）。对「换渠道只丢 prompt-cache、不影响正确性」的来源
//     （prompt_cache_key、oct 自造 request_fingerprint、user、safety_identifier）放开，
//     让请求真正在同优先级渠道间轮转分摊。
//
// 只影响选路，不改任何出站字节/头/指纹（shape 零回归）。
func routeStickyEnabled(mode dbmodel.GroupMode, sessionSource string) bool {
	if mode == dbmodel.GroupModeFillFirst {
		return true
	}
	return isCorrectnessCriticalSessionSource(sessionSource)
}

// isCorrectnessCriticalSessionSource 报告某个会话来源是否「换渠道会破坏正确性」。
// 返回 false 的来源都是纯优化型：换渠道最多丢一次 prompt-cache 命中，不会让请求出错。
// 来源字符串由 route_session_key.go 的 clientSessionInfo.Source 产出。
func isCorrectnessCriticalSessionSource(sessionSource string) bool {
	switch sessionSource {
	case "",
		"body:prompt_cache_key",
		"body:user",
		"body:safety_identifier",
		"octopus:request_fingerprint":
		return false
	default:
		// header:* / metadata:* / client_metadata:* / body:previous_response_id 等：
		// 线程/会话/上一轮响应级绑定，换渠道会让 codex/claude 多轮对话崩，必须保留 sticky。
		return true
	}
}
