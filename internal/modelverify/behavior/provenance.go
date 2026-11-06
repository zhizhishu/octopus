package behavior

import (
	"regexp"
	"sort"
	"strings"
)

// 共用来源规则表：只看「收到上游回复之后才拿得到」的标识位，把它们翻成一条条
// 「这条回复往哪家云 / 哪个协议偏」的线索。
//
// 为什么放在 behavior 而不是留在 op：同一份规则现在有**两个**消费者——
//   - 被动侧（internal/op/log_passive_source.go）：吃已落库的信封字段，零额外请求；
//   - 主动侧（本包的 Run）：吃探针请求顺手抄下的信封，跑完汇总成 ProvenanceSummary。
//
// 两边各写一份的后果是漂移：同一个 `msg_bdrk_` 前缀，日志里报 Bedrock、检测页不报，
// 用户看到的两个结论互相矛盾，而它们本该是同一件事。规则本身、规则顺序、
// Evidence 的截断口径与出处注释全部逐条照搬 op 侧既有实现，不在这里重新标定。
//
// 三条自律（与 model.RelayLogPassiveSource 一致）：只证伪不认证、反证优先、单条不够。

// ProvenanceInput 是一次回复留下的「号码形态」标识位。
//
// 全部是收到回复之后才读到的东西，一条都不改出站 / 下行字节。
type ProvenanceInput struct {
	// UpstreamModel 是上游自报的模型名（首个非空）。空表示上游没自报。
	UpstreamModel string
	// RequestModel 是请求侧模型名，只用于判断「头家族与请求家族是否跨厂商」。
	RequestModel string
	// ResponseID 是上游回复顶层 id 的原值。只在 Anthropic 出站路径上是上游原文，
	// 其余适配器可能自己重写编号，那条路径上不会传进来。
	ResponseID string
	// FirstToolCallID 是第一个非空 tool call id 的原值（采集条件同 ResponseID）。
	FirstToolCallID string
	// SignatureClaudeHash 表示 thinking 签名原文以 "claude#" 开头（Vertex 侧形态）。
	// 只判断形态，签名原文一概不留。
	SignatureClaudeHash bool
	// HeaderFamilies 是上游响应头命中的基础设施家族类别（只记类别不记原值，
	// 见 model.ClassifyUpstreamHeaderFamilies）。
	HeaderFamilies []string
	// ModelSwitchedTo 是**同一条流**里第二个不同的自报模型名。
	ModelSwitchedTo string
}

// ProvenanceHint 是一条「指向某家云 / 某个协议」的线索。
//
// 只放类别和前缀：Evidence 里绝不能出现响应头原值或完整编号（那些能反查到
// 上游主机 / 账号）。聚合层按 Code 归类，Platform / Strength 只是给人看的粗标签。
type ProvenanceHint struct {
	// Platform 取值：aws_kiro / aws_bedrock / aws / gcp_vertex / openai /
	// anthropic / 空串（空串 = 这条线索只说明「回复内部自相矛盾」，不指向任何一家）。
	Platform string `json:"platform"`
	// Strength 取值：strong / medium。
	Strength string `json:"strength"`
	// Code 是线索的稳定标识（聚合 / 前端按它归类，改它等于改数据契约）。
	Code string `json:"code"`
	// Evidence 只放前缀或模型名，最多 64 字符。
	Evidence string `json:"evidence,omitempty"`
}

// 线索取值域（与 model.RelayLogSourceHint 的字段取值逐字相同，改字等于改数据契约）。
const (
	ProvenancePlatformAWSKiro    = "aws_kiro"
	ProvenancePlatformAWSBedrock = "aws_bedrock"
	ProvenancePlatformAWS        = "aws"
	ProvenancePlatformGCPVertex  = "gcp_vertex"
	ProvenancePlatformOpenAI     = "openai"
	ProvenancePlatformAnthropic  = "anthropic"

	ProvenanceStrengthStrong = "strong"
	ProvenanceStrengthMedium = "medium"
)

