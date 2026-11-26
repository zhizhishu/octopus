package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	geminiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// ---------- 旧四层算法 oracle（a7fd2e93 原封搬入；仅测试用，自包含） ----------
//
// 生产路径的四层读字段函数已由规则表（internal/model/prompt_rule.go + internal/prompt
// 的 Evaluate/Apply）取代；本节是 golden 测试的**独立** oracle：把 legacy 三元组
// （settings + plan/rule/channel 字段）按旧顺序逐层应用，给 wire 比对当对照基准。
//
// 独立性要求（不得退化）：
//   - 本节**不** import/call internal/prompt 的新实现（Evaluate/Apply/SlotFor/ApplyText/
//     InsertSystemMessage 等一概不用），也不复用 model 层的「开关/受众/模式归一」判定；
//     四层遍历、enabled/非空判断、per-协议 join 与镜像 after 规则、客户端 system 保留逻辑
//     全部是本文件内的副本，新旧任何一边改动都不会自动带到另一边。
//   - 比对在**完整 wire bytes**（含 system 消息结构、header 级格式）上做，失败时打印首个
//     差异位置（firstWireDiff），不是只比内部字段。
//
// 生产路径不得调用本节任何函数。

type promptOverrideLayer struct {
	source string
	mode   dbmodel.PromptOverrideMode
	text   string
}

// applyPromptOverrides applies the system prompt layers global → plan → route rule → channel.
//
// A layer is skipped entirely unless its 启用 switch is on and its text is non-blank
// (oraclePromptOverrideActive). An applied layer puts its text before or after the
// request's own system prompt, using new-api's per-protocol joining rules
// (new-api relay/chat_completions_via_responses.go applySystemPromptIfNeeded,
// relay/claude_handler.go, relay/gemini_handler.go, relay/channel/codex/adaptor.go) and their
// mirror image for "after". When the request has no system prompt the text becomes it.
// The client's own system content is never removed or reordered.
//
// Each layer is applied on top of the previous result, so the final order is
//
//	channel-before, route-before, plan-before, global-before, CLIENT, global-after, plan-after, route-after, channel-after
//
// i.e. the more specific a layer is, the further out it sits.
func applyPromptOverrides(req *transformerModel.InternalLLMRequest, plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) promptOverrideSnapshot {
	return applyPromptOverridesForAudience(req, plan, rule, channel, false)
}

// Only the authenticated API-key principal can opt into administrator rules.
// The legacy entry point above is deliberately non-admin for direct test callers.
func applyPromptOverridesForAudience(req *transformerModel.InternalLLMRequest, plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel, authenticatedAdmin bool) promptOverrideSnapshot {
	snapshot := promptOverrideSnapshot{}
	if req == nil || !req.IsChatRequest() {
		return snapshot
	}
	before, after := false, false
	for _, layer := range promptOverrideLayersForAudience(plan, rule, channel, authenticatedAdmin) {
		mode := oracleNormalizePromptOverrideMode(layer.mode)
		oracleApplyPromptOverrideText(req, strings.TrimSpace(layer.text), mode)
		snapshot.Sources = append(snapshot.Sources, layer.source)
		if mode == dbmodel.PromptOverrideModeAfter {
			after = true
		} else {
			before = true
		}
	}
	switch {
	case before && after:
		snapshot.Mode = "before+after"
	case after:
		snapshot.Mode = dbmodel.PromptOverrideModeAfter
	case before:
		snapshot.Mode = dbmodel.PromptOverrideModeBefore
	}
	return snapshot
}

// promptOverrideLayers returns only the layers that will add something (switch on, text set).
func promptOverrideLayers(plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) []promptOverrideLayer {
	return promptOverrideLayersForAudience(plan, rule, channel, false)
}

func promptOverrideLayersForAudience(plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel, authenticatedAdmin bool) []promptOverrideLayer {
	layers := make([]promptOverrideLayer, 0, 4)
	if text, mode, enabled := globalPromptOverride(); oraclePromptOverrideActive(enabled, mode, text) && oraclePromptAudienceAllows(globalPromptAudience(), authenticatedAdmin) {
		layers = append(layers, promptOverrideLayer{source: "global", mode: mode, text: text})
	}
	if plan != nil && oraclePromptAudienceAllows(plan.PromptAudience, authenticatedAdmin) && oraclePromptOverrideActive(plan.PromptOverrideEnabled, plan.PromptOverrideMode, plan.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "access_plan:" + plan.Slug, mode: plan.PromptOverrideMode, text: plan.SystemPromptOverride})
	}
	if rule != nil && oraclePromptAudienceAllows(rule.PromptAudience, authenticatedAdmin) && oraclePromptOverrideActive(rule.PromptOverrideEnabled, rule.PromptOverrideMode, rule.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "route_rule:" + rule.RequestModel, mode: rule.PromptOverrideMode, text: rule.SystemPromptOverride})
	}
	if channel != nil && oraclePromptAudienceAllows(channel.PromptAudience, authenticatedAdmin) && oraclePromptOverrideActive(channel.PromptOverrideEnabled, channel.PromptOverrideMode, channel.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "channel:" + channel.Name, mode: channel.PromptOverrideMode, text: channel.SystemPromptOverride})
	}
	return layers
}

