package modeltest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// probeConditionsMaxBytes 是观察器读取出站请求体的上限（1 MiB）。
//
// 探针请求体只有一条短题目，正常远小于此值。上限存在的意义是让「读取被
// 悄悄截断」变成「明确判为未知」，而不是拿半截 JSON 去解析出以偏概全的
// 结论——半截字节解出来的温度/系统提示是**错**的观测，比没有更危险。
const probeConditionsMaxBytes = 1 << 20

// probeConditionsObserverKey 是 context 中携带条件观察回调的私有键。
type probeConditionsObserverKey struct{}

// WithProbeConditionsObserver 把一次性的条件观察回调挂到 ctx 上。
//
// 回调只在 probe 真正发出请求前被调用一次，收到的是**只读快照**：三态标量、
// 布尔 presence、提示词 SHA256，全部是标量/枚举，不含 URL、header、密钥或
// 请求体原文。跨样本的「条件是否变化」判断属于调用方（backend）的职责，
// 观察器只如实报告「本次出站实际带了什么」。
//
// 传 nil 回调等价于不观察，直接返回原 ctx（不额外包装）。
func WithProbeConditionsObserver(ctx context.Context, observer func(numberdist.Conditions)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, probeConditionsObserverKey{}, observer)
}

func probeConditionsObserverFromContext(ctx context.Context) (func(numberdist.Conditions), bool) {
	if ctx == nil {
		return nil, false
	}
	observer, ok := ctx.Value(probeConditionsObserverKey{}).(func(numberdist.Conditions))
	if !ok || observer == nil {
		return nil, false
	}
	return observer, true
}

// observeFinalOutboundConditions 在请求真正发出前抓取最终出站条件快照。
//
// req 必须是**覆盖参数已应用、最终即将发送**的那份请求。它只经由 req.GetBody
// 读取克隆体，绝不消费 req.Body，因此对上游字节流零影响；没有注册观察者时
// 直接返回，连 GetBody 都不会触碰。
func observeFinalOutboundConditions(ctx context.Context, protocol string, req *http.Request) {
	observer, ok := probeConditionsObserverFromContext(ctx)
	if !ok {
		return
	}
	observer(buildOutboundConditions(protocol, req))
}

// buildOutboundConditions 从最终出站请求构造条件快照。
func buildOutboundConditions(protocol string, req *http.Request) numberdist.Conditions {
	conditions := numberdist.Conditions{
		Protocol:         protocol,
		ProtocolID:       numberdist.ProtocolID,
		ParserVersion:    numberdist.ParserVersion,
		TransportProfile: numberdist.TransportProfile,
		TemperatureState: numberdist.StateUnknown,
		TopPState:        numberdist.StateUnknown,
	}

	// Gemini 的 stream 只从 URL 方法后缀判定：请求体里没有该字段，而路径
	// 末尾的 :streamGenerateContent / :generateContent 是唯一权威来源。
	// 只保留一个标量布尔，绝不把路径本身存进快照。
	if protocol == "gemini-generate-content" {
		observeGeminiStream(&conditions, req)
	}

	if top, ok := readOutboundTopObject(req); ok {
		switch protocol {
		case "anthropic-messages":
			extractAnthropicConditions(&conditions, top)
		case "openai-responses":
			extractResponsesConditions(&conditions, top)
		case "gemini-generate-content":
			extractGeminiConditions(&conditions, top)
		default:
			extractChatConditions(&conditions, top)
		}
	}

	finalizeConditions(&conditions)
	return conditions
}

