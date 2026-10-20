package op

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhizhishu/octopus/internal/model"
)

// 本文件是在 RelayLogAdd 收到的“已脱敏日志副本”上做有界纯本地被动观察的实现。
// 约束(与主管批准的最小设计一致):
//   - 只导入 stdlib + db model, 不引 Sender/http/task, 无任何外部副作用;
//   - 不复制业务正文, 只产出结构化摘要; 不改 ResponseContent;
//   - 不判模型真假, 没有 confidence/eProcess/真假断言; 无已校准基准时
//     一律 status=insufficient_evidence, reason=no_calibrated_profile;
//   - 特征算法独立实现(字符/行/段落/代码围栏/Markdown 行), 不复制雷达代码。

const (
	passiveMaxEnvelopeBytes = 128 * 1024 // 脱敏 JSON 总体上限, 超过整体跳过
	passiveMaxTextBytes     = 32 * 1024  // 候选文本上限, 超过跳过不静默截断
)

// passiveLogShape 是对脱敏 ResponseContent 的最小解析形状(OpenAI chat 家族)。
// 只挑字段, 不复制正文。
type passiveLogShape struct {
	Choices []passiveLogChoice `json:"choices"`
}
type passiveLogChoice struct {
	FinishReason *string            `json:"finish_reason"`
	Message      *passiveLogMessage `json:"message"`
	Delta        *passiveLogMessage `json:"delta"`
}
type passiveLogMessage struct {
	Role      string            `json:"role"`
	Content   *string           `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
	Refusal   *string           `json:"refusal"`
	// ContentArr 是 content 为部件数组时的兼容形状, 由 UnmarshalJSON 兜底;
	// 其它形状(数字/对象/部件数组解析失败/legacy function_call 等)置 NonText 保守拒绝。
	ContentArr []passiveLogAPart
	NonText    bool
}
type passiveLogAPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UnmarshalJSON 让 Content 兼容 string 或 part 数组两种形状:
// 字符串照常填 Content; 数组时存进 ContentArr; 其它形状/解析失败置 NonText。
func (m *passiveLogMessage) UnmarshalJSON(data []byte) error {
	type shadow struct {
		Role         string            `json:"role"`
		Content      json.RawMessage   `json:"content"`
		ToolCalls    []json.RawMessage `json:"tool_calls"`
		Refusal      *string           `json:"refusal"`
		FunctionCall json.RawMessage   `json:"function_call"`
		Audio        json.RawMessage   `json:"audio"`
		Images       json.RawMessage   `json:"images"`
	}
	var s shadow
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	m.Role = s.Role
	m.ToolCalls = s.ToolCalls
	m.Refusal = s.Refusal
	if (len(s.FunctionCall) > 0 && string(s.FunctionCall) != "null") ||
		(len(s.Audio) > 0 && string(s.Audio) != "null") ||
		(len(s.Images) > 0 && string(s.Images) != "null") {
		m.NonText = true
	}
	trimmed := strings.TrimSpace(string(s.Content))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(s.Content, &str); err != nil {
			return err
		}
		m.Content = &str
		return nil
	}
	if trimmed[0] == '[' {
		var parts []passiveLogAPart
		if err := json.Unmarshal(s.Content, &parts); err != nil {
			m.NonText = true
			return nil
		}
		m.ContentArr = parts
		return nil
	}
	// 其它形状(数字/对象等): 保守非文本。
	m.NonText = true
	return nil
}

// computePassiveSummary 纯函数: 输入一条 RelayLog(其 ResponseContent 已脱敏),
// 输出被动观察摘要; 任何分支都不修改输入、不返回错误(失败以 not_applicable 表达)。
func computePassiveSummary(relayLog model.RelayLog) *model.RelayLogPassiveSummary {
	na := func(status, reason string) *model.RelayLogPassiveSummary {
		return &model.RelayLogPassiveSummary{
			Version: model.PassiveSummaryVersion,
			Status:  status,
			Reason:  reason,
		}
	}

	// 范围外端点: 渠道测试/嵌入/媒体不在被动观察范围。
	endpoint := strings.ToLower(strings.TrimSpace(relayLog.RequestEndpoint))
	if strings.HasPrefix(endpoint, "model_test") || endpoint == "embeddings" ||
		strings.HasPrefix(endpoint, "embeddings_") || endpoint == "images" ||
		strings.HasPrefix(endpoint, "images_") || endpoint == "videos" ||
		strings.HasPrefix(endpoint, "videos_") || endpoint == "audio" ||
		strings.HasPrefix(endpoint, "audio_") {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonOutOfRange)
	}

	// 取消先于 generic error 判。
	if strings.EqualFold(strings.TrimSpace(relayLog.ErrorCode), "octopus_client_canceled") ||
		relayLog.UsageMissingReason == model.RelayLogUsageMissingReasonClientAborted {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonCanceled)
	}
	// 失败请求不观察(填充日志后重算同样落到这里, 不会变成有效观察)。
	if strings.TrimSpace(relayLog.Error) != "" ||
		strings.TrimSpace(relayLog.ErrorCode) != "" ||
		relayLog.ErrorStatus >= 400 {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonError)
	}
	if relayLog.UsageMissingReason == model.RelayLogUsageMissingReasonOpaqueResponse {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonOpaqueResponse)
	}

	body := relayLog.ResponseContent
	if strings.TrimSpace(body) == "" {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonBlank)
	}
	if len(body) > passiveMaxEnvelopeBytes {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonOversizedEnvelope)
	}

	var shape passiveLogShape
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonUnparseable)
	}

	// 只接受恰好 1 个 choice; 多个 choice 无法保证同源聚合, 不挑“任意一个 stop”。
	if len(shape.Choices) != 1 {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonMultipleChoices)
	}
	choice := shape.Choices[0]
	// finish_reason 必须存在且 == "stop"; 未知/缺失不当完整。
	if choice.FinishReason == nil || strings.TrimSpace(*choice.FinishReason) != "stop" {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonUnknownFinish)
	}
	// 流式 Delta 缺 Message, 无从保证聚合完整, 跳过。
	msg := choice.Message
	if msg == nil {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonUnknownFinish)
	}
	// 工具调用/拒答一律不观察。
	if len(msg.ToolCalls) > 0 || (msg.Refusal != nil && strings.TrimSpace(*msg.Refusal) != "") {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonToolOutput)
	}
	// 只接受 role==assistant; 缺 role 保守按 non_text; tool 角色/legacy
	// function_call/audio/images 即使另有文本也拒绝。
	if msg.NonText || strings.TrimSpace(msg.Role) != "assistant" {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonNonText)
	}

	// 候选文本: 仅接受纯文本内容。
	var text string
	switch {
	case msg.Content != nil:
		text = *msg.Content
	case len(msg.ContentArr) > 0:
		// 多个 content 部件只在全部 type=text 时合并; 任何工具/图片/音频部件 → 拒绝。
		var sb strings.Builder
		for _, part := range msg.ContentArr {
			pt := strings.ToLower(strings.TrimSpace(part.Type))
			if pt != "text" {
				return na(model.PassiveStatusNotApplicable, model.PassiveReasonNonText)
			}
			sb.WriteString(part.Text)
		}
		text = sb.String()
	default:
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonNonText)
	}

	if strings.TrimSpace(text) == "" {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonBlank)
	}
	if utf8.RuneCountInString(text) == 0 || len(text) > passiveMaxTextBytes {
		return na(model.PassiveStatusNotApplicable, model.PassiveReasonOversizedText)
	}

	features := passiveFeatures(text)
	return &model.RelayLogPassiveSummary{
		Version:  model.PassiveSummaryVersion,
		Status:   model.PassiveStatusInsufficientEvidence,
		Reason:   "no_calibrated_profile",
		Language: passiveLanguageLabel(text),
		Features: &features,
	}
}

// passiveFeatures 独立实现字符统计, 不复制任何雷达/画像代码。
func passiveFeatures(text string) model.RelayLogPassiveFeatures {
	lines := strings.Count(text, "\n") + 1
	paragraphs := 0
	inPara := false
	markdownLines := 0
	inCode := false
	codeBlockLines := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.TrimSpace(trimmed), "```") {
			inCode = !inCode
			codeBlockLines++
			continue
		}
		if inCode {
			codeBlockLines++
			continue
		}
		if trimmed == "" {
			inPara = false
			continue
		}
		if !inPara {
			paragraphs++
			inPara = true
		}
		if passiveMarkdownLine(trimmed) {
			markdownLines++
		}
	}
	total := utf8.RuneCountInString(text)
	var codeRatio float64
	if total > 0 {
		codeRatio = float64(codeBlockLines) / float64(lines)
	}
	return model.RelayLogPassiveFeatures{
		Chars:         total,
		Lines:         lines,
		Paragraphs:    paragraphs,
		CodeBlocks:    countFences(text),
		CodeRatio:     codeRatio,
		MarkdownLines: markdownLines,
	}
}

