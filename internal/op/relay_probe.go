package op

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
)

// 日志被动探针：在**已有业务日志**上做零上游请求的硬事实校验。
//
// 设计边界（与既有被动观察一致，不放松）：
//   - 不新增任何上游请求，不读整段正文之外的东西，不复制业务正文进结论；
//   - 只产出"对不上的地方"，不产出"这是正品"；没有 verification/passed 枚举；
//   - 账算得平不等于模型真，只是"这一条没有可指出的矛盾"。
//
// 为什么这些检查值得做（参考项目读码结论）：
//   中转最常见的两类问题是"多收钱"和"换后端"。前者几乎总会在 token 三个数字上
//   留下加减法级别的痕迹；后者会在分词器口径上留下跨厂商的系统性偏差。
//   两者都不需要给上游发任何试探请求——用已经流过的流量就能算。

// RelayProbeInput 是计算一条日志被动探针所需的全部输入。
//
// 独立成结构体而不是直接吃 model.RelayLog：日志结构体字段很多且会继续加，
// 显式列出真正用到的东西，能让"这条检测依赖什么"一眼可见，也方便测试构造。
type RelayProbeInput struct {
	// 模型名：请求发出去的（ActualModelName）与上游自报回来的。
	RequestedModel string
	UpstreamModel  string

	// token 三个数字，来自上游自报的 usage。
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// 缓存相关（缺失时为零，视为"上游没报"，不参与判定）。
	CacheHitTokens   int
	CacheWriteTokens int

	// SeparateCacheInput 表示本条的 InputTokens 是**上游独占口径**的输入：
	// 缓存命中/写入不计入 InputTokens，要另外加。Anthropic 就是这种语义
	// （input_tokens=12 + cache_read=40000 + cache_creation=3000 是正常报文）。
	//
	// 为什么要单独记：所有"缓存不能超过输入"的核对都必须拿**折叠后总数**当分母，
	// 否则每一条带缓存的 Anthropic 请求都会必然报假警（12 比 40000 小，但
	// 43012 才是真正的输入总量）。折叠后的总数见 CacheFoldedInputTokens。
	SeparateCacheInput bool

	// CacheFoldedInputTokens 是渠道口径统一的输入总量：独占口径时等于
	// InputTokens + 缓存命中 + 缓存写入；否则等于 InputTokens。
	// 由 relay 侧按 usageCacheStats 的同一套规则算好传进来，探针不再自己推。
	CacheFoldedInputTokens int

	// UsageSource 是这条日志 token 数字的来源（model.RelayLogUsageSource*）。
	// 分词器残差只在 upstream_usage 时有意义：本地估算的数字是我们自己用同一个
	// 分词器数出来的，再拿同一个分词器复算比对，残差只反映"两段文本长度差"，
	// 结论无意义——这种情形整条检查跳过。
	UsageSource string

	// 请求体与回复正文（均已脱敏）。只用于分词器残差与字符/token 比值。
	RequestContent  string
	ResponseContent string

	// 失败的请求不参与（错误回复的 token 数字没有意义）。
	HasError bool
}

// RelayProbeResult 是一条日志的探针结论集合。
type RelayProbeResult struct {
	Findings []model.RelayProbeFinding `json:"findings"`
	// Checked 记录实际执行了哪几条检查，便于界面说明"这条日志查了什么"，
	// 也避免用户以为没报问题=什么都查过了。
	Checked []string `json:"checked"`
}

// ComputeRelayLogProbes 纯函数：对一条日志算被动探针结论。
//
// 任何一条检查算不出来（缺字段、解析失败、样本太小）都**跳过而不是报异常**——
// "算不出来"和"算出来不对"必须分开，否则界面上全是噪声，真问题会淹没在里面。
func ComputeRelayLogProbes(in RelayProbeInput) RelayProbeResult {
	res := RelayProbeResult{Findings: []model.RelayProbeFinding{}}
	if in.HasError {
		// 失败请求的 usage 通常为空或残缺，拿来算账只会制造假警报。
		return res
	}

	res.Findings = append(res.Findings, probeTokenArithmetic(in)...)
	res.Checked = append(res.Checked, model.ProbeCodeTokenArithmetic)

	res.Findings = append(res.Findings, probeCacheExceedsPrompt(in)...)
	res.Checked = append(res.Checked, model.ProbeCodeCacheExceedsPrompt)

	res.Findings = append(res.Findings, probeEmptyOutput(in)...)
	res.Checked = append(res.Checked, model.ProbeCodeEmptyOutputWithTokens)

	if f, ok := probeCharTokenRatio(in); ok {
		res.Findings = append(res.Findings, f)
		res.Checked = append(res.Checked, model.ProbeCodeCharTokenRatio)
	}

	if f, ok := probeTokenizerResidual(in); ok {
		res.Findings = append(res.Findings, f)
		res.Checked = append(res.Checked, model.ProbeCodeTokenizerResidual)
	}

	return res
}

