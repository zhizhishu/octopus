package op

import (
	"regexp"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
)

// 纯被动后端判别：只看已经拿到的日志字段，零额外请求、零 token。
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
//
// 本文件有两层，输入各不相同：
//   - AnthropicScore/CounterEvidence/Signals：只吃**已落库**的信封字段（service_tier、
//     cache TTL 拆分），只在 Anthropic 协议路径上跑。
//   - Hints/HeaderFamilies：吃收上游回复时**只在内存里抄下**的标识位
//     （model.RelayLog.UpstreamEnvelope：响应头类别、响应 id、首个 tool call id、
//     thinking 签名形态、流内换模型），对所有协议生效。

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

// passiveSourceHints 的取值域（写进 model.RelayLogSourceHint，改字等于改数据契约）。
const (
	passiveSourcePlatformAWSKiro    = "aws_kiro"
	passiveSourcePlatformAWSBedrock = "aws_bedrock"
	passiveSourcePlatformAWS        = "aws"
	passiveSourcePlatformGCPVertex  = "gcp_vertex"
	passiveSourcePlatformOpenAI     = "openai"
	passiveSourcePlatformAnthropic  = "anthropic"

	passiveSourceStrengthStrong = "strong"
	passiveSourceStrengthMedium = "medium"
)

// passiveSourceHintEvidenceLimit 是 model.RelayLogSourceHint.Evidence 的字符上限。
// 只放前缀或模型名，超长本身就是异常，截断只为保护落库体积（不加标记）。
const passiveSourceHintEvidenceLimit = 64

// passiveSourceHintModelPartLimit 是「流内换模型」那条线索里每个模型名的上限。
const passiveSourceHintModelPartLimit = 30

// passiveVertexToolIDPattern 匹配 Vertex 侧的 tool_use id 形态（tool_<数字>）。
// 出处：cc-proxy-detector references/fingerprint-matrix.md:14-34。
var passiveVertexToolIDPattern = regexp.MustCompile(`^tool_[0-9]+$`)

// computePassiveSourceEvidence 从一条日志推导来源证据。
//
// 纯函数：只读入参，无副作用，不碰数据库、不发请求。
func computePassiveSourceEvidence(relayLog model.RelayLog) model.RelayLogPassiveSource {
	var ev model.RelayLogPassiveSource

	// Anthropic 打分块：只在 Anthropic 协议路径上跑。
	//
	// 为什么必须限协议：cache_creation 嵌套对象、service_tier 这些是 Anthropic
	// 响应形状的特征。拿 OpenAI 的日志套这套规则，会把"没有 service_tier"
	// 当成反证，全线误伤。
	//
	// 这里刻意是「跳过这一块」而不是早退：下面的来源线索对所有协议都生效，
	// 一个 else 分支都不该挡住它们。
	if passiveSourceIsAnthropicPath(relayLog) {
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
	}

	// 来源线索：对所有协议生效。
	hints := computePassiveSourceHints(relayLog)
	if len(hints) > 0 {
		ev.Hints = hints
		// 响应头类别**只在至少命中一条线索时**才写。几乎每条 HTTP 回复都可能带
		// 一个 CDN 头（cf-ray），无条件写等于给全表加一列噪音、把库撑大。
		ev.HeaderFamilies = relayLog.UpstreamEnvelope.HeaderFamilies
	}
	return ev
}

// passiveSourceIsAnthropicPath 判断这条日志走的是不是 Anthropic 协议路径。
//
// 口径与改造前一致（RequestEndpoint=="messages" 或路径含 /v1/messages），
// 抽出来只是为了让上面那个 if 读起来是"跳过一块"而不是"提前返回"。
func passiveSourceIsAnthropicPath(relayLog model.RelayLog) bool {
	return strings.EqualFold(strings.TrimSpace(relayLog.RequestEndpoint), "messages") ||
		strings.Contains(strings.ToLower(relayLog.RequestPath), "/v1/messages")
}

