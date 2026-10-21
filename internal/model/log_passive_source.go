package model

// RelayLogPassiveSource 是一条日志留下的后端来源证据。
//
// 设计来源：cc-proxy-detector 的三源权重表（官方 Anthropic / AWS Bedrock /
// Google Vertex 反代），只搬其中能靠现有流量判断的那几条——主动探针
// （thinking 签名、ratelimit 动态、tokenizer 差分）不在被动层范围。
//
// 三条铁律（都来自参考项目的实测教训）：
//  1. 只证伪不认证。任何"看起来像官方"的组合都不能证明是真官方。
//  2. 反证优先于正分。参考项目自己留了反面案例：某站只给了三个已被攻破的
//     正向信号，却判出 100% Anthropic，因为反证没参与。
//  3. 单条日志不够。这里输出的是"这条日志留下的证据"，聚合多行才有意义。
//
// 刻意不含"结论"字段（不是 official/bedrock/vertex 的判定）：
// 单条日志的字段组合不足以定源，硬给结论就是假精确。
type RelayLogPassiveSource struct {
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
func (e RelayLogPassiveSource) Empty() bool {
	return e.AnthropicScore == 0 && e.BedrockScore == 0 && e.VertexScore == 0 &&
		len(e.CounterEvidence) == 0 && len(e.Signals) == 0
}