func globalPromptAudience() dbmodel.PromptAudience {
	value, err := op.SettingGetString(dbmodel.SettingKeyPromptAudience)
	if err != nil {
		return dbmodel.PromptAudienceAll
	}
	return dbmodel.PromptAudience(value)
}

func globalPromptOverride() (string, dbmodel.PromptOverrideMode, bool) {
	text, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideSystem)
	if err != nil {
		return "", dbmodel.PromptOverrideModeBefore, false
	}
	mode := dbmodel.PromptOverrideModeBefore
	if value, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideMode); err == nil {
		mode = dbmodel.PromptOverrideMode(value)
	}
	enabled := false
	if value, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideEnabled); err == nil {
		enabled = value == "true"
	}
	return strings.TrimSpace(text), mode, enabled
}

// ---------- oracle 核心（a7fd2e93 的拼接实现，逐函数原封复制，只加 oracle 前缀） ----------

// oraclePromptOverrideActive：旧「开关 + 非空文本」判定（dbmodel.PromptOverrideActive 的副本）。
// 文本 trim 后为空 → 这一层什么都不加；legacy 模式值（没有开关列的旧数据）视作开。
func oraclePromptOverrideActive(enabled bool, mode dbmodel.PromptOverrideMode, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	return enabled || oracleIsLegacyPromptOverrideMode(mode)
}

// oracleIsLegacyPromptOverrideMode：append_system / replace_system 是开关列之前的旧值。
func oracleIsLegacyPromptOverrideMode(mode dbmodel.PromptOverrideMode) bool {
	return mode == dbmodel.PromptOverrideModeAppendSystem || mode == dbmodel.PromptOverrideModeReplaceSystem
}

// oracleNormalizePromptOverrideMode：任何存储值都折到 before/after（dbmodel 同义）。
func oracleNormalizePromptOverrideMode(mode dbmodel.PromptOverrideMode) dbmodel.PromptOverrideMode {
	if mode == dbmodel.PromptOverrideModeAfter {
		return dbmodel.PromptOverrideModeAfter
	}
	return dbmodel.PromptOverrideModeBefore
}

// oraclePromptAudienceAllows：旧受众判定（dbmodel.PromptAudienceAllows 的副本）。
func oraclePromptAudienceAllows(audience dbmodel.PromptAudience, authenticatedAdmin bool) bool {
	switch audience {
	case "", dbmodel.PromptAudienceAll:
		return true
	case dbmodel.PromptAudienceAdminOnly:
		return authenticatedAdmin
	default:
		return false
	}
}

// oracleClearResponsesRawPromptShape：旧 relay 在注入后的形状清理（生产 prompt_override.go
// 的同名函数副本），保持 oracle 与本轮生产改动完全解耦。
func oracleClearResponsesRawPromptShape(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	req.ResponsesInstructions = nil
	req.ResponsesInputRaw = nil
}

// oraclePromptSlot locates the request's own system prompt inside the internal message list.
type oraclePromptSlot struct {
	// index of the message holding the system prompt, -1 when the request has none.
	index int
	// asNewBlock: the protocol keeps the system prompt as an array of blocks; a layer is
	// added as its own block (Anthropic array-form system) instead of editing text.
	asNewBlock bool
	// beforeAt / afterAt: where a new block goes for "before" / "after".
	beforeAt, afterAt int
	// trimExisting: new-api TrimSpace's the existing text and drops the "\n" when it is blank
	// (Anthropic string system, Responses instructions).
	trimExisting bool
}