// computePassiveSourceHints 从一条日志里算出逐条来源线索。
//
// 输入全部是「收到上游回复之后才拿得到」的东西，一条都不改出站/下行字节：
//   - RelayLog.UpstreamResponseModel：上游自报的模型名（首个非空）；
//   - RelayLog.UpstreamEnvelope：响应头类别、响应 id、首个 tool call id、
//     thinking 签名形态、流内换模型后的第二个名字。
//
// 规则表（evidence 列就是写进 RelayLogSourceHint.Evidence 的内容）：
//
//  1. 模型名以 kiro- 开头                              → aws_kiro     / strong / model_prefix_kiro
//  2. 模型名以 anthropic. 开头或含 .anthropic.claude    → aws_bedrock  / medium / model_prefix_bedrock
//  3. 响应 id 以 req_vrtx_ 开头                         → gcp_vertex   / strong / response_id_vertex
//  4. 首个 tool call id 完全匹配 ^tool_[0-9]+$           → gcp_vertex   / strong / tool_id_vertex
//  5. 首个 tool call id 以 tooluse_ 开头                → aws          / medium / tool_id_bedrock
//  6. thinking 签名以 claude# 开头                      → gcp_vertex   / strong / signature_vertex
//  7. 头家族含 aws                                      → aws          / medium / header_aws
//  8. 头家族含 openai 且请求模型是别的家族              → openai       / medium / header_openai_cross_vendor
//  9. 头家族含 anthropic_ratelimit 且请求模型不是 anthropic → anthropic / medium / header_anthropic_cross_vendor
//  10. 同一条流里换过模型名                             → (空)         / medium / model_switch_mid_stream
//
// 出处：规则 1-6 来自 cc-proxy-detector references/fingerprint-matrix.md:14-34；
// 规则 10 来自 AI-Infra-Guard 的流完整性检查（项目笔记
// ai-infra-guard-api-checker.md:285）；规则 7-9 来自 modelprint 的头家族表
// （probes/net-headerdna.js:13-42）。
func computePassiveSourceHints(relayLog model.RelayLog) []model.RelayLogSourceHint {
	// 失败日志一条线索都不加：错误响应是网关回的错误页，头和正文反映的是前置层，
	// 不是模型后端。把它当来源证据等于给聚合层灌噪音。
	if passiveSourceLogFailed(relayLog) {
		return nil
	}

	env := relayLog.UpstreamEnvelope
	upstreamModel := strings.TrimSpace(relayLog.UpstreamResponseModel)
	lowerModel := strings.ToLower(upstreamModel)
	// 请求侧家族只看名字，不需要任何上游数据；识别不出时是空切片（保持沉默）。
	requestFamilies := behavior.InferFamilies(relayLog.RequestModelName)

	var hints []model.RelayLogSourceHint
	add := func(platform, strength, code, evidence string) {
		hints = append(hints, model.RelayLogSourceHint{
			Platform: platform,
			Strength: strength,
			Code:     code,
			Evidence: passiveSourceHintEvidence(evidence),
		})
	}

	// 规则 1-2：模型名前缀（同一份模型名字段上互斥）。
	if strings.HasPrefix(lowerModel, "kiro-") {
		add(passiveSourcePlatformAWSKiro, passiveSourceStrengthStrong, "model_prefix_kiro", upstreamModel)
	} else if strings.HasPrefix(lowerModel, "anthropic.") || strings.Contains(lowerModel, ".anthropic.claude") {
		add(passiveSourcePlatformAWSBedrock, passiveSourceStrengthMedium, "model_prefix_bedrock", upstreamModel)
	}

	// 规则 3：Vertex 原生响应 id 前缀。
	if strings.HasPrefix(env.ResponseID, "req_vrtx_") {
		add(passiveSourcePlatformGCPVertex, passiveSourceStrengthStrong, "response_id_vertex", "req_vrtx_")
	}

	// 规则 4-5：tool call id 形态（同一份 id 上互斥）。
	toolID := strings.TrimSpace(env.FirstToolCallID)
	if passiveVertexToolIDPattern.MatchString(toolID) {
		add(passiveSourcePlatformGCPVertex, passiveSourceStrengthStrong, "tool_id_vertex", "tool_<n>")
	} else if strings.HasPrefix(toolID, "tooluse_") {
		add(passiveSourcePlatformAWS, passiveSourceStrengthMedium, "tool_id_bedrock", "tooluse_")
	}

	// 规则 6：thinking 签名形态（只记形态，签名原文不落库）。
	if env.SignatureClaudeHash {
		add(passiveSourcePlatformGCPVertex, passiveSourceStrengthStrong, "signature_vertex", "claude#")
	}

	// 规则 7：任意 AWS 家族响应头（x-amzn-* / x-amz-*）。
	// 只说明"这条回复经过了某个 AWS 前置层"，既不证明是 Bedrock，也不证明不是中转。
	if passiveSourceListHas(env.HeaderFamilies, "aws") {
		add(passiveSourcePlatformAWS, passiveSourceStrengthMedium, "header_aws", "x-amz*")
	}

	// 规则 8-9：头家族与请求模型家族**跨厂商**。请求侧家族识别不出时不提
	// （缺一边不是信号，与 behavior 包的既有口径一致），否则等于把一条
	// "猜不到"的请求当成"换过家"。右边那个 "openai"/"anthropic" 是 behavior
	// 包的家族名（InferFamilies 的返回值），不是 Platform 取值。
	if passiveSourceListHas(env.HeaderFamilies, "openai") &&
		len(requestFamilies) > 0 && !passiveSourceListHas(requestFamilies, "openai") {
		add(passiveSourcePlatformOpenAI, passiveSourceStrengthMedium, "header_openai_cross_vendor", "openai-*")
	}
	if passiveSourceListHas(env.HeaderFamilies, "anthropic_ratelimit") &&
		len(requestFamilies) > 0 && !passiveSourceListHas(requestFamilies, "anthropic") {
		add(passiveSourcePlatformAnthropic, passiveSourceStrengthMedium, "header_anthropic_cross_vendor", "anthropic-ratelimit-*")
	}

	// 规则 10：同一条流里换过自报模型名。首个名字在 UpstreamResponseModel 列里
	// （captureUpstreamDeclaredModel 的首个非空），第二个在 UpstreamEnvelope 里。
	// platform 留空：这条线索说的是"回复内部自相矛盾"，不指向任何一家。
	if switched := strings.TrimSpace(env.ModelSwitchedTo); switched != "" {
		evidence := passiveSourceHintRunes(switched, passiveSourceHintModelPartLimit)
		if upstreamModel != "" {
			evidence = passiveSourceHintRunes(upstreamModel, passiveSourceHintModelPartLimit) +
				" → " + passiveSourceHintRunes(switched, passiveSourceHintModelPartLimit)
		}
		add("", passiveSourceStrengthMedium, "model_switch_mid_stream", evidence)
	}

	return hints
}

// passiveSourceLogFailed 判断这条日志是不是一次失败。
//
// 三种失败标记任一非零即算：错误正文、错误码、>=400 的状态码。失败日志的
// 响应头和正文来自错误页/前置层，不能当后端来源证据。
func passiveSourceLogFailed(relayLog model.RelayLog) bool {
	return strings.TrimSpace(relayLog.Error) != "" ||
		strings.TrimSpace(relayLog.ErrorCode) != "" ||
		relayLog.ErrorStatus >= 400
}

// passiveSourceListHas 报告一个字符串集合里有没有精确等于 want 的项（大小写不敏感）。
func passiveSourceListHas(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// passiveSourceHintEvidence 把 evidence 收敛成「前缀/模型名 + 最多 64 字符」。
func passiveSourceHintEvidence(evidence string) string {
	return passiveSourceHintRunes(evidence, passiveSourceHintEvidenceLimit)
}

// passiveSourceHintRunes 按 rune 截断，不追加任何标记（截断只保护落库体积，
// 不能改变"这是同一个值"的语义）。
func passiveSourceHintRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