// provenanceHintEvidenceLimit 是 ProvenanceHint.Evidence 的字符上限。
// 只放前缀或模型名，超长本身就是异常，截断只为保护落库 / 下发体积（不加标记）。
const provenanceHintEvidenceLimit = 64

// provenanceHintModelPartLimit 是「流内换模型」那条线索里每个模型名的上限。
const provenanceHintModelPartLimit = 30

// provenanceVertexToolIDPattern 匹配 Vertex 侧的 tool_use id 形态（tool_<数字>）。
// 出处：cc-proxy-detector references/fingerprint-matrix.md:14-34。
var provenanceVertexToolIDPattern = regexp.MustCompile(`^tool_[0-9]+$`)

// ProvenanceHints 从一次回复的信封字段算出逐条来源线索。
//
// 纯函数：只读入参，无副作用，不发请求、不碰数据库。
//
// 规则表（evidence 列就是写进 ProvenanceHint.Evidence 的内容）：
//
//  1. 模型名以 kiro- 开头                              → aws_kiro     / strong / model_prefix_kiro
//  2. 模型名以 anthropic. 开头或含 .anthropic.claude    → aws_bedrock  / medium / model_prefix_bedrock
//  3. 响应 id 以 req_vrtx_ 开头                         → gcp_vertex   / strong / response_id_vertex
//     3b. 响应 id 以 msg_bdrk_ 开头                        → aws_bedrock  / strong / response_id_bedrock
//  4. 首个 tool call id 完全匹配 ^tool_[0-9]+$           → gcp_vertex   / strong / tool_id_vertex
//  5. 首个 tool call id 以 tooluse_ 开头                → aws          / medium / tool_id_bedrock
//     5b. 首个 tool call id 以 toolu_bdrk_ 开头            → aws_bedrock  / strong / tool_id_bedrock_messages
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
//
// 「失败响应不加线索」不在这里判：失败与否是调用方手上那条日志 / 那次请求的状态，
// 不属于信封本身。被动侧的门在 op，主动侧只对成功的 Ask 记信封。
func ProvenanceHints(in ProvenanceInput) []ProvenanceHint {
	upstreamModel := strings.TrimSpace(in.UpstreamModel)
	lowerModel := strings.ToLower(upstreamModel)
	// 请求侧家族只看名字，不需要任何上游数据；识别不出时是空切片（保持沉默）。
	requestFamilies := InferFamilies(in.RequestModel)

	var hints []ProvenanceHint
	add := func(platform, strength, code, evidence string) {
		hints = append(hints, ProvenanceHint{
			Platform: platform,
			Strength: strength,
			Code:     code,
			Evidence: provenanceEvidence(evidence),
		})
	}

	// 规则 1-2：模型名前缀（同一份模型名字段上互斥）。
	if strings.HasPrefix(lowerModel, "kiro-") {
		add(ProvenancePlatformAWSKiro, ProvenanceStrengthStrong, "model_prefix_kiro", upstreamModel)
	} else if strings.HasPrefix(lowerModel, "anthropic.") || strings.Contains(lowerModel, ".anthropic.claude") {
		add(ProvenancePlatformAWSBedrock, ProvenanceStrengthMedium, "model_prefix_bedrock", upstreamModel)
	}

	// 规则 3：Vertex 原生响应 id 前缀；规则 3b：Bedrock 的 Anthropic 消息格式响应 id `msg_bdrk_`。
	responseID := strings.TrimSpace(in.ResponseID)
	if strings.HasPrefix(responseID, "req_vrtx_") {
		add(ProvenancePlatformGCPVertex, ProvenanceStrengthStrong, "response_id_vertex", "req_vrtx_")
	} else if strings.HasPrefix(responseID, "msg_bdrk_") {
		add(ProvenancePlatformAWSBedrock, ProvenanceStrengthStrong, "response_id_bedrock", "msg_bdrk_")
	}

	// 规则 4-5b：tool call id 形态（同一份 id 上互斥）。
	// `toolu_bdrk_`：Bedrock 走 Anthropic 消息格式时的工具编号。参考项目矩阵只列了 Converse 格式的
	// `tooluse_`；这一条是 2026-11-06 生产实测补的（一个 Claude 渠道 3 小时 195/195 个工具编号都是它）。
	// 中转要伪造它得主动把 `bdrk` 写进去，所以按强证据算；反过来没有它不说明不是 Bedrock。
	toolID := strings.TrimSpace(in.FirstToolCallID)
	switch {
	case provenanceVertexToolIDPattern.MatchString(toolID):
		add(ProvenancePlatformGCPVertex, ProvenanceStrengthStrong, "tool_id_vertex", "tool_<n>")
	case strings.HasPrefix(toolID, "toolu_bdrk_"):
		add(ProvenancePlatformAWSBedrock, ProvenanceStrengthStrong, "tool_id_bedrock_messages", "toolu_bdrk_")
	case strings.HasPrefix(toolID, "tooluse_"):
		add(ProvenancePlatformAWS, ProvenanceStrengthMedium, "tool_id_bedrock", "tooluse_")
	}

	// 规则 6：thinking 签名形态（只记形态，签名原文不落库）。
	if in.SignatureClaudeHash {
		add(ProvenancePlatformGCPVertex, ProvenanceStrengthStrong, "signature_vertex", "claude#")
	}

	// 规则 7：任意 AWS 家族响应头（x-amzn-* / x-amz-*）。
	// 只说明"这条回复经过了某个 AWS 前置层"，既不证明是 Bedrock，也不证明不是中转。
	if provenanceListHas(in.HeaderFamilies, "aws") {
		add(ProvenancePlatformAWS, ProvenanceStrengthMedium, "header_aws", "x-amz*")
	}

	// 规则 8-9：头家族与请求模型家族**跨厂商**。请求侧家族识别不出时不提
	// （缺一边不是信号，与 behavior 包的既有口径一致），否则等于把一条
	// "猜不到"的请求当成"换过家"。右边那个 "openai"/"anthropic" 是 behavior
	// 包的家族名（InferFamilies 的返回值），不是 Platform 取值。
	if provenanceListHas(in.HeaderFamilies, "openai") &&
		len(requestFamilies) > 0 && !provenanceListHas(requestFamilies, "openai") {
		add(ProvenancePlatformOpenAI, ProvenanceStrengthMedium, "header_openai_cross_vendor", "openai-*")
	}
	if provenanceListHas(in.HeaderFamilies, "anthropic_ratelimit") &&
		len(requestFamilies) > 0 && !provenanceListHas(requestFamilies, "anthropic") {
		add(ProvenancePlatformAnthropic, ProvenanceStrengthMedium, "header_anthropic_cross_vendor", "anthropic-ratelimit-*")
	}

	// 规则 10：同一条流里换过自报模型名。首个名字在 UpstreamModel 里（首个非空），
	// 第二个在 ModelSwitchedTo 里。
	// platform 留空：这条线索说的是"回复内部自相矛盾"，不指向任何一家。
	if switched := strings.TrimSpace(in.ModelSwitchedTo); switched != "" {
		evidence := provenanceRunes(switched, provenanceHintModelPartLimit)
		if upstreamModel != "" {
			evidence = provenanceRunes(upstreamModel, provenanceHintModelPartLimit) +
				" → " + provenanceRunes(switched, provenanceHintModelPartLimit)
		}
		add("", ProvenanceStrengthMedium, "model_switch_mid_stream", evidence)
	}

	return hints
}

