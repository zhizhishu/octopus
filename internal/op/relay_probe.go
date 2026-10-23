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
	if in.InputTokens <= 0 {
		return nil
	}
	cacheTotal := in.CacheHitTokens + in.CacheWriteTokens
	if cacheTotal <= 0 || cacheTotal <= in.InputTokens {
		return nil
	}
	return []model.RelayProbeFinding{{
		Code:     model.ProbeCodeCacheExceedsPrompt,
		Severity: model.RelayProbeSeverityMedium,
		Title: fmt.Sprintf("缓存计数超过了总输入：缓存命中 %d + 缓存写入 %d = %d，比输入总数 %d 还多。"+
			"缓存本该是输入的一部分，超过它说明计数口径不对",
			in.CacheHitTokens, in.CacheWriteTokens, cacheTotal, in.InputTokens),
		Evidence: map[string]any{
			"cache_hit":    in.CacheHitTokens,
			"cache_write":  in.CacheWriteTokens,
			"cache_total":  cacheTotal,
			"input_tokens": in.InputTokens,
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
	text := visibleReplyText(in.ResponseContent)
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
	severity := model.RelayProbeSeverityLow
	// 只有明显越界才升级：擦边值多半是中文/代码这类正常但极端的文本。
	if ratio < 1.0 || ratio > 20 {
		severity = model.RelayProbeSeverityMedium
	}
	return model.RelayProbeFinding{
		Code:     model.ProbeCodeCharTokenRatio,
		Severity: severity,
		Title: fmt.Sprintf("回复 %d 个字符却记了 %d 个输出 token，相当于每个 token %.2f 个字。"+
			"正常文本大约每个 token 2–5 个字，这个比例明显偏离，可能是 token 被多算或输出被截断了",
			chars, in.OutputTokens, ratio),
		Evidence: map[string]any{
			"chars":         chars,
			"output_tokens": in.OutputTokens,
			"ratio":         math.Round(ratio*100) / 100,
			"min":           model.ProbeCharTokenMin,
			"max":           model.ProbeCharTokenMax,
		},
	}, true
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
	if in.InputTokens < model.ProbeTokenizerMinTokens {
		return model.RelayProbeFinding{}, false
	}
	text := visibleRequestText(in.RequestContent)
	if strings.TrimSpace(text) == "" {
		return model.RelayProbeFinding{}, false
	}
	local := tokenizer.CountTokens(text, in.RequestedModel)
	// 0 在 CountTokens 里是"算失败"，不是"零个 token"——绝不能当有效测量。
	if local <= 0 {
		return model.RelayProbeFinding{}, false
	}
	diff := float64(local-in.InputTokens) / float64(in.InputTokens)
	if math.Abs(diff) <= model.ProbeTokenizerResidualTolerance {
		return model.RelayProbeFinding{}, false
	}
	return model.RelayProbeFinding{
		Code:     model.ProbeCodeTokenizerResidual,
		Severity: model.RelayProbeSeverityMedium,
		Title: fmt.Sprintf("上游报的输入是 %d 个 token，但用标准分词器复算同样的文字只有 %d 个（差 %+.0f%%）。"+
			"偏差这么大说明背后用的很可能不是这个模型该有的分词器——不同厂商的分词口径差异恰好就在这个量级",
			in.InputTokens, local, diff*100),
		Evidence: map[string]any{
			"upstream_input": in.InputTokens,
			"local_recount":  local,
			"relative_diff":  math.Round(diff*1000) / 1000,
			"tolerance":      model.ProbeTokenizerResidualTolerance,
			"min_tokens":     model.ProbeTokenizerMinTokens,
		},
	}, true
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

	var shape struct {
		Choices []struct {
			Message *struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
				FunctionCall     json.RawMessage `json:"function_call"`
			} `json:"message"`
			Delta *struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
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
				strings.TrimSpace(rawJSONString(ch.Message.ReasoningContent)) != "" {
				return true
			}
		}
		if ch.Delta != nil {
			if rawJSONPresent(ch.Delta.ToolCalls) ||
				strings.TrimSpace(rawJSONString(ch.Delta.ReasoningContent)) != "" {
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

// visibleRequestText 从请求 JSON 里取出模型真正看到的文字（所有 message 的文本）。
//
// 刻意不含 tools / system 的结构字段：各家序列化方式不同，把那部分算进来会让
// 残差判定变成"比谁的工具声明更啰嗦"，而不是"比分词器"。
func visibleRequestText(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var shape struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		// Anthropic 顶层 system 是独立的。
		System json.RawMessage `json:"system"`
		// 原生 completions 的 prompt。
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return ""
	}
	var sb strings.Builder
	if s := rawJSONText(shape.System); s != "" {
		sb.WriteString(s)
	}
	for _, msg := range shape.Messages {
		if s := rawJSONText(msg.Content); s != "" {
			sb.WriteString(s)
		}
	}
	if sb.Len() == 0 {
		if s := rawJSONText(shape.Prompt); s != "" {
			sb.WriteString(s)
		}
	}
	return sb.String()
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
		RequestContent:   relayLog.RequestContent,
		ResponseContent:  relayLog.ResponseContent,
		HasError:         strings.TrimSpace(relayLog.Error) != "" || relayLog.ErrorStatus >= 400,
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