// probeTokenArithmetic 查 total 是否等于 prompt + completion。
//
// 只要三个数字里有两个以上非零就判：单侧为零时这个等式本来就平凡成立，
// 拿去告警纯属噪声。
func probeTokenArithmetic(in RelayProbeInput) []model.RelayProbeFinding {
	if in.InputTokens <= 0 || in.OutputTokens <= 0 || in.TotalTokens <= 0 {
		return nil
	}
	sum := in.InputTokens + in.OutputTokens
	if in.TotalTokens == sum {
		return nil
	}
	diff := in.TotalTokens - sum
	return []model.RelayProbeFinding{{
		Code:     model.ProbeCodeTokenArithmetic,
		Severity: model.RelayProbeSeverityMedium,
		Title: fmt.Sprintf("这条日志的 token 总数对不上账：输入 %d + 输出 %d = %d，但记的总数是 %d（差 %+d）。"+
			"数字自相矛盾通常意味着中间有人重算或多收了钱", in.InputTokens, in.OutputTokens, sum, in.TotalTokens, diff),
		Evidence: map[string]any{
			"input":  in.InputTokens,
			"output": in.OutputTokens,
			"sum":    sum,
			"total":  in.TotalTokens,
			"diff":   diff,
		},
	}}
}

// probeCacheExceedsPrompt 查缓存计数是否超过输入总数。
//
// 缓存命中是输入的一部分，不可能比整个输入还多。超过就是计数口径错了，
// 或者有人把缓存数当额外收入加了进去。
func probeCacheExceedsPrompt(in RelayProbeInput) []model.RelayProbeFinding {
	// 分母必须是**渠道口径统一的输入总量**，不能拿上游独占口径的 InputTokens。
	//
	// Anthropic 的 usage 是独占口径：input_tokens=12 表示"非缓存部分 12 个"，
	// 缓存命中 40000、缓存写入 3000 是另外两栏。真实的输入总量是 43012。
	// 旧代码拿 InputTokens(12) 当分母，于是 cacheTotal(43000) > 12 恒成立 ——
	// 每一条带 prompt cache 的 Anthropic 请求都报"缓存计数超过了总输入"，
	// 而那条请求完全正常。假警报到这个密度就等于没有告警。
	denominator := in.CacheFoldedInputTokens
	if denominator <= 0 {
		// 没传折叠值（或确实为 0）：独占口径下自己折一次，其它口径直接用 InputTokens。
		denominator = in.InputTokens
		if in.SeparateCacheInput {
			denominator += in.CacheHitTokens + in.CacheWriteTokens
		}
	}
	if denominator <= 0 {
		return nil
	}
	cacheTotal := in.CacheHitTokens + in.CacheWriteTokens
	if cacheTotal <= 0 || cacheTotal <= denominator {
		return nil
	}
	return []model.RelayProbeFinding{{
		Code:     model.ProbeCodeCacheExceedsPrompt,
		Severity: model.RelayProbeSeverityMedium,
		Title: fmt.Sprintf("缓存计数超过了总输入：缓存命中 %d + 缓存写入 %d = %d，比输入总数 %d 还多。"+
			"缓存本该是输入的一部分，超过它说明计数口径不对",
			in.CacheHitTokens, in.CacheWriteTokens, cacheTotal, denominator),
		Evidence: map[string]any{
			"cache_hit":    in.CacheHitTokens,
			"cache_write":  in.CacheWriteTokens,
			"cache_total":  cacheTotal,
			"input_tokens": in.InputTokens,
			"input_folded": denominator,
		},
	}}
}