// ProvenanceSummary 是一次检测里所有成功请求的信封汇总。
//
// 与单条 ProvenanceHint 的区别：这里已经按 Code 去过重、头家族取过并集，
// 所以前端可以直接展示，不必自己再合并。
type ProvenanceSummary struct {
	// Hints 是按 Code 去重后的线索，保持首次出现的顺序（规则表顺序，稳定可对比）。
	Hints []ProvenanceHint `json:"hints,omitempty"`
	// HeaderFamilies 是全部观测到的响应头家族类别的并集（字典序）。
	HeaderFamilies []string `json:"header_families,omitempty"`
	// Observed 是记到信封的成功请求数。0 表示这次一条信封都没有——
	// 「没看到线索」和「根本没看到回复」必须能分开，否则挂掉的渠道会显示成清白。
	Observed int `json:"observed"`
}

// summarizeProvenance 把逐条信封汇总成一份结论。
//
// 去重口径：按 Code 去重（同一条线索在三次请求里各命中一次，只报一次），
// 保留首次出现的顺序而不是按分数排序——规则表顺序本身就有含义
// （强证据在前），排序会把同一条线索的两次观测搅乱。
func summarizeProvenance(envelopes []ProvenanceInput) ProvenanceSummary {
	summary := ProvenanceSummary{Observed: len(envelopes)}
	if len(envelopes) == 0 {
		return summary
	}
	seenCode := make(map[string]struct{}, len(envelopes))
	families := make([]string, 0, 5)
	seenFamily := make(map[string]struct{}, 5)
	for _, in := range envelopes {
		for _, family := range in.HeaderFamilies {
			name := strings.ToLower(strings.TrimSpace(family))
			if name == "" {
				continue
			}
			if _, ok := seenFamily[name]; ok {
				continue
			}
			seenFamily[name] = struct{}{}
			families = append(families, name)
		}
		for _, hint := range ProvenanceHints(in) {
			if _, ok := seenCode[hint.Code]; ok {
				continue
			}
			seenCode[hint.Code] = struct{}{}
			summary.Hints = append(summary.Hints, hint)
		}
	}
	if len(families) > 0 {
		sort.Strings(families)
		summary.HeaderFamilies = families
	}
	return summary
}

