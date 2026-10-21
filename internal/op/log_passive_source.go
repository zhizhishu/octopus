package op

import (
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
)

// 纯被动后端判别：只看已经落库的日志字段，零额外请求、零 token。
//
// 设计来源：cc-proxy-detector 的三源权重表（官方 Anthropic / AWS Bedrock /
// Google Vertex 反代），但**只搬其中能靠现有流量判断的那几条**，主动探针
// （thinking 签名、ratelimit 动态、tokenizer 差分）不在本文件范围。
//
// 参考项目原文的权重表是给"主动探测拿到的完整响应"设计的，直接照搬会误判：
// 我们的输入是 octopus 归一化后的内部格式，很多原始字段（响应头、原始 id、
// usage 的字段命名风格）已经被抹平。所以这里只保留**归一化之后仍然存活**的信号。
//
// 三条铁律（都来自参考项目的实测教训）：
//  1. 只证伪不认证。任何"看起来像官方"的组合都不能证明是真官方。
//  2. 反证优先于正分。cc-proxy-detector 自己留了反面案例：某站只给了三个
//     已被攻破的正向信号，却判出 100% Anthropic，因为反证没参与。
//  3. 单条日志不够。这里输出的是"这条日志留下的证据"，聚合多行才有意义。

// passiveSourceWeights 三源累计分的权重。
// 数字直接取自 cc-proxy-detector 的权重表，但只保留我们能观测到的项。
const (
	// cache_creation 是嵌套对象（含 ephemeral_5m/1h 子字段）→ Anthropic 原生风格。
	// 很多中转把它拍平成单个数字，那就失去这条证据。
	weightCacheCreationNested = 1
	// service_tier 字段存在 → Anthropic 官方响应会带。
	weightServiceTier = 3
	// 缓存写入的 5m/1h TTL 拆分：这是 Anthropic 独有的计费维度，
	// 能拆出来说明响应确实来自会区分 TTL 的后端。
	weightCacheTTLSplit = 2
)

// passiveSourcePenalties 缺失字段反证。
//
// 🔴 这是整套逻辑里最重要的部分。参考项目的教训：只做正向加分，
// 中转站只要凑齐几个容易伪造的字段就能拿满分。反证要求"该有的没有"。
//
// 但反证有个前提：必须先确认这条日志**本来应该**有这些字段
// （即已经确定是 Anthropic 协议路径），否则对 OpenAI/Gemini 流量会全线误伤。
const (
	penaltyMissingServiceTier = 3
	penaltyMissingCacheObject = 2
)

// PassiveSourceEvidence 是一条日志留下的后端来源证据。
//
// 刻意不含"结论"字段（不是 official/bedrock/vertex 的判定）：
// 单条日志的字段组合不足以定源，硬给结论就是假精确。
// 聚合层拿 Scores 做累计，才可能形成有意义的倾向。
type PassiveSourceEvidence struct {
	// AnthropicScore / BedrockScore / VertexScore 是按参考项目权重表累计的分数。
	// 全 0 = 这条日志没有提供任何来源方向的证据（大多数情况）。
	AnthropicScore int `json:"anthropic_score,omitempty"`
	BedrockScore   int `json:"bedrock_score,omitempty"`
	VertexScore    int `json:"vertex_score,omitempty"`
	// CounterEvidence 记录命中的反证项（缺失了本该有的字段）。
	// 非空说明这条日志"看着像 Anthropic 协议、但缺了官方必带的字段"。
	CounterEvidence []string `json:"counter_evidence,omitempty"`
	// Signals 记录命中的正向信号，便于排查"为什么给了这个分"。
	Signals []string `json:"signals,omitempty"`
}

// Empty 表示这条日志没提供任何来源证据。
func (e PassiveSourceEvidence) Empty() bool {
	return e.AnthropicScore == 0 && e.BedrockScore == 0 && e.VertexScore == 0 &&
		len(e.CounterEvidence) == 0 && len(e.Signals) == 0
}

// computePassiveSourceEvidence 从一条已落库日志推导来源证据。
//
// 纯函数：只读入参，无副作用，不碰数据库、不发请求。
func computePassiveSourceEvidence(relayLog model.RelayLog) PassiveSourceEvidence {
	var ev PassiveSourceEvidence

	// 只在 Anthropic 协议路径上做判断。
	//
	// 为什么必须限协议：cache_creation 嵌套对象、service_tier 这些是 Anthropic
	// 响应形状的特征。拿 OpenAI 的日志套这套规则，会把"没有 service_tier"
	// 当成反证，全线误伤。
	isAnthropicPath := strings.EqualFold(strings.TrimSpace(relayLog.RequestEndpoint), "messages") ||
		strings.Contains(strings.ToLower(relayLog.RequestPath), "/v1/messages")
	if !isAnthropicPath {
		return ev
	}

	// 有缓存写入总量，说明上游报了缓存字段。
	hasCacheWrite := relayLog.CacheWriteTokens > 0
	// 5m/1h 拆分存在 = 上游区分 TTL，这是 Anthropic 的独有维度。
	hasTTLSplit := relayLog.CacheWrite5mTokens > 0 || relayLog.CacheWrite1hTokens > 0

	if hasTTLSplit {
		ev.AnthropicScore += weightCacheCreationNested + weightCacheTTLSplit
		ev.Signals = append(ev.Signals, "cache_ttl_split")
	}
	if tier := strings.TrimSpace(relayLog.UpstreamServiceTier); tier != "" {
		ev.AnthropicScore += weightServiceTier
		ev.Signals = append(ev.Signals, "service_tier="+tier)
	}

	// 反证：只在"确实走了 Anthropic 路径且这次请求命中过缓存"时才要求
	// 官方必带字段。没命中缓存时上游本来就可能不报 cache_creation，
	// 对空响应提反证等于凭空扣分。
	//
	// 门控条件刻意保守：只有当这条日志已经有正向 Anthropic 信号时才反证。
	// 否则一条"没有任何字段"的日志会被判成"可疑"，那是纯噪音。
	if ev.AnthropicScore > 0 {
		if hasCacheWrite && !hasTTLSplit {
			ev.CounterEvidence = append(ev.CounterEvidence, "cache_creation_not_nested")
			ev.AnthropicScore -= penaltyMissingCacheObject
		}
		if strings.TrimSpace(relayLog.UpstreamServiceTier) == "" {
			ev.CounterEvidence = append(ev.CounterEvidence, "service_tier_absent")
			ev.AnthropicScore -= penaltyMissingServiceTier
		}
	}

	// 分数不应为负：反证的作用是抵消正分，不是累计惩罚。
	// 负分会让聚合层无法区分"证据弱"和"强烈反证"。
	if ev.AnthropicScore < 0 {
		ev.AnthropicScore = 0
	}
	return ev
}