func oraclePromptSlotFor(req *transformerModel.InternalLLMRequest) oraclePromptSlot {
	switch req.RawAPIFormat {
	case transformerModel.APIFormatAnthropicMessage:
		return oracleAnthropicPromptSlot(req)
	case transformerModel.APIFormatOpenAIResponse:
		// Inbound puts a non-empty `instructions` as Messages[0] and keeps the raw value in
		// ResponsesInstructions. developer/system items inside `input` are NOT the
		// instructions slot (new-api's codex adaptor only looks at `instructions`).
		if req.ResponsesInstructions != nil && len(req.Messages) > 0 && oracleIsSystemRole(req.Messages[0].Role) {
			return oraclePromptSlot{index: 0, trimExisting: true}
		}
		return oraclePromptSlot{index: -1}
	case transformerModel.APIFormatGeminiContents:
		// Inbound joins systemInstruction.parts' non-empty texts with "\n" into Messages[0],
		// so editing its start/end equals editing the first/last non-empty text part.
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			return oraclePromptSlot{index: 0}
		}
		return oraclePromptSlot{index: -1}
	default:
		// OpenAI chat: first system (or developer) message anywhere, like new-api's
		// containSystemPrompt loop.
		for i, msg := range req.Messages {
			if oracleIsSystemRole(msg.Role) {
				return oraclePromptSlot{index: i}
			}
		}
		return oraclePromptSlot{index: -1}
	}
}

// Claude CLI system header blocks. A genuine Claude Code request starts its system array
// with the billing header block followed by the agent identity block; the Anthropic outbound
// cloak keys on exactly that (billing header must stay parts[0], otherwise it synthesizes
// one). A "before" block is therefore inserted AFTER these blocks so the CLI's own blocks
// keep their positions and bytes and no second billing header is ever synthesized.
const (
	oracleClaudeBillingHeaderPrefix = "x-anthropic-billing-header:"
	oracleClaudeAgentSDKMarker      = "built on Anthropic's Claude Agent SDK"
	oracleClaudeCodeIdentityMarker  = "You are Claude Code, Anthropic's official CLI for Claude"
)

func oracleAnthropicPromptSlot(req *transformerModel.InternalLLMRequest) oraclePromptSlot {
	// Only the leading system run is the top-level `system`; later role=system messages are
	// mid-conversation system messages (claude CLI beta) and stay where they are.
	leading := 0
	for leading < len(req.Messages) && req.Messages[leading].Role == "system" {
		leading++
	}
	if leading == 0 {
		return oraclePromptSlot{index: -1}
	}
	if req.TransformerMetadata["anthropic_system_array_format"] != "true" {
		return oraclePromptSlot{index: 0, trimExisting: true}
	}
	beforeAt := 0
	if beforeAt < leading && strings.HasPrefix(strings.TrimSpace(oraclePromptMessageText(req.Messages[beforeAt])), oracleClaudeBillingHeaderPrefix) {
		beforeAt++
	}
	if beforeAt < leading {
		text := oraclePromptMessageText(req.Messages[beforeAt])
		if strings.Contains(text, oracleClaudeAgentSDKMarker) || strings.HasPrefix(strings.TrimSpace(text), oracleClaudeCodeIdentityMarker) {
			beforeAt++
		}
	}
	return oraclePromptSlot{index: 0, asNewBlock: true, beforeAt: beforeAt, afterAt: leading}
}

func oracleApplyPromptOverrideText(req *transformerModel.InternalLLMRequest, text string, mode dbmodel.PromptOverrideMode) {
	after := mode == dbmodel.PromptOverrideModeAfter
	slot := oraclePromptSlotFor(req)
	if slot.index < 0 {
		oracleInsertSystemMessage(req, 0, text)
		if req.RawAPIFormat == transformerModel.APIFormatOpenAIResponse {
			instructions := text
			req.ResponsesInstructions = &instructions
		}
		return
	}
	if slot.asNewBlock {
		if after {
			oracleInsertSystemMessage(req, slot.afterAt, text)
		} else {
			oracleInsertSystemMessage(req, slot.beforeAt, text)
		}
		return
	}
	msg := &req.Messages[slot.index]
	if len(msg.Content.MultipleContent) > 0 {
		part := transformerModel.MessageContentPart{Type: "text", Text: &text}
		parts := make([]transformerModel.MessageContentPart, 0, len(msg.Content.MultipleContent)+1)
		if after {
			parts = append(append(parts, msg.Content.MultipleContent...), part)
		} else {
			parts = append(append(parts, part), msg.Content.MultipleContent...)
		}
		msg.Content.MultipleContent = parts
	} else {
		existing := ""
		if msg.Content.Content != nil {
			existing = *msg.Content.Content
		}
		if slot.trimExisting {
			existing = strings.TrimSpace(existing)
		}
		merged := text
		switch {
		case slot.trimExisting && existing == "":
		case after:
			merged = existing + "\n" + text
		default:
			merged = text + "\n" + existing
		}
		msg.Content.Content = &merged
	}
	if req.RawAPIFormat == transformerModel.APIFormatOpenAIResponse && slot.index == 0 && msg.Content.Content != nil {
		instructions := *msg.Content.Content
		req.ResponsesInstructions = &instructions
	}
}

