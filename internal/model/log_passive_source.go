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
	// 🔴 这个分数只有 Anthropic 一源。参考项目（cc-proxy-detector）能分
	// Bedrock / Vertex，靠的是**上游响应头**（x-amzn-*, x-goog-*）和原始响应体
	// 字段风格；我们的日志只存归一化后的信封字段（服务档位/模型名/指纹）。
	// 曾经为了"对齐参考项目"留了 BedrockScore/VertexScore 两个字段，但没有任何
	// 代码给它们赋过值，结果界面永远显示 "Bedrock 0 / Vertex 0" —— 那是假信息，
	// 比不显示更糟。
	//
	// 现在补上了两条**不落库**的原料：响应头类别（HeaderFamilies，来自
	// RelayLog.UpstreamEnvelope）和编号/模型名前缀（Hints 规则表）。它们仍然
	// 只证伪不认证：命中 aws 家族头只说明"这条回复经过了某个 AWS 前置层"，
	// 既不证明是 Bedrock，也不证明不是中转；反过来没命中也不能说明任何事。
	AnthropicScore int `json:"anthropic_score,omitempty"`
	// CounterEvidence 记录命中的反证项（缺失了本该有的字段）。
	// 非空说明这条日志"看着像 Anthropic 协议、但缺了官方必带的字段"。
	CounterEvidence []string `json:"counter_evidence,omitempty"`
	// Signals 记录命中的正向信号，便于排查"为什么给了这个分"。
	Signals []string `json:"signals,omitempty"`
	// Hints 是逐条"这条回复往哪家云/哪个协议偏"的线索（规则表见
	// op/log_passive_source.go）。空表示没命中任何一条。
	Hints []RelayLogSourceHint `json:"hints,omitempty"`
	// HeaderFamilies 是上游响应头命中的基础设施家族类别（aws/envoy/openai/…，
	// 见 ClassifyUpstreamHeaderFamilies）。
	//
	// 只有这条日志**至少命中一条 Hint** 时才写：几乎每条 HTTP 回复都可能带一个
	// CDN 头（cf-ray），无条件写就等于给全表加一列噪音、把库撑大。
	HeaderFamilies []string `json:"header_families,omitempty"`
}

// RelayLogSourceHint 是一条"指向某家云 / 某个协议"的线索。
//
// 只放类别和前缀：Evidence 里绝不能出现响应头原值或完整编号（那些能反查到
// 上游主机/账号）。聚合层按 Code 归类，Platform/Strength 只是给人看的粗标签。
type RelayLogSourceHint struct {
	// Platform 取值：aws_kiro / aws_bedrock / aws / gcp_vertex / openai /
	// anthropic / 空串（空串 = 这条线索只说明"回复内部有问题"，不指向任何一家）。
	Platform string `json:"platform,omitempty"`
	// Strength 取值：strong / medium / weak。
	Strength string `json:"strength,omitempty"`
	// Code 是线索的稳定标识（聚合/前端按它归类，改它等于改数据契约）。
	Code string `json:"code,omitempty"`
	// Evidence 只放前缀或模型名，最多 64 字符。
	Evidence string `json:"evidence,omitempty"`
}

// Empty 表示这条日志没提供任何来源证据。
func (e RelayLogPassiveSource) Empty() bool {
	return e.AnthropicScore == 0 &&
		len(e.CounterEvidence) == 0 && len(e.Signals) == 0 &&
		len(e.Hints) == 0 && len(e.HeaderFamilies) == 0
}