// topHint 取一条最该拿去展示的线索：优先 strong，其次 medium。
//
// 为什么按强度挑而不是按出现顺序：汇总顺序是规则表顺序，而展示只有一行——
// 有 `msg_bdrk_` 这种能直接说出平台的强证据时，展示一条「头里有 x-amz*」是浪费。
func (s ProvenanceSummary) topHint() (ProvenanceHint, bool) {
	for _, want := range []string{ProvenanceStrengthStrong, ProvenanceStrengthMedium} {
		for _, hint := range s.Hints {
			if hint.Strength == want {
				return hint, true
			}
		}
	}
	return ProvenanceHint{}, false
}

// ProvenancePlatformText 把 Platform 取值翻成给人看的中文名。
//
// 空平台不是"看不出"，而是「回复内部自相矛盾（中途换了模型）」，所以它有话可说。
func ProvenancePlatformText(platform string) string {
	switch platform {
	case ProvenancePlatformAWSKiro:
		return "Kiro（AWS）"
	case ProvenancePlatformAWSBedrock:
		return "AWS Bedrock"
	case ProvenancePlatformAWS:
		return "AWS 系"
	case ProvenancePlatformGCPVertex:
		return "Google Vertex"
	case ProvenancePlatformOpenAI:
		return "回复带 OpenAI 痕迹"
	case ProvenancePlatformAnthropic:
		return "回复带 Anthropic 痕迹"
	default:
		return "中途换了模型"
	}
}

// provenanceListHas 报告一个字符串集合里有没有精确等于 want 的项（大小写不敏感）。
func provenanceListHas(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// provenanceEvidence 把 evidence 收敛成「前缀/模型名 + 最多 64 字符」。
func provenanceEvidence(evidence string) string {
	return provenanceRunes(evidence, provenanceHintEvidenceLimit)
}

// provenanceRunes 按 rune 截断，不追加任何标记（截断只保护下发体积，
// 不能改变"这是同一个值"的语义）。
func provenanceRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