func oracleInsertSystemMessage(req *transformerModel.InternalLLMRequest, at int, text string) {
	content := text
	msg := transformerModel.Message{Role: "system", Content: transformerModel.MessageContent{Content: &content}}
	out := make([]transformerModel.Message, 0, len(req.Messages)+1)
	out = append(out, req.Messages[:at]...)
	out = append(out, msg)
	out = append(out, req.Messages[at:]...)
	req.Messages = out
}

func oracleIsSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

func oraclePromptMessageText(msg transformerModel.Message) string {
	if msg.Content.Content != nil {
		return *msg.Content.Content
	}
	for _, part := range msg.Content.MultipleContent {
		if part.Type == "text" && part.Text != nil {
			return *part.Text
		}
	}
	return ""
}

type promptResult struct {
	body   map[string]any
	raw    []byte
	header http.Header
	snap   promptOverrideSnapshot
}

// promptWire parses body with the inbound, applies the layers exactly like relay.go does
// (applyPromptOverrides, then oracleClearResponsesRawPromptShape when a layer applied), runs
// the real outbound and returns the upstream body/headers. No network. The oracle side never
// touches internal/prompt: the expected path is the a7fd2e93 copy below.
func promptWire(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType,
	tweak func(*transformerModel.InternalLLMRequest), plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel,
) promptResult {
	return promptWireForAudience(t, in, body, out, tweak, plan, rule, channel, false)
}

// promptWireForAudience 与 promptWire 相同，但把「调用者是不是已认证管理员」交给 oracle，
// 用来让 admin_only 受众也能在旧四层 oracle 上跑出基准字节。
func promptWireForAudience(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType,
	tweak func(*transformerModel.InternalLLMRequest), plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel,
	authenticatedAdmin bool,
) promptResult {
	t.Helper()
	ctx := context.Background()
	if in == inbound.InboundTypeGemini {
		ctx = geminiInbound.WithRequestOptions(ctx, "gemini-x", false)
	}
	req, err := inbound.Get(in).TransformRequest(ctx, []byte(body))
	if err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if tweak != nil {
		tweak(req)
	}
	snap := applyPromptOverridesForAudience(req, plan, rule, channel, authenticatedAdmin)
	if len(snap.Sources) > 0 {
		oracleClearResponsesRawPromptShape(req)
	}
	httpReq, err := outbound.Get(out).TransformRequest(context.Background(), req, "https://upstream.invalid", "sk-test")
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	raw, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return promptResult{body: got, raw: raw, header: httpReq.Header.Clone(), snap: snap}
}

const (
	before = dbmodel.PromptOverrideModeBefore
	after  = dbmodel.PromptOverrideModeAfter
)

func ruleWith(text string, mode dbmodel.PromptOverrideMode) *dbmodel.AccessRouteRule {
	return &dbmodel.AccessRouteRule{RequestModel: "m", SystemPromptOverride: text, PromptOverrideMode: mode, PromptOverrideEnabled: true}
}

func ruleOff(text string, mode dbmodel.PromptOverrideMode) *dbmodel.AccessRouteRule {
	r := ruleWith(text, mode)
	r.PromptOverrideEnabled = false
	return r
}

func jsonEq(t *testing.T, label string, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("%s: bad want json: %v", label, err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("%s:\n got  %s\n want %s", label, g, wantJSON)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertOffIsByteIdentical: switch off (in both positions, every layer) must send exactly
// the bytes and headers of a request with no prompt configured at all.
func assertOffIsByteIdentical(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType, tweak func(*transformerModel.InternalLLMRequest)) {
	t.Helper()
	base := promptWire(t, in, body, out, tweak, nil, nil, nil)
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		other := after
		if mode == after {
			other = before
		}
		plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "PLAN", PromptOverrideMode: mode}
		channel := &dbmodel.Channel{Name: "c", SystemPromptOverride: "CHAN", PromptOverrideMode: other}
		got := promptWire(t, in, body, out, tweak, plan, ruleOff("ROUTE", mode), channel)
		if !bytes.Equal(base.raw, got.raw) {
			t.Fatalf("%s: switch off changed the upstream body:\n%s\n%s", mode, base.raw, got.raw)
		}
		if !reflect.DeepEqual(base.header, got.header) {
			t.Fatalf("%s: switch off changed upstream headers:\n%v\n%v", mode, base.header, got.header)
		}
		if len(got.snap.Sources) != 0 || got.snap.Mode != "" {
			t.Fatalf("%s: snapshot %#v", mode, got.snap)
		}
	}
}

// ---------- OpenAI chat: first system/developer message ----------

func TestPromptChatStringBeforeAfter(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{before: "P\nS", after: "S\nP"} {
		got := promptWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["messages"], `[{"role":"system","content":`+mustJSON(t, want)+`},{"role":"user","content":"hi"}]`)
		if got.snap.Mode != mode {
			t.Fatalf("snapshot mode %q", got.snap.Mode)
		}
	}
}

