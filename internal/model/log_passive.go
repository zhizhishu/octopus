package model

// RelayLogPassiveSummary 是在 RelayLogAdd 收到的“已脱敏响应副本”上做的一次
// 纯本地、无外部副作用的被动观察摘要。它不复制业务正文，只保留结构化特征；
// 不发上游请求、不参与路由/计费/指纹，也不构成任何“模型真/假”的判定——
// 没有已校准的目标型号基准时一律 insufficient_evidence。
type RelayLogPassiveSummary struct {
	// Version 是特征算法版本，当前固定 v1。聚合分桶按它隔离，算法演进不会
	// 把不同口径的数字混进同一个桶。
	Version string `json:"version"`
	// Status 两态:
	//   not_applicable        — 这条日志不在观察范围(错误/取消/工具输出/媒体/超长等),
	//                           Features 恒为 nil, 不计有效观察;
	//   insufficient_evidence — 完整有效回复且有特征, 但没有已校准基准, 明确不判真假。
	Status string `json:"status"`
	// Reason 细分原因(error/canceled/tool_output/blank/oversized_text/multiple_choices/…)。
	Reason string `json:"reason"`
	// Language 是按文本里汉字/拉丁字母计数得到的粗标签 zh/en/mixed/other,
	// 明确不是语种认证。not_applicable 时为空。
	Language string `json:"language,omitempty"`
	// Features 只有完整有效回复才有(nil=无特征/不计观察)。指针三态:
	// nil=旧行或未观察; 非 nil=有效观察。
	Features *RelayLogPassiveFeatures `json:"features,omitempty"`
}

// RelayLogPassiveFeatures 是纯本地字符统计, 故意精简:
// chars/lines/paragraphs/code_blocks/code_ratio/markdown_lines 六项, 不扩词库、
// 不做画像、不带任何置信度字段。
type RelayLogPassiveFeatures struct {
	Chars         int     `json:"chars"`
	Lines         int     `json:"lines"`
	Paragraphs    int     `json:"paragraphs"`
	CodeBlocks    int     `json:"code_blocks"`
	CodeRatio     float64 `json:"code_ratio"`
	MarkdownLines int     `json:"markdown_lines"`
	// UpstreamFingerprintPresent 表示这次响应里上游自报了 system_fingerprint。
	//
	// **只记"有没有"，故意不记值、不比较值。**
	// 实测该字段会逐请求乱跳（参考项目里 27 个响应拿到 27 个不同值），
	// 拿它做身份判据必假阳性。它唯一站得住的用法是"上游有没有报这个字段"——
	// 官方会报，很多中转直接丢，这个存在性本身是事实。
	UpstreamFingerprintPresent bool `json:"upstream_fingerprint_present,omitempty"`
	// ServiceTier 是上游自报的服务档位原值（空=没报）。不解读、不折算、不判定
	// "该给什么档"，只如实记录。是否降档由上层聚合比较，摘要层不做判断。
	ServiceTier string `json:"service_tier,omitempty"`
}

// PassiveSummaryVersion 固定特征算法版本。
//
// v1 → v2：Features 增加 upstream_fingerprint_present / service_tier 两项。
// 必须升版本：聚合分桶按它隔离，v1 的桶里没有这两个字段，
// 混进 v2 会把"没报指纹"和"没采这项"算成同一件事，得出相反的结论。
const PassiveSummaryVersion = "v2"

// Status 两态。
const (
	PassiveStatusNotApplicable        = "not_applicable"
	PassiveStatusInsufficientEvidence = "insufficient_evidence"
)

// Reason 常量。not_applicable 侧:
const (
	PassiveReasonError             = "error"              // 请求失败(ErrorStatus/ErrorCode/Error 非空)
	PassiveReasonCanceled          = "canceled"           // 客户端取消, 先于 generic error 判
	PassiveReasonToolOutput        = "tool_output"        // tool_calls/工具调用/拒答/多模态部件
	PassiveReasonUnknownFinish     = "unknown_finish"     // finish_reason 缺失或非 stop, 不当完整
	PassiveReasonMultipleChoices   = "multiple_choices"   // choices != 1
	PassiveReasonNonText           = "non_text"           // 有 message 但内容非纯文本(含部件非 text)
	PassiveReasonBlank             = "blank"              // 候选文本为空或纯空白
	PassiveReasonUnparseable       = "unparseable"        // ResponseContent 不是合法 JSON/形状不对
	PassiveReasonOversizedEnvelope = "oversized_envelope" // 脱敏 JSON 总体 >128KiB
	PassiveReasonOversizedText     = "oversized_text"     // 候选文本 >32KiB, 跳过不截断
	PassiveReasonOutOfRange        = "out_of_range"       // model_test/embedding/媒体等范围外端点
	PassiveReasonOpaqueResponse    = "opaque_response"    // 非结构化/opaque 响应内容
)