// countFences 数 ``` 围栏对(开+闭算 1 个块; 未闭合的落单围栏不计块)。
func countFences(text string) int {
	fences := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fences++
		}
	}
	return fences / 2
}

// passiveMarkdownLine 粗判一行是否 Markdown 结构行(标题/列表/引用/表格分隔)。
func passiveMarkdownLine(trimmed string) bool {
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ">") {
		return true
	}
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ") {
		return true
	}
	if len(trimmed) > 2 && trimmed[0] >= '1' && trimmed[0] <= '9' &&
		(trimmed[1] == '.' || trimmed[1] == ')') {
		return true
	}
	if strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "---") || strings.HasPrefix(trimmed, "***") {
		return true
	}
	return false
}

// passiveLanguageLabel 按汉字/拉丁字母计数给粗标签, 明确不是语种认证。
func passiveLanguageLabel(text string) string {
	var cjk, latin int
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			cjk++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			latin++
		}
	}
	switch {
	case cjk == 0 && latin == 0:
		return "other"
	case cjk > 0 && latin > 0:
		// 双方都过一定量才算 mixed, 少量拉丁字母(缩写/代码词)不算。
		if cjk >= 8 && latin >= 8 {
			return "mixed"
		}
		if cjk >= latin {
			return "zh"
		}
		return "en"
	case cjk > 0:
		return "zh"
	default:
		return "en"
	}
}