// probeEmptyOutput 查"正文是空的却计了输出 token"。
//
// 反过来也要防：有正文却计 0 输出。这里只报前者——后者可能只是上游没报 usage，
// 报出来大半是噪声。
//
// 判"空"必须看**取出来的正文**，不能看整段 JSON 外壳：`{"choices":[...,
// "content":""]}` 这段外壳永远不空，拿它判空等于这条检查永远不触发。
func probeEmptyOutput(in RelayProbeInput) []model.RelayProbeFinding {
	if in.OutputTokens <= 0 {
		return nil
	}
	if hasAnyVisibleOutput(in.ResponseContent) {
		return nil
	}
	return []model.RelayProbeFinding{{
		Code:     model.ProbeCodeEmptyOutputWithTokens,
		Severity: model.RelayProbeSeverityMedium,
		Title: fmt.Sprintf("回复正文是空的，却记了 %d 个输出 token。"+
			"要么内容在中间被吃掉了，要么这笔钱没有对应的输出", in.OutputTokens),
		Evidence: map[string]any{"output": in.OutputTokens},
	}}
}

// probeCharTokenRatio 用"正文字符数 / 输出 token 数"反推分词是否合理。
//
// 人类可读文本大约每 token 2–5 个字符；低于下界说明 token 被灌水，
// 高于上界说明输出被截断但计数没跟着减。
func probeCharTokenRatio(in RelayProbeInput) (model.RelayProbeFinding, bool) {
	if in.OutputTokens <= 0 {
		return model.RelayProbeFinding{}, false
	}
	// 分子必须与分母同源：output_tokens 实际由「可见正文 + 工具调用参数 + 不可见推理」
	// 三部分组成，只数正文会把一切带工具调用的回复报成"token 灌水"（生产误报实证：
	// 13 字正文 + 一条 bash 命令 = 120 token，比值 0.11 触发假警）。
	text, meta := replyBillableText(in.ResponseContent)
	if strings.TrimSpace(text) == "" {
		return model.RelayProbeFinding{}, false
	}
	chars := len([]rune(text))
	if chars == 0 {
		return model.RelayProbeFinding{}, false
	}
	ratio := float64(chars) / float64(in.OutputTokens)
	if ratio >= model.ProbeCharTokenMin && ratio <= model.ProbeCharTokenMax {
		return model.RelayProbeFinding{}, false
	}
	// 方向感知：不可见推理的质量没有上界，它只会让分母虚高、比值偏低。
	// 所以低位越界（疑似灌水）在推理型请求下不可判——整条跳过；
	// 高位越界（字符远超 token）推理解释不了，照报。
	reasoning := meta.hasReasoning || requestHasReasoningEffort(in.RequestContent)
	if ratio < model.ProbeCharTokenMin && reasoning {
		return model.RelayProbeFinding{}, false
	}
	severity := model.RelayProbeSeverityLow
	// 只有明显越界才升级：擦边值多半是中文/代码这类正常但极端的文本。
	farOut := ratio < 1.0 || ratio > 20
	if farOut {
		severity = model.RelayProbeSeverityMedium
	}
	// 置信度：high 要求数字完全同源（单 choice、无工具参数、无推理、越界幅度
	// 超过阈值两倍）；有已计入的工具参数、多 choice、贴阈值，都是 medium。
	confidence := model.RelayProbeConfidenceHigh
	var factors []string
	if meta.toolArgsCounted {
		factors = append(factors, "tool_arguments_counted")
	}
	if meta.choiceCount > 1 {
		factors = append(factors, "multi_choice")
	}
	if !farOut {
		factors = append(factors, "near_threshold")
	}
	if len(factors) > 0 {
		confidence = model.RelayProbeConfidenceMedium
	}
	evidence := map[string]any{
		"chars":         chars,
		"output_tokens": in.OutputTokens,
		"ratio":         math.Round(ratio*100) / 100,
		"min":           model.ProbeCharTokenMin,
		"max":           model.ProbeCharTokenMax,
		"counted_parts": meta.countedParts,
	}
	if len(factors) > 0 {
		evidence["confidence_factors"] = factors
	}
	return model.RelayProbeFinding{
		Code:       model.ProbeCodeCharTokenRatio,
		Severity:   severity,
		Confidence: confidence,
		Title: fmt.Sprintf("回复可数文本 %d 个字符却记了 %d 个输出 token，相当于每个 token %.2f 个字。"+
			"正常文本大约每个 token 2–5 个字，这个比例明显偏离，可能是 token 被多算或输出被截断了",
			chars, in.OutputTokens, ratio),
		Evidence: evidence,
	}, true
}