// readOutboundTopObject 通过 GetBody 有界读取并解析顶层 JSON 对象。
//
// 返回 false 涵盖所有「无法得到可信顶层对象」的情形：没有 GetBody、GetBody
// 报错或返回 nil、读取超限、非法 JSON、JSON null、以及数组等非对象顶层。
// 这些情况下一律保持各字段为 unknown，而不是把「没读到」冒充成「没有」。
func readOutboundTopObject(req *http.Request) (map[string]json.RawMessage, bool) {
	if req == nil || req.GetBody == nil {
		return nil, false
	}
	rc, err := req.GetBody()
	if err != nil || rc == nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(io.LimitReader(rc, probeConditionsMaxBytes+1))
	if err != nil {
		return nil, false
	}
	if len(body) > probeConditionsMaxBytes {
		return nil, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	if top == nil {
		// JSON null。
		return nil, false
	}
	return top, true
}

// probeTrackedFields 是完整性判定的固定字段顺序，保证 UnknownFields 稳定且唯一。
var probeTrackedFields = []string{
	"temperature",
	"top_p",
	"max_tokens",
	"stream",
	"thinking",
	"reasoning_effort",
	"system",
	"tools",
	"prompt_sha256",
}

// finalizeConditions 依据各字段是否已被确定，收尾 completeness 与 unknown_fields。
//
// complete 仅当九个被跟踪字段全部确定；一个都没确定时为 unknown；其余为 partial。
// 字段的最新状态完全由结构体取值推断（Nil/空串三态天然区分），因此这里是
// 「已知性」的唯一判定点，各提取函数只需如实摆放字段值。
func finalizeConditions(c *numberdist.Conditions) {
	unknown := make([]string, 0, len(probeTrackedFields))
	known := 0
	for _, field := range probeTrackedFields {
		if conditionFieldKnown(c, field) {
			known++
		} else {
			unknown = append(unknown, field)
		}
	}

	switch {
	case known == 0:
		c.Completeness = "unknown"
	case len(unknown) == 0:
		c.Completeness = "complete"
	default:
		c.Completeness = "partial"
	}

	if len(unknown) == 0 {
		c.UnknownFields = nil
	} else {
		c.UnknownFields = unknown
	}
}

func conditionFieldKnown(c *numberdist.Conditions, field string) bool {
	switch field {
	case "temperature":
		return c.TemperatureState != numberdist.StateUnknown
	case "top_p":
		return c.TopPState != numberdist.StateUnknown
	case "max_tokens":
		return c.MaxTokens != nil
	case "stream":
		return c.Stream != nil
	case "thinking":
		return c.Thinking != ""
	case "reasoning_effort":
		return c.ReasoningEffort != ""
	case "system":
		return c.SystemInjected != nil
	case "tools":
		return c.ToolsInjected != nil
	case "prompt_sha256":
		return c.PromptSHA256 != ""
	}
	return false
}

// ---------------------------------------------------------------------------
// 通用原始 JSON 取值工具
// ---------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) == "null"
}

// readFloatField 把「字段未出现 / 显式 null 或非法 / 有限数值」映射成三态。
//
//   - 未出现：StateAbsent + nil（确实是「没带」）；
//   - 出现但为 null / 非数值 / NaN / Inf：StateUnknown + nil（带了但读不懂）；
//   - 有限数值：StateValue + 指针。
func readFloatField(raw json.RawMessage, present bool) (string, *float64) {
	if !present {
		return numberdist.StateAbsent, nil
	}
	if isJSONNull(raw) {
		return numberdist.StateUnknown, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return numberdist.StateUnknown, nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return numberdist.StateUnknown, nil
	}
	return numberdist.StateValue, &f
}

// setStream 读取非 Gemini 协议顶层的 stream 布尔。
//
// 未出现、null、或非布尔一律视为 unknown（保留了但读不懂，不能当作「非流式」）。
func setStream(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["stream"]
	if !ok || isJSONNull(raw) {
		return
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return
	}
	c.Stream = boolPtr(b)
}

// setMaxTokens 读取候选 token 上限字段。
//
// 候选里出现多个真实数值时判为 unknown（无法确定上游最终采用了哪一个）；
// 只有恰好一个正值且落在有界范围内才记录，并写下命中的字段名。
func setMaxTokens(c *numberdist.Conditions, top map[string]json.RawMessage, candidates ...string) {
	field := ""
	for _, key := range candidates {
		raw, ok := top[key]
		if !ok || isJSONNull(raw) {
			continue
		}
		if field != "" {
			// 两个候选都带了真实值。
			return
		}
		field = key
	}
	if field == "" {
		return
	}
	n, ok := decodePositiveBoundedInt(top[field])
	if !ok {
		return
	}
	c.MaxTokenField = field
	c.MaxTokens = &n
}

