package model

// 日志被动探针（passive probe）的结论模型。
//
// 与 PassiveSummary 的分工：
//   - PassiveSummary 是"这条回复长什么样"（字符/行/代码块），回答"文风画像"；
//   - 本文件的 RelayProbeFinding 是"这条日志里有没有对不上的硬事实"，
//     回答"账算不平 / 后端不是它自称的那家"。
//
// 两者的共同底线：**只证伪，不认证**。所有结论都只是线索，没有任何一个字段可以
// 被读成"已确认是正品"。这一点在类型层面就写死——没有 verified/passed 这样的枚举。

// RelayProbeFinding 是一条被动探针结论。
type RelayProbeFinding struct {
	// Code 是稳定的机器码，前端靠它做分组与文案，不靠 Title 文本匹配。
	Code string `json:"code"`
	// Severity 与既有日志异常一致：medium（值得看）/ low（只是线索）。
	Severity string `json:"severity"`
	// Title 是给人看的一句话，中文，不用术语。
	Title string `json:"title"`
	// Evidence 是判定依据的原始数字/字符串，便于用户自己复核而不必信结论。
	Evidence map[string]any `json:"evidence,omitempty"`
}

// 探针码。命名规则：<被检查的东西>_<怎么不对>。
const (
	// ProbeCodeTokenArithmetic 表示 total != prompt + completion。
	//
	// 这是最便宜的硬账：三个数字都在同一条日志里，加减法即可。中转常见的手法是
	// 抬高 total 或者漏加明细来多收钱，这条一算就露。
	ProbeCodeTokenArithmetic = "token_arithmetic"
	// ProbeCodeCacheExceedsPrompt 表示缓存命中数大于输入总数——物理上不可能。
	ProbeCodeCacheExceedsPrompt = "cache_exceeds_prompt"
	// ProbeCodeCharTokenRatio 表示按字数与输出 token 数反推的比值落在人类语言
	// 不可能出现的区间。正常文本约 2–5 字符/token；远低于下限说明 token 被灌水，
	// 远高于上限说明输出被截断但计数没减。
	ProbeCodeCharTokenRatio = "char_token_ratio"
	// ProbeCodeTokenizerResidual 表示用本地分词器算出的输入 token 与上游自报的
	// 输入 token 差别大到不可能是同一个分词器。
	//
	// 这是本次新增里最有分量的一条：真 Claude 与 OpenAI 系分词器差异极大，
	// 一个声称是 Claude 的后端如果按 OpenAI 口径计数，残差会明显偏离。
	ProbeCodeTokenizerResidual = "tokenizer_residual"
	// ProbeCodeEmptyOutputWithTokens 表示回复正文为空但计了输出 token。
	ProbeCodeEmptyOutputWithTokens = "empty_output_with_tokens"
	// ProbeCodeModelEchoSpurious 表示上游回显的模型名与请求的模型名同族不同款
	// （如请求 opus、回显 sonnet）——比"完全不符"更隐蔽，单独出一码。
	ProbeCodeModelEchoSpurious = "model_echo_spurious"
)

// Severity 取值。
const (
	RelayProbeSeverityMedium = "medium"
	RelayProbeSeverityLow    = "low"
)

// 阈值集中在这里，改动必须连带改测试。
const (
	// ProbeCharTokenMin / Max 是人类可读文本每 token 字符数的合理区间。
	//
	// 下界 1.2 与 2.0 之间取 1.2：中文与代码会显著压低这个比值（一个 token 可能
	// 只对应一个汉字），取太松会大面积误报。上界 10 是参考实现给出的经验值。
	ProbeCharTokenMin = 1.2
	ProbeCharTokenMax = 10.0

	// ProbeTokenizerResidualTolerance 是本地分词器与上游自报输入 token 的可容忍
	// 相对差（比例）。
	//
	// 给 0.35 而不是更严：不同实现的分词细节（特殊 token、空白归一、系统提示词
	// 注入、工具声明序列化方式）都会带来系统性偏差，卡太死会把正常的跨协议转换
	// 全部报成异常。超过 35% 的偏差已经不是"实现细节"，而是"根本不是同一个分词器"。
	ProbeTokenizerResidualTolerance = 0.35

	// ProbeTokenizerMinTokens 是参与残差判定的最小输入 token 数。
	//
	// 小样本上一点计数差异就会放大成很大的比例，低于这个量级不下结论。
	ProbeTokenizerMinTokens = 200
)