// replyBillableMeta 记录"分子数了哪些部分"，供置信度与证据面板使用。
type replyBillableMeta struct {
	hasReasoning    bool
	toolArgsCounted bool
	choiceCount     int
	countedParts    []string
}

// replyBillableText 汇总一次回复里"模型逐字吐出、我们看得见"的全部文本，
// 跨所有 choice 求和（n>1 时 output_tokens 也是全部 choice 之和，只数第一个
// 会让比值必然偏低）。返回文本与元信息。
//
// 数的部分：正文 content、tool_calls[].function.arguments、function_call.arguments、
// 可见的 reasoning_content / reasoning / Anthropic thinking 文本、tool_use.input
// 的叶子字符串。不数的部分：JSON 包装与键名。
func replyBillableText(body string) (string, replyBillableMeta) {
	var meta replyBillableMeta
	if strings.TrimSpace(body) == "" {
		return "", meta
	}
	var shape struct {
		Choices []struct {
			Message *struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				ToolCalls        []struct {
					Function *struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				FunctionCall *struct {
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"message"`
			Content json.RawMessage `json:"content"`
		} `json:"choices"`
		// Anthropic 形状：顶层 content 是 block 数组。
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return "", meta
	}
	var sb strings.Builder
	seen := map[string]bool{}
	mark := func(part string) {
		if !seen[part] {
			seen[part] = true
			meta.countedParts = append(meta.countedParts, part)
		}
	}
	// Anthropic 顶层 blocks。
	for _, block := range shape.Content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				sb.WriteString(block.Text)
				mark("content")
			}
		case "thinking", "redacted_thinking":
			meta.hasReasoning = true
			if strings.TrimSpace(block.Thinking) != "" {
				sb.WriteString(block.Thinking)
				mark("reasoning")
			}
		case "tool_use", "server_tool_use":
			if s := jsonLeafText(block.Input); s != "" {
				sb.WriteString(s)
				meta.toolArgsCounted = true
				mark("tool_arguments")
			}
		}
	}
	for _, ch := range shape.Choices {
		meta.choiceCount++
		if ch.Message != nil {
			if s := rawJSONText(ch.Message.Content); s != "" {
				sb.WriteString(s)
				mark("content")
			}
			if s := rawJSONString(ch.Message.ReasoningContent); strings.TrimSpace(s) != "" {
				sb.WriteString(s)
				meta.hasReasoning = true
				mark("reasoning")
			}
			if s := rawJSONString(ch.Message.Reasoning); strings.TrimSpace(s) != "" {
				sb.WriteString(s)
				meta.hasReasoning = true
				mark("reasoning")
			}
			for _, tc := range ch.Message.ToolCalls {
				if tc.Function != nil && strings.TrimSpace(tc.Function.Arguments) != "" {
					sb.WriteString(tc.Function.Arguments)
					meta.toolArgsCounted = true
					mark("tool_arguments")
				}
			}
			if ch.Message.FunctionCall != nil && strings.TrimSpace(ch.Message.FunctionCall.Arguments) != "" {
				sb.WriteString(ch.Message.FunctionCall.Arguments)
				meta.toolArgsCounted = true
				mark("tool_arguments")
			}
		}
		if s := rawJSONText(ch.Content); s != "" {
			sb.WriteString(s)
			mark("content")
		}
	}
	return sb.String(), meta
}

// requestHasReasoningEffort 报告请求是否声明了推理强度（reasoning_effort /
// reasoning / thinking 任意形状）。低位比值在推理型请求下不可判，见
// probeCharTokenRatio 的方向感知注释。
func requestHasReasoningEffort(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	var shape struct {
		ReasoningEffort json.RawMessage `json:"reasoning_effort"`
		Reasoning       json.RawMessage `json:"reasoning"`
		Thinking        json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return false
	}
	return rawJSONPresent(shape.ReasoningEffort) || rawJSONPresent(shape.Reasoning) || rawJSONPresent(shape.Thinking)
}

// jsonLeafText 递归收集一段 JSON 里全部字符串**值**（键名与包装不数——
// 那才是"模型逐字读到的文本"，键名是结构开销）。用于 tool_use.input 与
// 顶层 tools 的 schema 描述。
func jsonLeafText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var sb strings.Builder
	collectJSONLeafStrings(v, &sb)
	return sb.String()
}

func collectJSONLeafStrings(v any, sb *strings.Builder) {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			sb.WriteString(t)
		}
	case []any:
		for _, item := range t {
			collectJSONLeafStrings(item, sb)
		}
	case map[string]any:
		for _, val := range t {
			collectJSONLeafStrings(val, sb)
		}
	}
}

// probeTokenizerResidual 用本地分词器复算输入 token，与上游自报值比较。
//
// 这是本次新增里最有分量的一条。真实的 Claude 用的是自家分词器，与 OpenAI 的
// o200k 口径差异很大；一个声称 Claude 的中转如果背后是 OpenAI 系模型（或干脆
// 按 OpenAI 口径计费），残差会稳定地偏出合理范围。
//
// 必须小心的两件事：
//  1. CountTokens 出错时**静默返回 0**。所以这里把 0 当作"没算出来"而不是
//     "上游是 0 个 token"，否则会把工具失败当成模型造假。
//  2. 请求体里只取"用户可见的文本"来算，不拿整个 JSON 去算——JSON 的引号、
//     转义、字段名都不该计入模型真正看到的 token。
func probeTokenizerResidual(in RelayProbeInput) (model.RelayProbeFinding, bool) {
	// 同源门槛：token 数字必须是上游自报的。本地估算的数字本来就是我们用同一个
	// 分词器数出来的，再复算比对等于自己核对自己，结论无意义——整条跳过。
	if in.UsageSource != "" && in.UsageSource != model.RelayLogUsageSourceUpstream {
		return model.RelayProbeFinding{}, false
	}
	// 分母必须用**折叠后的输入总量**：Anthropic 独占口径下 InputTokens 只是
	// 非缓存部分（12），缓存命中 40000 才是大头。拿独占值当分母，每一条带
	// prompt cache 的 Anthropic 请求都会报出 +300000% 的假残差。
	upstream := in.CacheFoldedInputTokens
	if upstream <= 0 {
		upstream = in.InputTokens
	}
	if upstream < model.ProbeTokenizerMinTokens {
		return model.RelayProbeFinding{}, false
	}
	text, meta := requestBillableText(in.RequestContent)
	// 含图片/音频/文件部件：这些部件占多少 token 无法从存储正文恢复（日志只留了
	// 占位符），不可计算且无界的量不猜——整条跳过，不用低置信度硬报。
	if meta.hasMedia {
		return model.RelayProbeFinding{}, false
	}
	if strings.TrimSpace(text) == "" {
		return model.RelayProbeFinding{}, false
	}
	local := tokenizer.CountTokens(text, in.RequestedModel)
	// 0 在 CountTokens 里是"算失败"，不是"零个 token"——绝不能当有效测量。
	if local <= 0 {
		return model.RelayProbeFinding{}, false
	}
	diff := float64(local-upstream) / float64(upstream)
	if math.Abs(diff) <= model.ProbeTokenizerResidualTolerance {
		return model.RelayProbeFinding{}, false
	}
	// 置信度：数进去的 tools/tool_calls/重放推理，服务商注入自家模板时的包装开销
	// 不可计算但有界（每条工具调用约 10–20 token）；本地分词器与请求模型不同族
	// 同理（CountTokens 目前恒为 o200k 口径）。两者都是有界干扰 → medium。
	confidence := model.RelayProbeConfidenceHigh
	var factors []string
	if meta.hasTools {
		factors = append(factors, "upstream_templated_tools")
	}
	if meta.toolArgsCounted {
		factors = append(factors, "tool_arguments_counted")
	}
	if meta.reasoningCounted {
		factors = append(factors, "replayed_reasoning")
	}
	if !isO200kFamilyModel(in.RequestedModel) {
		factors = append(factors, "tokenizer_family_mismatch")
	}
	if len(factors) > 0 {
		confidence = model.RelayProbeConfidenceMedium
	}
	evidence := map[string]any{
		"upstream_input_folded": upstream,
		"upstream_input_raw":    in.InputTokens,
		"local_recount":         local,
		"relative_diff":         math.Round(diff*1000) / 1000,
		"tolerance":             model.ProbeTokenizerResidualTolerance,
		"min_tokens":            model.ProbeTokenizerMinTokens,
		"counted_parts":         meta.countedParts,
	}
	if len(factors) > 0 {
		evidence["confidence_factors"] = factors
	}
	return model.RelayProbeFinding{
		Code:       model.ProbeCodeTokenizerResidual,
		Severity:   model.RelayProbeSeverityMedium,
		Confidence: confidence,
		Title: fmt.Sprintf("上游报的输入是 %d 个 token，但用标准分词器复算同样的文字只有 %d 个（差 %+.0f%%）。"+
			"偏差这么大说明背后用的很可能不是这个模型该有的分词器——不同厂商的分词口径差异恰好就在这个量级",
			upstream, local, diff*100),
		Evidence: evidence,
	}, true
}

// isO200kFamilyModel 粗判模型是否 OpenAI o200k 口径家族。本地分词器目前只有
// o200k 一种（见 internal/utils/tokenizer），非同族模型的复算自带系统性偏差，
// 置信度降一档而不是当成铁证。
func isO200kFamilyModel(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"gpt", "o1", "o3", "o4", "codex"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// requestBillableMeta 记录请求侧复算数了哪些部分、遇到了哪些不可计算量。
type requestBillableMeta struct {
	hasMedia         bool
	hasTools         bool
	toolArgsCounted  bool
	reasoningCounted bool
	countedParts     []string
}

// requestBillableText 收集请求里"模型逐字读到"的全部文本：所有 message 的正文、
// system、prompt、assistant 历史里的 tool_calls 参数、重放的 reasoning、
// Anthropic 的 tool_use/tool_result/thinking 块、顶层 tools 的名称/描述/schema
// 叶子字符串。不数 JSON 包装与键名（包装开销有界，远在容差之内）。
//
// 媒体部件（图片/音频/文件）不数也无法数——日志只存了占位符，token 量不可恢复，
// 通过 meta.hasMedia 告知调用方整条跳过。
func requestBillableText(body string) (string, requestBillableMeta) {
	var meta requestBillableMeta
	if strings.TrimSpace(body) == "" {
		return "", meta
	}
	var shape struct {
		Messages []struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			Reasoning        json.RawMessage `json:"reasoning"`
			Images           json.RawMessage `json:"images"`
			Audio            json.RawMessage `json:"audio"`
			ToolCalls        []struct {
				Function *struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall *struct {
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"messages"`
		System json.RawMessage `json:"system"`
		Prompt json.RawMessage `json:"prompt"`
		Tools  json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return "", meta
	}
	var sb strings.Builder
	contentCounted := false
	systemCounted := false
	promptCounted := false
	if s := rawJSONText(shape.System); s != "" {
		sb.WriteString(s)
		systemCounted = true
	}
	for _, msg := range shape.Messages {
		if messageContentBillableText(msg.Content, &meta, &sb) {
			contentCounted = true
		}
		if rawJSONPresent(msg.Images) || rawJSONPresent(msg.Audio) {
			meta.hasMedia = true
		}
		if s := rawJSONString(msg.ReasoningContent); strings.TrimSpace(s) != "" {
			sb.WriteString(s)
			meta.reasoningCounted = true
		}
		if s := rawJSONString(msg.Reasoning); strings.TrimSpace(s) != "" {
			sb.WriteString(s)
			meta.reasoningCounted = true
		}
		for _, tc := range msg.ToolCalls {
			if tc.Function != nil && strings.TrimSpace(tc.Function.Arguments) != "" {
				sb.WriteString(tc.Function.Arguments)
				meta.toolArgsCounted = true
			}
		}
		if msg.FunctionCall != nil && strings.TrimSpace(msg.FunctionCall.Arguments) != "" {
			sb.WriteString(msg.FunctionCall.Arguments)
			meta.toolArgsCounted = true
		}
	}
	if sb.Len() == 0 {
		if s := rawJSONText(shape.Prompt); s != "" {
			sb.WriteString(s)
			promptCounted = true
		}
	}
	if s := jsonLeafText(shape.Tools); s != "" {
		sb.WriteString(s)
		meta.hasTools = true
	}
	// counted_parts 按真实数到的内容标注：content 块里的 tool_use / thinking
	// 也归到各自的类目，不再笼统写成"正文"。
	if systemCounted {
		meta.countedParts = append(meta.countedParts, "system")
	}
	if contentCounted || promptCounted {
		meta.countedParts = append(meta.countedParts, "content")
	}
	if meta.reasoningCounted {
		meta.countedParts = append(meta.countedParts, "reasoning")
	}
	if meta.toolArgsCounted {
		meta.countedParts = append(meta.countedParts, "tool_arguments")
	}
	if meta.hasTools {
		meta.countedParts = append(meta.countedParts, "tools_schema")
	}
	return sb.String(), meta
}

// messageContentBillableText 把一条 message 的 content 数进 sb：纯字符串、
// 文本部件、Anthropic 的 tool_use.input / tool_result / thinking 块。
// 遇到媒体部件置 meta.hasMedia。返回是否数到了内容。
func messageContentBillableText(raw json.RawMessage, meta *requestBillableMeta, sb *strings.Builder) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) != "" {
			sb.WriteString(s)
			return true
		}
		return false
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return false
	}
	counted := false
	for _, p := range parts {
		switch p.Type {
		case "text", "":
			if strings.TrimSpace(p.Text) != "" {
				sb.WriteString(p.Text)
				counted = true
			}
		case "thinking", "redacted_thinking":
			if strings.TrimSpace(p.Thinking) != "" {
				sb.WriteString(p.Thinking)
				meta.reasoningCounted = true
				counted = true
			}
		case "tool_use", "server_tool_use":
			if s := jsonLeafText(p.Input); s != "" {
				sb.WriteString(s)
				meta.toolArgsCounted = true
				counted = true
			}
		case "tool_result":
			// tool_result 的 content 还是一段部件数组，递归数它的文本。
			if messageContentBillableText(p.Content, meta, sb) {
				counted = true
			}
		case "image_url", "image", "input_audio", "audio", "input_image", "file", "video":
			meta.hasMedia = true
		}
	}
	return counted
}