func TestPromptChatArrayBeforeAfter(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1"},{"type":"text","text":"S2"}]},{"role":"user","content":"hi"}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{
		before: `[{"type":"text","text":"P"},{"type":"text","text":"S1"},{"type":"text","text":"S2"}]`,
		after:  `[{"type":"text","text":"S1"},{"type":"text","text":"S2"},{"type":"text","text":"P"}]`,
	} {
		got := promptWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["messages"].([]any)[0].(map[string]any)["content"], want)
	}
}

func TestPromptChatFirstSystemAnywhereDeveloperCounts(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"developer","content":"D"}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{before: "P\nD", after: "D\nP"} {
		got := promptWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, nil, ruleWith("P", mode), nil)
		msgs := got.body["messages"].([]any)
		if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != want {
			t.Fatalf("%s: %v", mode, msgs)
		}
	}
}

func TestPromptChatNoSystemBothPositionsAddSystem(t *testing.T) {
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeOpenAIChat, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			outbound.OutboundTypeOpenAIChat, nil, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["messages"], `[{"role":"system","content":"P"},{"role":"user","content":"hi"}]`)
	}
}

func TestPromptChatOffIsByteIdentical(t *testing.T) {
	assertOffIsByteIdentical(t, inbound.InboundTypeOpenAIChat,
		`{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`, outbound.OutboundTypeOpenAIChat, nil)
	assertOffIsByteIdentical(t, inbound.InboundTypeOpenAIChat,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, outbound.OutboundTypeOpenAIChat, nil)
}

// ---------- OpenAI Responses: instructions ----------

const responsesBody = `{"model":"m","instructions":"I","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func TestPromptResponsesBeforeAfter(t *testing.T) {
	for mode, want := range map[dbmodel.PromptOverrideMode]string{before: "P\nI", after: "I\nP"} {
		got := promptWire(t, inbound.InboundTypeOpenAIResponse, responsesBody, outbound.OutboundTypeOpenAIResponse, nil, nil, ruleWith("P", mode), nil)
		if got.body["instructions"] != want {
			t.Fatalf("%s: instructions %q", mode, got.body["instructions"])
		}
		if strings.Contains(mustJSON(t, got.body["input"]), `"P"`) {
			t.Fatalf("layer text leaked into input: %s", mustJSON(t, got.body["input"]))
		}
	}
}

func TestPromptResponsesNoInstructionsBothPositionsSetIt(t *testing.T) {
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeOpenAIResponse,
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			outbound.OutboundTypeOpenAIResponse, nil, nil, ruleWith("P", mode), nil)
		if got.body["instructions"] != "P" {
			t.Fatalf("%s: %v", mode, got.body["instructions"])
		}
	}
}

func TestPromptResponsesOffIsByteIdentical(t *testing.T) {
	assertOffIsByteIdentical(t, inbound.InboundTypeOpenAIResponse,
		`{"model":"m","instructions":"I","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"D"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		outbound.OutboundTypeOpenAIResponse, nil)
}

