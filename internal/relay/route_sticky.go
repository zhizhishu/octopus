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
// 上面「放开纯优化型来源」是一次刻意的**分摊优先**权衡：宁可丢 prompt-cache 命中，也要让轮询
// 真的轮起来。但这个取舍不是所有部署都想要——渠道少、上游按 cache 计价的部署更想要命中率。
// 故加 route_sticky_cache_first 全局开关（默认 false=上面的分级原样不动）：
//   - false（默认，缓存优先关）：行为与开关引入前逐位一致。
//   - true（缓存优先开）：轮询等模式下，**非空**的纯优化型会话来源也保留 sticky，同会话钉住同一
//     渠道换 cache 命中；**空**来源（拿不到任何会话键，粘性桶会被无关请求共用）依旧不 sticky。
//
// 开关只上移「哪些请求算一个会话」的门槛，FillFirst 与正确性关键来源两条路径完全不受影响。
// 只影响选路，不改任何出站字节/头/指纹（shape 零回归）。
func routeStickyEnabled(mode dbmodel.GroupMode, sessionSource string) bool {
	if mode == dbmodel.GroupModeFillFirst {
		return true
	}
	if isCorrectnessCriticalSessionSource(sessionSource) {
		return true
	}
	// 走到这里=纯优化型来源。空来源没有会话可言，任何档位都不粘（也省掉一次设置读）。
	if sessionSource == "" {
		return false
	}
	return routeStickyCacheFirstEnabled()
}

// routeStickyCacheFirstEnabled 读「缓存优先」全局开关。settingBool 底下是 op 的设置内存缓存
// （非 DB 往返），与本包其它热路径读设置的写法一致，每请求一次内存读可承受；读不到（缓存未初始化
// / 值非法）一律回落 false=保持现行分摊优先。
func routeStickyCacheFirstEnabled() bool {
	return settingBool(dbmodel.SettingKeyRouteStickyCacheFirst, false)
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
