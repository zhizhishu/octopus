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
	// AnthropicScore 是按参考项目权重表累计的分数。
	// 0 = 这条日志没有提供任何来源方向的证据（大多数情况）。
	//
	// 🔴 这里只有 Anthropic 一源，不是三源。参考项目（cc-proxy-detector）能分
	// Bedrock / Vertex，靠的是**上游响应头**（x-amzn-*, x-goog-*）和原始响应体
	// 字段风格；我们的日志只存归一化后的信封字段（服务档位/模型名/指纹），
	// 响应头根本没有落库，物理上分不出是哪家云。曾经为了"对齐参考项目"留了
	// BedrockScore/VertexScore 两个字段，但没有任何代码给它们赋过值，
	// 结果界面永远显示 "Bedrock 0 / Vertex 0" —— 那是假信息，比不显示更糟。
	// 要真做多源，前提是先把响应头存进日志，而不是先摆两个空字段。
	AnthropicScore int `json:"anthropic_score,omitempty"`
	// CounterEvidence 记录命中的反证项（缺失了本该有的字段）。
	// 非空说明这条日志"看着像 Anthropic 协议、但缺了官方必带的字段"。
	CounterEvidence []string `json:"counter_evidence,omitempty"`
	// Signals 记录命中的正向信号，便于排查"为什么给了这个分"。
	Signals []string `json:"signals,omitempty"`
}

// Empty 表示这条日志没提供任何来源证据。
func (e RelayLogPassiveSource) Empty() bool {
	return e.AnthropicScore == 0 &&
		len(e.CounterEvidence) == 0 && len(e.Signals) == 0
}