func TestPromptResponsesDeveloperInputIsNotInstructions(t *testing.T) {
	in, err := inbound.Get(inbound.InboundTypeOpenAIResponse).TransformRequest(context.Background(),
		[]byte(`{"model":"m","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"D"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	applyPromptOverrides(in, nil, ruleWith("P", after), nil)
	if in.ResponsesInstructions == nil || *in.ResponsesInstructions != "P" || *in.Messages[1].Content.Content != "D" {
		t.Fatalf("instructions %v messages %#v", in.ResponsesInstructions, in.Messages[:2])
	}
}

// ---------- Anthropic: top-level system ----------

func suppressCloak(req *transformerModel.InternalLLMRequest) {
	req.TransformOptions.SuppressClaudeIdentity = true
}

func TestPromptAnthropicStringBeforeAfter(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":"  S  ","messages":[{"role":"user","content":"hi"}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{before: "P\nS", after: "S\nP"} {
		got := promptWire(t, inbound.InboundTypeAnthropic, body, outbound.OutboundTypeAnthropic, suppressCloak, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["system"], `[{"type":"text","text":`+mustJSON(t, want)+`}]`)
	}
	blank := `{"model":"m","max_tokens":10,"system":"","messages":[{"role":"user","content":"hi"}]}`
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeAnthropic, blank, outbound.OutboundTypeAnthropic, suppressCloak, nil, ruleWith("P", mode), nil)
		jsonEq(t, "blank "+string(mode), got.body["system"], `[{"type":"text","text":"P"}]`)
	}
}

func TestPromptAnthropicArrayBeforeAfter(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"B"}],"messages":[{"role":"user","content":"hi"}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{
		before: `[{"type":"text","text":"P"},{"type":"text","text":"A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"B"}]`,
		after:  `[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"B"},{"type":"text","text":"P"}]`,
	} {
		got := promptWire(t, inbound.InboundTypeAnthropic, body, outbound.OutboundTypeAnthropic, suppressCloak, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["system"], want)
	}
}

func TestPromptAnthropicNoSystemBothPositionsSetIt(t *testing.T) {
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeAnthropic, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			outbound.OutboundTypeAnthropic, suppressCloak, nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["system"], `[{"type":"text","text":"P"}]`)
	}
}

func TestPromptAnthropicOffIsByteIdentical(t *testing.T) {
	// cloak on (default) and off
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"A"}],"messages":[{"role":"user","content":"hi"}]}`
	assertOffIsByteIdentical(t, inbound.InboundTypeAnthropic, body, outbound.OutboundTypeAnthropic, nil)
	assertOffIsByteIdentical(t, inbound.InboundTypeAnthropic, body, outbound.OutboundTypeAnthropic, suppressCloak)
}

const claudeCLIBody = `{"model":"m","max_tokens":10,"system":[` +
	`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.999.abc; cc_entrypoint=cli; cch=00000;"},` +
	`{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."},` +
	`{"type":"text","text":"MAIN","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"hi"},{"role":"system","content":"ENV"}]}`

func withMidConversationBeta(req *transformerModel.InternalLLMRequest) {
	req.TransformOptions.AnthropicBetas = append(req.TransformOptions.AnthropicBetas, "mid-conversation-system-2026-04-07")
}

func billingBlocks(sys []any) int {
	n := 0
	for _, b := range sys {
		if strings.HasPrefix(b.(map[string]any)["text"].(string), "x-anthropic-billing-header:") {
			n++
		}
	}
	return n
}

// Claude CLI shape with the cloak on: the CLI's billing-header and identity blocks stay
// first and byte-identical; "before" goes right after them, "after" at the end of the
// system array; the outbound never synthesizes a second billing header; the
// mid-conversation system message stays in messages.
func TestPromptAnthropicClaudeCLIBlocks(t *testing.T) {
	base := promptWire(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, withMidConversationBeta, nil, nil, nil)
	baseSys := base.body["system"].([]any)
	if len(baseSys) != 3 {
		t.Fatalf("baseline system should be the CLI's own 3 blocks: %v", baseSys)
	}
	for mode, at := range map[dbmodel.PromptOverrideMode]int{before: 2, after: 3} {
		got := promptWire(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, withMidConversationBeta, nil, ruleWith("P", mode), nil)
		sys := got.body["system"].([]any)
		if len(sys) != 4 {
			t.Fatalf("%s: exactly one block must be added: %v", mode, sys)
		}
		jsonEq(t, string(mode)+" layer block", sys[at], `{"type":"text","text":"P"}`)
		rest := append(append([]any{}, sys[:at]...), sys[at+1:]...)
		if !reflect.DeepEqual(rest, baseSys) {
			t.Fatalf("%s: CLI blocks moved or changed:\n%v\n%v", mode, baseSys, rest)
		}
		if billingBlocks(sys) != 1 {
			t.Fatalf("%s: duplicate billing header: %v", mode, sys)
		}
		if !reflect.DeepEqual(got.body["messages"], base.body["messages"]) {
			t.Fatalf("%s: messages changed", mode)
		}
		if !reflect.DeepEqual(got.header, base.header) {
			t.Fatalf("%s: headers changed:\n%v\n%v", mode, base.header, got.header)
		}
	}
}

func TestPromptAnthropicClaudeCLIOffIsByteIdentical(t *testing.T) {
	assertOffIsByteIdentical(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, withMidConversationBeta)
}

func TestPromptAnthropicMidConversationSystemIsNotTopLevel(t *testing.T) {
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeAnthropic,
			`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"ENV"}]}`,
			outbound.OutboundTypeAnthropic, func(r *transformerModel.InternalLLMRequest) { suppressCloak(r); withMidConversationBeta(r) },
			nil, ruleWith("P", mode), nil)
		jsonEq(t, string(mode), got.body["system"], `[{"type":"text","text":"P"}]`)
		if !strings.Contains(mustJSON(t, got.body["messages"]), "ENV") {
			t.Fatalf("mid-conversation system dropped: %v", got.body["messages"])
		}
	}
}

// ---------- Gemini: systemInstruction ----------

func geminiSystemText(got map[string]any) string {
	si, ok := got["systemInstruction"].(map[string]any)
	if !ok {
		si, ok = got["system_instruction"].(map[string]any)
	}
	if !ok {
		return ""
	}
	var texts []string
	for _, p := range si["parts"].([]any) {
		if s, ok := p.(map[string]any)["text"].(string); ok {
			texts = append(texts, s)
		}
	}
	return strings.Join(texts, "|")
}