func decodePositiveBoundedInt(raw json.RawMessage) (int, bool) {
	if isJSONNull(raw) {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// recognizedReasoningEfforts 是认可的出站强度白名单（已规范化后比较）。
var recognizedReasoningEfforts = map[string]struct{}{
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
}

// setReasoningEffort 记录一个被认可的强度档位；未出现/未知/非白名单一律留空（unknown）。
func setReasoningEffort(c *numberdist.Conditions, raw json.RawMessage) {
	if len(raw) == 0 || isJSONNull(raw) {
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return
	}
	canonical := transformermodel.CanonicalEffort(s)
	if _, ok := recognizedReasoningEfforts[canonical]; ok {
		c.ReasoningEffort = canonical
	}
}

// objectField 取出嵌套对象字段为 map；缺省/null/非对象一律返回 nil。
func objectField(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	raw, ok := m[key]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil
	}
	return sub
}

// extractTools 读取顶层 tools presence：缺席/空数组为 false，非空合法数组为 true，
// 非数组（改坏）为 unknown。
func extractTools(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["tools"]
	if !ok || isJSONNull(raw) {
		c.ToolsInjected = boolPtr(false)
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return
	}
	c.ToolsInjected = boolPtr(len(arr) > 0)
}

// singleTextContent 提取「单段文本」内容（字符串，或仅含一个 type=text 块的数组）。
func singleTextContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	if len(blocks) != 1 || blocks[0].Type != "text" {
		return "", false
	}
	return blocks[0].Text, true
}

// extractMessagesPromptText 从 messages[] 中提取唯一一条 user 文本。
//
// 多方 user、缺省、或多模态（含非文本块）都视为歧义，返回 false（保持 unknown）——
// 宁可不报，也不拿一段并非最终题目的文本去算 SHA。
func extractMessagesPromptText(top map[string]json.RawMessage) (string, bool) {
	raw, ok := top["messages"]
	if !ok || isJSONNull(raw) {
		return "", false
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return "", false
	}
	var userTexts []string
	for _, m := range msgs {
		if strings.ToLower(strings.TrimSpace(m.Role)) != "user" {
			continue
		}
		text, ok := singleTextContent(m.Content)
		if !ok {
			return "", false
		}
		userTexts = append(userTexts, text)
	}
	if len(userTexts) != 1 {
		return "", false
	}
	return userTexts[0], true
}

// promptSHA256Of 计算提示词文本的 SHA256（小写十六进制）。
//
// 当文本恰好等于内置题目时，结果与 model.HlwyPromptSHA256() 完全一致——两者
// 都是对同一段字节做 SHA256，不需另行比较，也不会泄露原文。
func promptSHA256Of(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

func extractAnthropicConditions(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["temperature"]
	c.TemperatureState, c.Temperature = readFloatField(raw, ok)
	raw, ok = top["top_p"]
	c.TopPState, c.TopP = readFloatField(raw, ok)

	setStream(c, top)
	setMaxTokens(c, top, "max_tokens")
	extractAnthropicThinking(c, top)
	if oc := objectField(top, "output_config"); oc != nil {
		setReasoningEffort(c, oc["effort"])
	}
	extractAnthropicSystem(c, top)
	extractTools(c, top)
	if text, ok := extractMessagesPromptText(top); ok {
		c.PromptSHA256 = promptSHA256Of(text)
	}
}

func extractAnthropicThinking(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["thinking"]
	if !ok || isJSONNull(raw) {
		return
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	switch strings.TrimSpace(obj.Type) {
	case "enabled", "disabled", "adaptive", "between_tools":
		c.Thinking = strings.TrimSpace(obj.Type)
	}
}

func extractAnthropicSystem(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["system"]
	if !ok || isJSONNull(raw) {
		c.SystemInjected = boolPtr(false)
		return
	}
	// 字符串形态。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		c.SystemInjected = boolPtr(strings.TrimSpace(s) != "")
		return
	}
	// 数组形态。
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		c.SystemInjected = boolPtr(len(arr) > 0)
		return
	}
	// 其它形态：读不懂，保持 unknown。
}