// visibleReplyText 从响应 JSON 里取出助手可见的正文。
//
// 取不出来就返回空字符串，让调用方跳过——**不拿整个 JSON 顶替**，
// 否则字段名和转义会被算成"正文"，比值立刻失真。
func visibleReplyText(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var shape struct {
		Choices []struct {
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			// Anthropic / 原生协议的顶层 content 也兼容一下。
			Content json.RawMessage `json:"content"`
		} `json:"choices"`
		// Anthropic 形状。
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return ""
	}
	if len(shape.Content) > 0 {
		var sb strings.Builder
		for _, c := range shape.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	for _, ch := range shape.Choices {
		if ch.Message != nil {
			if s := rawJSONText(ch.Message.Content); s != "" {
				return s
			}
		}
		if s := rawJSONText(ch.Content); s != "" {
			return s
		}
	}
	return ""
}

// hasAnyVisibleOutput 报告这次回复是否存在**任何**产出。
//
// 🔴 不能用「正文是不是空的」来判空。一轮正常的回复可以是：
//   - 纯工具调用：正文空，全部内容在 tool_calls / tool_use 里；
//   - 纯推理：正文空，内容在 reasoning_content / thinking block 里；
//   - 结构化输出：正文空，内容在 function_call / json 里。
//
// 这三种都花了 output token 而且有真实产出。只看正文会把它们一律判成
// "回复正文是空的，却记了 N 个输出 token" —— 一个会让用户去查上游、
// 实际什么都没错的假警报。
func hasAnyVisibleOutput(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	if strings.TrimSpace(visibleReplyText(body)) != "" {
		return true
	}
	// 日志落库时媒体数据被换成了占位符（metrics.go 的 filterResponseForLog）：
	// 图片 URL 与音频 data 都变成 "[...data omitted for storage]"。占位符在 =
	// 当时**确实有**媒体产出，output token 计得有理——不能报"空回复计了费"。
	if strings.Contains(body, "omitted for storage") {
		return true
	}
	// 图片另存后，响应里的图是 "[image: image/png, 245KB, id=<sha256>]"（见
	// relay/log_image_capture.go），同样说明当时确实有图片产出。
	if strings.Contains(body, `"[image: `) && strings.Contains(body, ", id=") {
		return true
	}

	var shape struct {
		Choices []struct {
			Message *struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				// OpenRouter/Ollama 风格的第二个推理字段，只认 reasoning_content
				// 会把纯推理回复误报成空回复。
				Reasoning    json.RawMessage `json:"reasoning"`
				Refusal      string          `json:"refusal"`
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
			} `json:"message"`
			Delta *struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			// 有些网关把工具调用挂在 choice 级别。
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"choices"`
		// Anthropic 原生形状：content 是 block 数组。
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		// 少数协议把工具调用放在顶层。
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		// 解析不了就**不放行也不报警**：判"空"需要证据，解析失败不是证据。
		// 这里返回 true 表示"无法证明它是空的"，宁可漏报也不误报。
		return true
	}

	if rawJSONPresent(shape.ToolCalls) {
		return true
	}
	for _, block := range shape.Content {
		switch block.Type {
		case "thinking", "redacted_thinking", "tool_use", "server_tool_use":
			return true
		}
		if strings.TrimSpace(block.Thinking) != "" || rawJSONPresent(block.Input) {
			return true
		}
	}
	for _, ch := range shape.Choices {
		if rawJSONPresent(ch.ToolCalls) {
			return true
		}
		if ch.Message != nil {
			if rawJSONPresent(ch.Message.ToolCalls) || rawJSONPresent(ch.Message.FunctionCall) ||
				strings.TrimSpace(rawJSONString(ch.Message.ReasoningContent)) != "" ||
				strings.TrimSpace(rawJSONString(ch.Message.Reasoning)) != "" ||
				strings.TrimSpace(ch.Message.Refusal) != "" {
				return true
			}
		}
		if ch.Delta != nil {
			if rawJSONPresent(ch.Delta.ToolCalls) ||
				strings.TrimSpace(rawJSONString(ch.Delta.ReasoningContent)) != "" ||
				strings.TrimSpace(rawJSONString(ch.Delta.Reasoning)) != "" {
				return true
			}
		}
	}
	return false
}

// rawJSONPresent 报告一段 RawMessage 是否有实质内容（空和字面 null 都不算）。
func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != "[]" && trimmed != `""` && trimmed != "{}"
}

// rawJSONString 把可能是 JSON 字符串的 RawMessage 解回纯文本。
func rawJSONString(raw json.RawMessage) string {
	if !rawJSONPresent(raw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// rawJSONText 把 content 字段的三种常见形状（字符串 / 部件数组 / 空）取成纯文本。
func rawJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// computeRelayProbesForLog 把一条 RelayLog 映射成探针输入并算结论。
//
// 单独一层的原因：RelayLog 字段很多且会继续加，直接用结构体到处传会让
// "这条检测到底吃了哪些字段"变得不可见。这里显式列出来。
func computeRelayProbesForLog(relayLog model.RelayLog) []model.RelayProbeFinding {
	in := RelayProbeInput{
		RequestedModel:   relayLog.ActualModelName,
		UpstreamModel:    relayLog.UpstreamResponseModel,
		InputTokens:      relayLog.InputTokens,
		OutputTokens:     relayLog.OutputTokens,
		CacheHitTokens:   relayLog.CacheHitTokens,
		CacheWriteTokens: relayLog.CacheWriteTokens,
		// relay 落库时 InputTokens 存的是上游原值（Anthropic 是独占口径），
		// 折叠后的总量单独存在 CacheInputTokens 里 —— 直接用现成的，别重推。
		CacheFoldedInputTokens: relayLog.CacheInputTokens,
		UsageSource:            relayLog.UsageSource,
		RequestContent:         relayLog.RequestContent,
		ResponseContent:        relayLog.ResponseContent,
		HasError:               strings.TrimSpace(relayLog.Error) != "" || relayLog.ErrorStatus >= 400,
	}
	// TotalTokens 不在 RelayLog 上——日志只存了输入/输出两栏, 总数由二者相加。
	// 所以账目检查在这里没有可用的第三个数, 跳过它而不是自己编一个再自己核对。
	in.TotalTokens = in.InputTokens + in.OutputTokens
	res := ComputeRelayLogProbes(in)
	// 总数是我们自己算的, 拿它去核对它自己必然相等, 这条检查在日志路径上无意义。
	out := make([]model.RelayProbeFinding, 0, len(res.Findings))
	for _, f := range res.Findings {
		if f.Code == model.ProbeCodeTokenArithmetic {
			continue
		}
		out = append(out, f)
	}
	return out
}

// passiveProbeOn 读被动检测总开关。
//
// 默认开: 被动检测不发上游请求、不耗 token、不增延迟, 没有需要保护用户
// 免于触发的理由, 所以默认值就是 true。读取失败同样按开处理——见下文。
func passiveProbeOn() bool {
	v, err := SettingGetBool(model.SettingKeyLogPassiveProbeEnabled)
	if err != nil {
		// 读失败按"开": 被动检测本身零成本, 因为一次配置读取抖动就把检测
		// 静默停掉, 比多算一次更糟(用户会以为功能坏了却没有任何提示)。
		return true
	}
	return v
}