func TestPromptGeminiBeforeAfter(t *testing.T) {
	body := `{"systemInstruction":{"parts":[{"text":"S1"},{"text":"S2"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	for mode, want := range map[dbmodel.PromptOverrideMode]string{before: "P\nS1\nS2", after: "S1\nS2\nP"} {
		got := promptWire(t, inbound.InboundTypeGemini, body, outbound.OutboundTypeGemini, nil, nil, ruleWith("P", mode), nil)
		if s := geminiSystemText(got.body); s != want {
			t.Fatalf("%s: %q", mode, s)
		}
	}
	for _, mode := range []dbmodel.PromptOverrideMode{before, after} {
		got := promptWire(t, inbound.InboundTypeGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			outbound.OutboundTypeGemini, nil, nil, ruleWith("P", mode), nil)
		if s := geminiSystemText(got.body); s != "P" {
			t.Fatalf("none %s: %q", mode, s)
		}
	}
}

func TestPromptGeminiOffIsByteIdentical(t *testing.T) {
	assertOffIsByteIdentical(t, inbound.InboundTypeGemini,
		`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, outbound.OutboundTypeGemini, nil)
}

// ---------- layering ----------

func chatReq(system string) *transformerModel.InternalLLMRequest {
	user := "ping"
	msgs := []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &user}}}
	if system != "" {
		s := system
		msgs = append([]transformerModel.Message{{Role: "system", Content: transformerModel.MessageContent{Content: &s}}}, msgs...)
	}
	return &transformerModel.InternalLLMRequest{Model: "m", Messages: msgs}
}

func layers(planMode, ruleMode, chanMode dbmodel.PromptOverrideMode) (*dbmodel.AccessPlan, *dbmodel.AccessRouteRule, *dbmodel.Channel) {
	return &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "PLAN", PromptOverrideMode: planMode, PromptOverrideEnabled: true},
		&dbmodel.AccessRouteRule{RequestModel: "m", SystemPromptOverride: "ROUTE", PromptOverrideMode: ruleMode, PromptOverrideEnabled: true},
		&dbmodel.Channel{Name: "ch", SystemPromptOverride: "CHAN", PromptOverrideMode: chanMode, PromptOverrideEnabled: true}
}

func TestPromptLayerOrderSpecificLayersSitOutermost(t *testing.T) {
	cases := []struct {
		modes      [3]dbmodel.PromptOverrideMode
		client     string
		want, mode string
	}{
		{[3]dbmodel.PromptOverrideMode{before, before, before}, "S", "CHAN\nROUTE\nPLAN\nS", "before"},
		{[3]dbmodel.PromptOverrideMode{after, after, after}, "S", "S\nPLAN\nROUTE\nCHAN", "after"},
		{[3]dbmodel.PromptOverrideMode{before, after, before}, "S", "CHAN\nPLAN\nS\nROUTE", "before+after"},
		{[3]dbmodel.PromptOverrideMode{after, before, after}, "S", "ROUTE\nS\nPLAN\nCHAN", "before+after"},
		// no client system: the first layer becomes the system prompt, the rest stack on it
		{[3]dbmodel.PromptOverrideMode{before, after, before}, "", "CHAN\nPLAN\nROUTE", "before+after"},
	}
	for _, c := range cases {
		req := chatReq(c.client)
		plan, rule, ch := layers(c.modes[0], c.modes[1], c.modes[2])
		snap := applyPromptOverrides(req, plan, rule, ch)
		if got := *req.Messages[0].Content.Content; got != c.want || len(req.Messages) != 2 {
			t.Fatalf("%v client %q: got %q want %q", c.modes, c.client, got, c.want)
		}
		if string(snap.Mode) != c.mode || !reflect.DeepEqual(snap.Sources, []string{"access_plan:vip", "route_rule:m", "channel:ch"}) {
			t.Fatalf("%v: snapshot %#v", c.modes, snap)
		}
	}
}

func TestPromptLayerOrderAnthropicBlocks(t *testing.T) {
	plan, rule, ch := layers(before, after, after)
	plan2 := *plan
	got := promptWire(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, withMidConversationBeta, &plan2, rule, ch)
	var texts []string
	for _, b := range got.body["system"].([]any) {
		texts = append(texts, b.(map[string]any)["text"].(string))
	}
	want := []string{"x-anthropic-billing-header: cc_version=2.1.999.abc; cc_entrypoint=cli; cch=00000;",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.", "PLAN", "MAIN", "ROUTE", "CHAN"}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("got %q", texts)
	}
}