// ---------------------------------------------------------------------------
// OpenAI Chat
// ---------------------------------------------------------------------------

func extractChatConditions(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["temperature"]
	c.TemperatureState, c.Temperature = readFloatField(raw, ok)
	raw, ok = top["top_p"]
	c.TopPState, c.TopP = readFloatField(raw, ok)

	setStream(c, top)
	setMaxTokens(c, top, "max_tokens", "max_completion_tokens")
	extractChatThinking(c, top)
	setReasoningEffort(c, top["reasoning_effort"])
	extractChatSystem(c, top)
	extractTools(c, top)
	if text, ok := extractMessagesPromptText(top); ok {
		c.PromptSHA256 = promptSHA256Of(text)
	}
}

// extractChatThinking 只认 thinking.type（enabled/disabled）与 enable_thinking 布尔。
func extractChatThinking(c *numberdist.Conditions, top map[string]json.RawMessage) {
	if raw, ok := top["thinking"]; ok && !isJSONNull(raw) {
		var obj struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			switch strings.TrimSpace(obj.Type) {
			case "enabled", "disabled":
				c.Thinking = strings.TrimSpace(obj.Type)
			}
		}
		// thinking 对象存在但类型不认识：保持 unknown，别被 enable_thinking 猜补。
		return
	}
	if raw, ok := top["enable_thinking"]; ok && !isJSONNull(raw) {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			if b {
				c.Thinking = "enabled"
			} else {
				c.Thinking = "disabled"
			}
		}
	}
}

func extractChatSystem(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["messages"]
	if !ok || isJSONNull(raw) {
		return
	}
	var msgs []struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return
	}
	injected := false
	for _, m := range msgs {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			injected = true
		}
	}
	c.SystemInjected = boolPtr(injected)
}

// ---------------------------------------------------------------------------
// OpenAI Responses
// ---------------------------------------------------------------------------

func extractResponsesConditions(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["temperature"]
	c.TemperatureState, c.Temperature = readFloatField(raw, ok)
	raw, ok = top["top_p"]
	c.TopPState, c.TopP = readFloatField(raw, ok)

	setStream(c, top)
	setMaxTokens(c, top, "max_output_tokens")
	// Responses 无 thinking.type；强度来自 reasoning.effort。
	if reasoning := objectField(top, "reasoning"); reasoning != nil {
		setReasoningEffort(c, reasoning["effort"])
	}
	extractResponsesSystem(c, top)
	extractTools(c, top)
	if text, ok := extractResponsesPromptText(top); ok {
		c.PromptSHA256 = promptSHA256Of(text)
	}
}

func extractResponsesSystem(c *numberdist.Conditions, top map[string]json.RawMessage) {
	known := false

	if raw, ok := top["instructions"]; ok && !isJSONNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			known = true
			if strings.TrimSpace(s) != "" {
				c.SystemInjected = boolPtr(true)
				return
			}
		}
	}

	if raw, ok := top["input"]; ok && !isJSONNull(raw) {
		var items []struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &items); err == nil {
			known = true
			for _, it := range items {
				switch strings.ToLower(strings.TrimSpace(it.Role)) {
				case "system", "developer":
					c.SystemInjected = boolPtr(true)
					return
				}
			}
		}
		// input 也可能是纯字符串形态（无 system 项）。
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			known = true
		}
	}

	if known {
		c.SystemInjected = boolPtr(false)
	}
}

func extractResponsesPromptText(top map[string]json.RawMessage) (string, bool) {
	raw, ok := top["input"]
	if !ok || isJSONNull(raw) {
		return "", false
	}
	// input 直接是字符串：本身就是唯一 user 文本。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", false
	}
	var userTexts []string
	for _, it := range items {
		role := strings.ToLower(strings.TrimSpace(it.Role))
		if role != "user" {
			continue // system/developer/assistant/tool 均非 user 文本
		}
		text, ok := singleResponsesTextContent(it.Content)
		if !ok {
			return "", false
		}
		userTexts = append(userTexts, text)
	}
	if len(userTexts) != 1 {
		return "", false
	}
	return userTexts[0], true
}

func singleResponsesTextContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false
	}
	if len(parts) != 1 {
		return "", false
	}
	switch parts[0].Type {
	case "input_text", "text", "output_text":
		return parts[0].Text, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Gemini generateContent
// ---------------------------------------------------------------------------

func observeGeminiStream(c *numberdist.Conditions, req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	switch {
	case strings.HasSuffix(req.URL.Path, ":streamGenerateContent"):
		c.Stream = boolPtr(true)
	case strings.HasSuffix(req.URL.Path, ":generateContent"):
		c.Stream = boolPtr(false)
	}
}

func extractGeminiConditions(c *numberdist.Conditions, top map[string]json.RawMessage) {
	gen := objectField(top, "generationConfig")

	raw, ok := gen["temperature"]
	c.TemperatureState, c.Temperature = readFloatField(raw, ok)
	raw, ok = gen["topP"]
	c.TopPState, c.TopP = readFloatField(raw, ok)

	setMaxTokens(c, gen, "maxOutputTokens")
	extractGeminiThinking(c, gen)
	setReasoningEffort(c, objectField(gen, "thinkingConfig")["thinkingLevel"])
	extractGeminiSystem(c, top)
	extractTools(c, top)
	if text, ok := extractGeminiPromptText(top); ok {
		c.PromptSHA256 = promptSHA256Of(text)
	}
}

// extractGeminiThinking 只按清晰语义判定开关：预算 0=关、>0=开、<0（动态）=未知；
// 仅有 thinkingConfig 本身或 includeThoughts 不足以证明已开启。
func extractGeminiThinking(c *numberdist.Conditions, gen map[string]json.RawMessage) {
	tc := objectField(gen, "thinkingConfig")
	if tc == nil {
		return
	}
	if raw, ok := tc["thinkingBudget"]; ok && !isJSONNull(raw) {
		var n int64
		if err := json.Unmarshal(raw, &n); err == nil {
			switch {
			case n == 0:
				c.Thinking = "disabled"
			case n > 0:
				c.Thinking = "enabled"
			}
		}
		return
	}
	if raw, ok := tc["thinkingLevel"]; ok && !isJSONNull(raw) {
		var level string
		if err := json.Unmarshal(raw, &level); err == nil {
			switch strings.ToLower(strings.TrimSpace(level)) {
			case "low", "medium", "high":
				c.Thinking = "enabled"
			}
		}
	}
}

func extractGeminiSystem(c *numberdist.Conditions, top map[string]json.RawMessage) {
	raw, ok := top["systemInstruction"]
	if !ok || isJSONNull(raw) {
		c.SystemInjected = boolPtr(false)
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	if partsRaw, ok := obj["parts"]; ok {
		var parts []json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			c.SystemInjected = boolPtr(len(parts) > 0)
			return
		}
	}
	c.SystemInjected = boolPtr(len(obj) > 0)
}

func extractGeminiPromptText(top map[string]json.RawMessage) (string, bool) {
	raw, ok := top["contents"]
	if !ok || isJSONNull(raw) {
		return "", false
	}
	var contents []struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &contents); err != nil {
		return "", false
	}
	var userTexts []string
	for _, content := range contents {
		role := strings.ToLower(strings.TrimSpace(content.Role))
		if role != "user" && role != "" {
			continue // model 角色不是用户文本
		}
		if len(content.Parts) != 1 {
			return "", false // 多段即多模态
		}
		var part map[string]json.RawMessage
		if err := json.Unmarshal(content.Parts[0], &part); err != nil {
			return "", false
		}
		if len(part) != 1 {
			return "", false // 除 text 外还带了别的内容（图/函数调用等）
		}
		textRaw, ok := part["text"]
		if !ok {
			return "", false
		}
		var text string
		if err := json.Unmarshal(textRaw, &text); err != nil {
			return "", false
		}
		userTexts = append(userTexts, text)
	}
	if len(userTexts) != 1 {
		return "", false
	}
	return userTexts[0], true
}