// admin_only 受众（旧四层 oracle 侧）：普通主体拿不到管理员层，管理员主体拿得到；
// 受众只影响自己那一层，其它层照常注入。这条基准由**独立 oracle** 算出，新规则表那边的
// 对应比对在 prompt_rules_test.go（TestPromptRulesAudienceAdminOnlyMatchesLegacyOracle）。
func TestPromptAudienceAdminOnlyOracle(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`
	plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "ADMIN-PLAN", PromptOverrideMode: before, PromptOverrideEnabled: true, PromptAudience: dbmodel.PromptAudienceAdminOnly}
	rule := ruleWith("OPEN", after)

	ordinary := promptWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, plan, rule, nil)
	jsonEq(t, "ordinary", ordinary.body["messages"], `[{"role":"system","content":"S\nOPEN"},{"role":"user","content":"hi"}]`)
	if !reflect.DeepEqual(ordinary.snap.Sources, []string{"route_rule:m"}) {
		t.Fatalf("ordinary sources %#v（admin_only 层必须被跳过）", ordinary.snap.Sources)
	}

	admin := promptWireForAudience(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, plan, rule, nil, true)
	jsonEq(t, "admin", admin.body["messages"], `[{"role":"system","content":"ADMIN-PLAN\nS\nOPEN"},{"role":"user","content":"hi"}]`)
	if !reflect.DeepEqual(admin.snap.Sources, []string{"access_plan:vip", "route_rule:m"}) {
		t.Fatalf("admin sources %#v", admin.snap.Sources)
	}
}

func TestPromptOffLayerIsSkippedOthersApply(t *testing.T) {
	req := chatReq("S")
	plan, rule, ch := layers(before, after, before)
	rule.PromptOverrideEnabled = false
	snap := applyPromptOverrides(req, plan, rule, ch)
	if *req.Messages[0].Content.Content != "CHAN\nPLAN\nS" || !reflect.DeepEqual(snap.Sources, []string{"access_plan:vip", "channel:ch"}) {
		t.Fatalf("%q %#v", *req.Messages[0].Content.Content, snap)
	}
}

func TestPromptLegacyModesCountAsOnBefore(t *testing.T) {
	for _, legacy := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeAppendSystem, dbmodel.PromptOverrideModeReplaceSystem} {
		req := chatReq("original")
		// unmigrated row: switch column false, legacy mode → behaves as on + before
		rule := &dbmodel.AccessRouteRule{RequestModel: "m", SystemPromptOverride: "replacement", PromptOverrideMode: legacy}
		snap := applyPromptOverrides(req, nil, rule, nil)
		if snap.Mode != before || *req.Messages[0].Content.Content != "replacement\noriginal" || len(req.Messages) != 2 {
			t.Fatalf("%s: %#v %#v", legacy, snap, req.Messages)
		}
	}
}

func TestPromptBlankTextIsIgnoredEvenWhenOn(t *testing.T) {
	req := chatReq("")
	snap := applyPromptOverrides(req, nil, ruleWith("   \n", before), nil)
	if len(snap.Sources) != 0 || len(req.Messages) != 1 {
		t.Fatalf("%#v", snap)
	}
}

func TestPromptGlobalLayerUsesSwitchAndPosition(t *testing.T) {
	setupRelayErrorDB(t)
	t.Cleanup(func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(before))
	})
	set := func(k dbmodel.SettingKey, v string) {
		t.Helper()
		if err := op.SettingSetString(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	set(dbmodel.SettingKeyPromptOverrideSystem, "GLOBAL")
	set(dbmodel.SettingKeyPromptOverrideMode, string(after))

	// default switch is off: nothing happens
	req := chatReq("S")
	if snap := applyPromptOverrides(req, nil, nil, nil); len(snap.Sources) != 0 || *req.Messages[0].Content.Content != "S" {
		t.Fatalf("global must be off by default: %#v", snap)
	}
	set(dbmodel.SettingKeyPromptOverrideEnabled, "true")
	req = chatReq("S")
	plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "PLAN", PromptOverrideMode: after, PromptOverrideEnabled: true}
	ch := &dbmodel.Channel{Name: "ch", SystemPromptOverride: "CHAN", PromptOverrideMode: before, PromptOverrideEnabled: true}
	snap := applyPromptOverrides(req, plan, nil, ch)
	if got := *req.Messages[0].Content.Content; got != "CHAN\nS\nGLOBAL\nPLAN" {
		t.Fatalf("got %q", got)
	}
	if !reflect.DeepEqual(snap.Sources, []string{"global", "access_plan:vip", "channel:ch"}) {
		t.Fatalf("%#v", snap)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "yes"); err == nil {
		t.Fatalf("switch setting must only accept true/false")
	}
}
