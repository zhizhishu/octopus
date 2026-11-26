package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/prompt"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	geminiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// promptRulesWire 用真实入站/出站转换跑新规则管道：仿照 promptWire（oracle 版），
// 断言新路径与旧四层在同一入站请求上的出站字节一致。规则直接入参（不查库）；
// promptRuleListFn 只在迁移 golden 里替换。
func promptRulesWire(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType,
	tweak func(*transformerModel.InternalLLMRequest), rules []dbmodel.PromptRule, scope prompt.Scope,
) ([]byte, http.Header, promptOverrideSnapshot) {
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
	snap := applyPromptRulesForAudience(req, rules, scope)
	if len(snap.Sources) > 0 {
		clearResponsesRawPromptShape(req)
	}
	httpReq, err := outbound.Get(out).TransformRequest(context.Background(), req, "https://upstream.invalid", "sk-test")
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	raw, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw, httpReq.Header.Clone(), snap
}

// outboundWire 把内部请求跑真实出站转换并返回 wire body（不联网，无上游请求）。
func outboundWire(t *testing.T, req *transformerModel.InternalLLMRequest, out outbound.OutboundType) []byte {
	t.Helper()
	httpReq, err := outbound.Get(out).TransformRequest(context.Background(), req, "https://upstream.invalid", "sk-test")
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	raw, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw
}

// legacyInboundTransform 只跑入站解析（与 promptRulesWire 同一条入站路径），
// 供旧四层 oracle 在同一份入站请求上叠加旧层。
func legacyInboundTransform(t *testing.T, in inbound.InboundType, body string, tweak func(*transformerModel.InternalLLMRequest)) *transformerModel.InternalLLMRequest {
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
	return req
}

// legacy16ComboWire 装配旧四层入参（global 走设置缓存，其余用对象字段）并跑 oracle 出站。
func legacy16ComboWire(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType, tweak func(*transformerModel.InternalLLMRequest), gPos, planPos, routePos, chanPos dbmodel.PromptOverrideMode) []byte {
	t.Helper()
	set := func(k dbmodel.SettingKey, v string) {
		t.Helper()
		if err := op.SettingSetString(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	set(dbmodel.SettingKeyPromptOverrideSystem, "GLOBAL")
	set(dbmodel.SettingKeyPromptOverrideMode, string(gPos))
	set(dbmodel.SettingKeyPromptOverrideEnabled, "true")
	plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "PLAN", PromptOverrideMode: planPos, PromptOverrideEnabled: true}
	route := &dbmodel.AccessRouteRule{RequestModel: "m", SystemPromptOverride: "ROUTE", PromptOverrideMode: routePos, PromptOverrideEnabled: true}
	channel := &dbmodel.Channel{Name: "ch", SystemPromptOverride: "CHAN", PromptOverrideMode: chanPos, PromptOverrideEnabled: true}
	req := legacyInboundTransform(t, in, body, tweak)
	snap := applyPromptOverridesForAudience(req, plan, route, channel, false)
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
	return raw
}

// firstWireDiff 只用于逐字节比对失败时：给出**首个**不同字节的下标与两边上下文（含
// system 消息结构/header 级 JSON 的原始字节），让「哪一个字节开始走样」一眼可读。
func firstWireDiff(legacy, rules []byte) string {
	shared := len(legacy)
	if len(rules) < shared {
		shared = len(rules)
	}
	clip := func(b []byte, from, to int) []byte {
		if to > len(b) {
			to = len(b)
		}
		if from > to {
			from = to
		}
		return b[from:to]
	}
	for i := 0; i < shared; i++ {
		if legacy[i] == rules[i] {
			continue
		}
		from := i - 40
		if from < 0 {
			from = 0
		}
		return fmt.Sprintf("first diff at byte %d%s\n legacy[%d:] %q\n rules [%d:] %q",
			i, wireLengthNote(legacy, rules), from, clip(legacy, from, i+40), from, clip(rules, from, i+40))
	}
	if len(legacy) == len(rules) {
		return "wire is identical"
	}
	return fmt.Sprintf("no value diff in the first %d bytes%s", shared, wireLengthNote(legacy, rules))
}

func wireLengthNote(legacy, rules []byte) string {
	if len(legacy) == len(rules) {
		return ""
	}
	return fmt.Sprintf(" (length legacy=%d rules=%d)", len(legacy), len(rules))
}

func positionOfMode(m dbmodel.PromptOverrideMode) dbmodel.PromptRulePosition {
	if m == dbmodel.PromptOverrideModeAfter {
		return dbmodel.PromptRulePositionAfter
	}
	return dbmodel.PromptRulePositionBefore
}

// rulesFrom16Combo 与旧四层一一对应：global/plan/model(旧 route)/channel，
// 同一文本、同一位置、按最高受限维度定层级。作用域写成迁移产物（migration 031）的形状：
// route 规则是 plan_mode=selected + 该方案 ID（旧路由归属于某个方案），不是 all；
// 这样 16 组合矩阵验的就是迁移真实形状，而不是手拼的宽范围。
func rulesFrom16Combo(gPos, planPos, routePos, chanPos dbmodel.PromptOverrideMode, reqModel string) []dbmodel.PromptRule {
	return []dbmodel.PromptRule{
		{ID: 1, Name: "global", Enabled: true, Text: "GLOBAL", Position: positionOfMode(gPos), PlanMode: dbmodel.PromptRulePlanModeAll, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeAll},
		{ID: 2, Name: "plan:vip", Enabled: true, Text: "PLAN", Position: positionOfMode(planPos), PlanMode: dbmodel.PromptRulePlanModeSelected, PlanIDs: []int{1}, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeAll},
		{ID: 3, Name: "route:m", Enabled: true, Text: "ROUTE", Position: positionOfMode(routePos), PlanMode: dbmodel.PromptRulePlanModeSelected, PlanIDs: []int{1}, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeSelected, Models: []string{reqModel}},
		{ID: 4, Name: "channel:ch", Enabled: true, Text: "CHAN", Position: positionOfMode(chanPos), PlanMode: dbmodel.PromptRulePlanModeAll, ChannelMode: dbmodel.PromptRuleChannelModeInclude, ChannelIDs: []int{7}, ModelMode: dbmodel.PromptRuleModelModeAll},
	}
}

var prompt16ComboBodies = map[inbound.InboundType]struct {
	in      inbound.InboundType
	out     outbound.OutboundType
	body    string
	bodyNoS string
	tweak   func(*transformerModel.InternalLLMRequest)
}{
	inbound.InboundTypeOpenAIChat: {in: inbound.InboundTypeOpenAIChat, out: outbound.OutboundTypeOpenAIChat,
		body:    `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`,
		bodyNoS: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	inbound.InboundTypeOpenAIResponse: {in: inbound.InboundTypeOpenAIResponse, out: outbound.OutboundTypeOpenAIResponse,
		body:    `{"model":"m","instructions":"I","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		bodyNoS: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
	inbound.InboundTypeAnthropic: {in: inbound.InboundTypeAnthropic, out: outbound.OutboundTypeAnthropic,
		body:    `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[{"role":"user","content":"hi"}]}`,
		bodyNoS: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		tweak:   func(r *transformerModel.InternalLLMRequest) { r.TransformOptions.SuppressClaudeIdentity = true }},
}

// promptComboInName 是失败标签用的协议名：inbound.InboundType 是 int 枚举，
// 直接 string(combo.in) 会被 go test 的 vet(stringintconv) 判成单 rune 转换。
var promptComboInName = map[inbound.InboundType]string{
	inbound.InboundTypeOpenAIChat:     "chat",
	inbound.InboundTypeOpenAIResponse: "responses",
	inbound.InboundTypeAnthropic:      "anthropic",
}

// 16 种前后组合 × 4 协议（chat / responses / anthropic / gemini）× 有无客户端 system：
// 新规则管道与旧四层**独立 oracle**（prompt_override_test.go 里 a7fd2e93 的自包含副本）
// 的出站 wire 字节必须逐字节一致；不一致时报首个不同字节（firstWireDiff）。
func TestPromptRulesWireSixteenCombosMatchLegacyOracle(t *testing.T) {
	setupRelayErrorDB(t)
	cleanup := func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore))
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, combo := range prompt16ComboBodies {
		for _, withSystem := range []bool{true, false} {
			body := combo.body
			if !withSystem {
				body = combo.bodyNoS
			}
			for _, g := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
				for _, p := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
					for _, r := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
						for _, c := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
							label := promptComboInName[combo.in] + "/" + map[bool]string{true: "S", false: "noS"}[withSystem] +
								"/" + string(g) + string(p) + string(r) + string(c)
							want := legacy16ComboWire(t, combo.in, body, combo.out, combo.tweak, g, p, r, c)
							rules := rulesFrom16Combo(g, p, r, c, "m")
							scope := prompt.Scope{PlanID: 1, RequestModel: "m", ChannelID: 7}
							got, _, _ := promptRulesWire(t, combo.in, body, combo.out, combo.tweak, rules, scope)
							if !bytes.Equal(want, got) {
								t.Fatalf("%s: legacy != rules wire\n%s\nlegacy: %s\nrules:  %s", label, firstWireDiff(want, got), want, got)
							}
						}
					}
				}
			}
		}
	}
}

// Gemini 入站与出站单独跑完 16 组合（ctx 需要带 gemini 请求选项）。
func TestPromptRulesWireGeminiSixteenCombosMatchLegacyOracle(t *testing.T) {
	setupRelayErrorDB(t)
	cleanup := func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore))
	}
	cleanup()
	t.Cleanup(cleanup)

	const withS = `{"model":"gemini-x","systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	const noS = `{"model":"gemini-x","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	for _, withSystem := range []bool{true, false} {
		for _, g := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
			for _, p := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
				for _, r := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
					for _, c := range []dbmodel.PromptOverrideMode{dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter} {
						body := withS
						if !withSystem {
							body = noS
						}
						want := legacy16ComboWire(t, inbound.InboundTypeGemini, body, outbound.OutboundTypeGemini, nil, g, p, r, c)
						rules := rulesFrom16Combo(g, p, r, c, "gemini-x")
						scope := prompt.Scope{PlanID: 1, RequestModel: "gemini-x", ChannelID: 7}
						got, _, _ := promptRulesWire(t, inbound.InboundTypeGemini, body, outbound.OutboundTypeGemini, nil, rules, scope)
						if !bytes.Equal(want, got) {
							label := string(g) + string(p) + string(r) + string(c)
							t.Fatalf("gemini %s: legacy != rules wire\n%s\nlegacy: %s\nrules:  %s", label, firstWireDiff(want, got), want, got)
						}
					}
				}
			}
		}
	}
}

// admin_only 受众：普通 key 不注入；管理员 key 注入（与旧四层受众语义一致）。
func TestPromptRulesAudienceAdminOnly(t *testing.T) {
	adminRule := basePromptRule(1, "adm", true, "ADMIN-ONLY", dbmodel.PromptRulePositionBefore)
	adminRule.Audience = dbmodel.PromptAudienceAdminOnly
	ordinary := basePromptRule(2, "ord", true, "ORD", dbmodel.PromptRulePositionBefore)
	rules := []dbmodel.PromptRule{adminRule, ordinary}
	body := `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`

	ordinaryRaw, _, snap := promptRulesWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, rules, prompt.Scope{RequestModel: "m"})
	if strings.Contains(string(ordinaryRaw), "ADMIN-ONLY") || len(snap.Sources) != 1 || snap.Sources[0] != "ord" {
		t.Fatalf("admin-only leaked to ordinary user: %s %#v", ordinaryRaw, snap)
	}
	adminRaw, _, snapA := promptRulesWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, rules, prompt.Scope{RequestModel: "m", IsAdmin: true})
	if !strings.Contains(string(adminRaw), "ADMIN-ONLY") || len(snapA.Sources) != 2 {
		t.Fatalf("admin should get both rules: %s %#v", adminRaw, snapA)
	}
}

// true Claude CLI 出站：计费头与身份块保持在位，规则块插在它们之后，不产生第二计费头；
// 工具与 mid-conversation system（ENV）等非目标内容一律不动。
//
// 顺序先按实现核过：internal/prompt/proto.go 的 anthropicSlot 把 before 位置定在
// billing / agent-identity 两块之后，Apply 按 tier→sort→id 应用。本用例里 MID 是全范围
// 规则（tier 0）、P 是渠道规则（tier 3），所以 MID 先落地（before 插在 identity 后 /
// after 追加到末尾），P 再前插到 billing+identity 之后：
//
//	before: billing, SDK, P, MID, MAIN
//	after : billing, SDK, P, MAIN, MID
func TestPromptRulesClaudeCLIBlocksPreserved(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.999.abc; cc_entrypoint=cli; cch=00000;"
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	const claudeCLIBody = `{"model":"m","max_tokens":10,` +
		`"tools":[{"name":"get_weather","description":"look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],` +
		`"system":[` +
		`{"type":"text","text":"` + billing + `"},` +
		`{"type":"text","text":"` + identity + `"},` +
		`{"type":"text","text":"MAIN","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"hi"},{"role":"system","content":"ENV"}]}`
	tweak := func(r *transformerModel.InternalLLMRequest) {
		r.TransformOptions.SuppressClaudeIdentity = true
		r.TransformOptions.AnthropicBetas = append(r.TransformOptions.AnthropicBetas, "mid-conversation-system-2026-04-07")
	}

	// 基准：一条规则都不加，拿到 CLI 自己的 3 块 + 原样透传的工具/消息，后面逐项对比。
	baseRaw, _, baseSnap := promptRulesWire(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, tweak, nil, prompt.Scope{RequestModel: "m", ChannelID: 7})
	if len(baseSnap.Sources) != 0 {
		t.Fatalf("baseline must inject nothing: %#v", baseSnap.Sources)
	}
	base := decodePromptJSON(t, baseRaw)
	if baseSys, ok := base["system"].([]any); !ok || len(baseSys) != 3 {
		t.Fatalf("baseline system should be the CLI's own 3 blocks: %v", base["system"])
	}
	baseTools, ok := base["tools"].([]any)
	if !ok || len(baseTools) != 1 || baseTools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("baseline dropped the client tool: %v", base["tools"])
	}

	chRule := basePromptRule(1, "ch", true, "P", dbmodel.PromptRulePositionBefore)
	chRule.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	chRule.ChannelIDs = []int{7}
	for _, pos := range []dbmodel.PromptRulePosition{dbmodel.PromptRulePositionBefore, dbmodel.PromptRulePositionAfter} {
		mid := basePromptRule(2, "mid", true, "MID", pos)
		want := []string{billing, identity, "P", "MID", "MAIN"}
		if pos == dbmodel.PromptRulePositionAfter {
			want = []string{billing, identity, "P", "MAIN", "MID"}
		}
		raw, _, snap := promptRulesWire(t, inbound.InboundTypeAnthropic, claudeCLIBody, outbound.OutboundTypeAnthropic, tweak,
			[]dbmodel.PromptRule{mid, chRule}, prompt.Scope{RequestModel: "m", ChannelID: 7})
		body := decodePromptJSON(t, raw)
		sys, ok := body["system"].([]any)
		if !ok || len(sys) != len(want) {
			t.Fatalf("%s: want %d system blocks, got %v", pos, len(want), body["system"])
		}
		got := make([]string, 0, len(sys))
		billingBlocks, mainAt := 0, -1
		for i, blk := range sys {
			text, _ := blk.(map[string]any)["text"].(string)
			got = append(got, text)
			if strings.HasPrefix(text, "x-anthropic-billing-header:") {
				billingBlocks++
			}
			if text == "MAIN" {
				mainAt = i
			}
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("%s: system blocks\n got  %q\n want %q", pos, got, want)
		}
		if billingBlocks != 1 {
			t.Fatalf("%s: billing header must appear exactly once: %q", pos, got)
		}
		if mainAt < 0 {
			t.Fatalf("%s: MAIN block disappeared: %q", pos, got)
		}
		if cc, ok := sys[mainAt].(map[string]any)["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
			t.Fatalf("%s: MAIN cache_control lost: %v", pos, sys[mainAt])
		}
		// 工具与 ENV（mid-conversation system）等非目标内容与无规则基准逐字节一致。
		jsonEq(t, string(pos)+" tools", body["tools"], mustJSON(t, base["tools"]))
		jsonEq(t, string(pos)+" messages", body["messages"], mustJSON(t, base["messages"]))
		if len(snap.Sources) != 2 {
			t.Fatalf("%s: sources = %#v", pos, snap.Sources)
		}
	}
}

// 多候选不累积：只解析一次入站请求，captureAttemptBase 留原始模板，然后在同一个 req 上做
// 两轮「restore + 注入」（这就是 relay 每个候选实际做的事），两轮出站字节必须一致，且系统
// 提示词里只出现一次 P。两次各自重新解析入站请求会掩盖累积，所以这里不能重新 parse。
func TestPromptRulesAcrossCandidatesDoNotAccumulate(t *testing.T) {
	rules := []dbmodel.PromptRule{basePromptRule(1, "g", true, "P", dbmodel.PromptRulePositionBefore)}
	const body = `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`
	scope := prompt.Scope{RequestModel: "m"}
	req := legacyInboundTransform(t, inbound.InboundTypeOpenAIChat, body, nil)
	base := captureAttemptBase(req)

	applyOnce := func() ([]byte, promptOverrideSnapshot) {
		base.restore(req, "m")
		snap := applyPromptRulesForAudience(req, rules, scope)
		if len(snap.Sources) > 0 {
			clearResponsesRawPromptShape(req)
		}
		return outboundWire(t, req, outbound.OutboundTypeOpenAIChat), snap
	}
	one, snapOne := applyOnce()
	two, snapTwo := applyOnce()
	if len(one) == 0 || !bytes.Equal(one, two) {
		t.Fatalf("second candidate diverged:\n%s\n%s", one, two)
	}
	for _, snap := range []promptOverrideSnapshot{snapOne, snapTwo} {
		if len(snap.Sources) != 1 || snap.Sources[0] != "g" {
			t.Fatalf("each candidate must apply the rule exactly once: %#v", snap)
		}
	}
	msgs, ok := decodePromptJSON(t, one)["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("unexpected messages: %s", one)
	}
	if got := msgs[0].(map[string]any)["content"]; got != "P\nS" {
		t.Fatalf("system content = %v, want %q（规则只应加一次）", got, "P\nS")
	}
	if count := strings.Count(string(one), `P\nS`); count != 1 {
		t.Fatalf("rule text must appear exactly once in the wire body, got %d: %s", count, one)
	}
}

// 规则匹配用原始请求模型（scope.RequestModel），即使 req.Model 已被候选映射改写。
func TestPromptRulesMatchOriginalRequestModelNotMapped(t *testing.T) {
	rule := basePromptRule(1, "by-model", true, "P", dbmodel.PromptRulePositionBefore)
	rule.ModelMode = dbmodel.PromptRuleModelModeSelected
	rule.Models = []string{"req-model"}
	body := `{"model":"req-model","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`
	mapped := func(r *transformerModel.InternalLLMRequest) { r.Model = "mapped-upstream" } // 候选映射后 req.Model
	raw, _, snap := promptRulesWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, mapped,
		[]dbmodel.PromptRule{rule}, prompt.Scope{RequestModel: "req-model", ChannelID: 7})
	if !strings.Contains(string(raw), "P") || len(snap.Sources) != 1 {
		t.Fatalf("rule must match the ORIGINAL request model despite mapping: %s %#v", raw, snap)
	}
	// 换一个不相关原始模型则不命中。
	rule2 := basePromptRule(1, "by-model", true, "P", dbmodel.PromptRulePositionBefore)
	rule2.ModelMode = dbmodel.PromptRuleModelModeSelected
	rule2.Models = []string{"other-model"}
	raw2, _, snap2 := promptRulesWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, mapped,
		[]dbmodel.PromptRule{rule2}, prompt.Scope{RequestModel: "req-model", ChannelID: 7})
	if strings.Contains(string(raw2), "\n\"P\"") || len(snap2.Sources) != 0 {
		t.Fatalf("unrelated model rule must not fire: %s %#v", raw2, snap2)
	}
}

// legacyAudienceWire 用**独立 oracle**（旧四层，prompt_override_test.go 里的 a7fd2e93 副本）
// 出站：受众由 admin 决定，给 admin_only 当基准字节。
func legacyAudienceWire(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType,
	tweak func(*transformerModel.InternalLLMRequest), plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel,
	admin bool,
) []byte {
	t.Helper()
	req := legacyInboundTransform(t, in, body, tweak)
	snap := applyPromptOverridesForAudience(req, plan, rule, channel, admin)
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
	return raw
}

// admin_only 受众逐字节比对：新规则表与旧四层 oracle 在普通主体 / 管理员主体上出站字节
// 必须一致（普通主体只拿 OPEN-CHAN，管理员主体额外拿 ADMIN-PLAN）。
func TestPromptRulesAudienceAdminOnlyMatchesLegacyOracle(t *testing.T) {
	setupRelayErrorDB(t)
	cleanup := func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore))
	}
	cleanup()
	t.Cleanup(cleanup)

	const body = `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`
	plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "ADMIN-PLAN", PromptOverrideMode: dbmodel.PromptOverrideModeBefore,
		PromptOverrideEnabled: true, PromptAudience: dbmodel.PromptAudienceAdminOnly}
	channel := &dbmodel.Channel{Name: "ch", SystemPromptOverride: "OPEN-CHAN", PromptOverrideMode: dbmodel.PromptOverrideModeAfter, PromptOverrideEnabled: true}

	planRule := basePromptRule(1, "plan:vip", true, "ADMIN-PLAN", dbmodel.PromptRulePositionBefore)
	planRule.Audience = dbmodel.PromptAudienceAdminOnly
	planRule.PlanMode = dbmodel.PromptRulePlanModeSelected
	planRule.PlanIDs = []int{1, 9}
	chanRule := basePromptRule(2, "channel:ch", true, "OPEN-CHAN", dbmodel.PromptRulePositionAfter)
	chanRule.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	chanRule.ChannelIDs = []int{7}
	rules := []dbmodel.PromptRule{planRule, chanRule}

	for _, admin := range []bool{false, true} {
		want := legacyAudienceWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, plan, nil, channel, admin)
		got, _, snap := promptRulesWire(t, inbound.InboundTypeOpenAIChat, body, outbound.OutboundTypeOpenAIChat, nil, rules,
			prompt.Scope{PlanID: 1, RequestModel: "m", ChannelID: 7, IsAdmin: admin})
		if !bytes.Equal(want, got) {
			t.Fatalf("admin=%v: legacy != rules wire\n%s\nlegacy: %s\nrules:  %s", admin, firstWireDiff(want, got), want, got)
		}
		wantSources := 1
		if admin {
			wantSources = 2
		}
		if len(snap.Sources) != wantSources {
			t.Fatalf("admin=%v sources = %#v (want %d)", admin, snap.Sources, wantSources)
		}
	}
}

// 竞速候选复用请求级快照：buildGroupRaceContenders 拿到快照时零回表；快照为 nil（入口
// 读取失败）时也零回表且照常组候选；只有完全没传快照的直接调用者才在这里读一次。不管
// 哪种情况，候选模板只注入一次（不累积、不双重注入）。
func TestGroupRaceReusesRequestLevelPromptSnapshot(t *testing.T) {
	restore := promptRuleListFn
	t.Cleanup(func() { promptRuleListFn = restore })
	reads := 0
	promptRuleListFn = func(context.Context) ([]dbmodel.PromptRule, error) {
		reads++
		return nil, op.ErrPromptRuleStoreUnavailable
	}

	const channelID = 9301
	channel := &dbmodel.Channel{ID: channelID, Name: "race-rules", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, RaceMode: true, DisableCircuitBreaker: true}
	item := dbmodel.GroupItem{ChannelID: channelID, ModelName: "m"}
	keys := []dbmodel.ChannelKey{{ID: 93011, ChannelID: channelID, Enabled: true, ChannelKey: "k"}}
	newRequest := func() *relayRequest { return &relayRequest{internalRequest: chatReq("S"), requestModel: "m"} }
	rule := basePromptRule(1, "g", true, "P", dbmodel.PromptRulePositionBefore)

	contenders, _ := buildGroupRaceContenders(context.Background(), newRequest(), item, channel, keys, routeGroupResult{}, "chat", []dbmodel.PromptRule{rule})
	if reads != 0 {
		t.Fatalf("传入请求级快照时不得再读规则表，reads=%d", reads)
	}
	if len(contenders) != 1 {
		t.Fatalf("contenders = %d, want 1", len(contenders))
	}
	if got := *contenders[0].internalRequest.Messages[0].Content.Content; got != "P\nS" {
		t.Fatalf("竞速模板必须只注入一次，got %q", got)
	}

	contenders, _ = buildGroupRaceContenders(context.Background(), newRequest(), item, channel, keys, routeGroupResult{}, "chat", []dbmodel.PromptRule(nil))
	if reads != 0 {
		t.Fatalf("nil 快照（读取失败）也必须零回表，reads=%d", reads)
	}
	if len(contenders) != 1 {
		t.Fatalf("nil 快照必须照常组候选（无规则继续），contenders=%d", len(contenders))
	}
	if got := *contenders[0].internalRequest.Messages[0].Content.Content; got != "S" {
		t.Fatalf("无规则时不得注入，got %q", got)
	}

	contenders, _ = buildGroupRaceContenders(context.Background(), newRequest(), item, channel, keys, routeGroupResult{}, "chat")
	if reads != 1 {
		t.Fatalf("没传快照的直接调用者只应读一次，reads=%d", reads)
	}
	if len(contenders) != 1 {
		t.Fatalf("读取失败必须当作无规则继续竞速，contenders=%d", len(contenders))
	}
}

// store 读失败 = 本请求没有规则：请求照常转发（不 500、不报错）、注入文本为空、也绝不
// 回落旧四层字段（下面把旧全局层全打开当回归防线）。对照组证明同一套 harness 能看见注入，
// 所以「空」不是假阴性。
func TestPromptRulesStoreErrorForwardsWithoutRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	for k, v := range map[dbmodel.SettingKey]string{
		dbmodel.SettingKeyPromptOverrideSystem:  "LEGACY-GLOBAL",
		dbmodel.SettingKeyPromptOverrideMode:    string(dbmodel.PromptOverrideModeBefore),
		dbmodel.SettingKeyPromptOverrideEnabled: "true",
	} {
		if err := op.SettingSetString(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	t.Cleanup(func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore))
	})

	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-rules","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:         "rules-store-error",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		Cloak:        dbmodel.ChannelCloak{Mode: "never"},
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		Model:        "upstream-model",
		ModelMapping: map[string]string{"rules-store-model": "upstream-model"},
		Priority:     1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	restore := promptRuleListFn
	t.Cleanup(func() { promptRuleListFn = restore })

	const requestBody = `{"model":"rules-store-model","messages":[{"role":"system","content":"CLIENT"},{"role":"user","content":"ping"}]}`
	send := func() (int, string, []string) {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIChat, c)
		mu.Lock()
		sent := append([]string(nil), bodies...)
		mu.Unlock()
		return rec.Code, rec.Body.String(), sent
	}
	systemText := func(t *testing.T, raw string) string {
		t.Helper()
		var sent transformerModel.InternalLLMRequest
		if err := json.Unmarshal([]byte(raw), &sent); err != nil {
			t.Fatalf("decode upstream body: %v (%s)", err, raw)
		}
		if len(sent.Messages) == 0 {
			t.Fatalf("upstream body has no messages: %s", raw)
		}
		if !strings.EqualFold(sent.Messages[0].Role, "system") || sent.Messages[0].Content.Content == nil {
			t.Fatalf("upstream body lost the client system message: %s", raw)
		}
		return *sent.Messages[0].Content.Content
	}

	t.Run("store error forwards with no rules", func(t *testing.T) {
		promptRuleListFn = func(context.Context) ([]dbmodel.PromptRule, error) {
			return nil, op.ErrPromptRuleStoreUnavailable
		}
		code, body, sent := send()
		if code == http.StatusInternalServerError {
			t.Fatalf("store error must not abort the request with a 500: %d %s", code, body)
		}
		if code != http.StatusOK {
			t.Fatalf("store error must still forward the request: %d %s", code, body)
		}
		if strings.Contains(body, "prompt_rules_unavailable") || strings.Contains(body, "prompt rule store") {
			t.Fatalf("client must not see a prompt-rules error: %s", body)
		}
		if !strings.Contains(body, "chatcmpl-rules") {
			t.Fatalf("response must carry the upstream payload after a real forward: %s", body)
		}
		if len(sent) != 1 {
			t.Fatalf("upstream hits = %d, want 1", len(sent))
		}
		if got := systemText(t, sent[0]); got != "CLIENT" {
			t.Fatalf("injected text must be empty (client system only), got %q in %s", got, sent[0])
		}
		if strings.Contains(sent[0], "LEGACY-GLOBAL") {
			t.Fatalf("must never fall back to the legacy four layers: %s", sent[0])
		}
	})

	t.Run("control: a rule is injected exactly once", func(t *testing.T) {
		promptRuleListFn = func(context.Context) ([]dbmodel.PromptRule, error) {
			return []dbmodel.PromptRule{basePromptRule(1, "g", true, "RULE-TEXT", dbmodel.PromptRulePositionBefore)}, nil
		}
		code, body, sent := send()
		if code != http.StatusOK {
			t.Fatalf("request failed: %d %s", code, body)
		}
		if !strings.Contains(body, "chatcmpl-rules") {
			t.Fatalf("response must carry the upstream payload: %s", body)
		}
		if len(sent) != 1 {
			t.Fatalf("upstream hits = %d, want 1", len(sent))
		}
		if got := systemText(t, sent[0]); got != "RULE-TEXT\nCLIENT" {
			t.Fatalf("rule must be injected exactly once before the client system prompt, got %q", got)
		}
		if strings.Contains(sent[0], "LEGACY-GLOBAL") {
			t.Fatalf("legacy layers must stay dead: %s", sent[0])
		}
	})
}

// 接线静态断言（模型检测静态断言风格）：生产 relay.go / race_group.go 必须走
// applyPromptRulesForAudience + 请求级规则快照，不得再调用旧入口；oracle 文件必须自包含。
func TestPromptRulesWiringStatic(t *testing.T) {
	relaySrc, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatalf("read relay.go: %v", err)
	}
	raceSrc, err := os.ReadFile("race_group.go")
	if err != nil {
		t.Fatalf("read race_group.go: %v", err)
	}
	if !strings.Contains(string(relaySrc), "applyPromptRulesForAudience(internalRequest, promptRules") {
		t.Fatal("relay.go 串行候选必须调用 applyPromptRulesForAudience ")
	}
	// 每请求只读一次：Handler 入口读一次，并把这份快照递给竞速候选。
	if !strings.Contains(string(relaySrc), "loadPromptRulesOrNone(") {
		t.Fatal("relay.go 必须在 Handler 入口读一次规则表（loadPromptRulesOrNone）")
	}
	if !strings.Contains(string(relaySrc), "requestEndpoint, promptRules)") {
		t.Fatal("relay.go 必须把请求级快照传给 buildGroupRaceContenders，禁止竞速路径逐候选回表")
	}
	// 读失败 = 无规则继续：旧 fail-closed 的 500 通道必须消失。
	for _, banned := range []string{"prompt_rules_unavailable", "prompt rule store unavailable"} {
		if strings.Contains(string(relaySrc), banned) {
			t.Fatalf("relay.go 不得再把规则表读取失败变成客户端错误（出现 %q）", banned)
		}
	}
	if !strings.Contains(string(raceSrc), "applyPromptRulesForAudience(template, promptRules") {
		t.Fatal("race_group.go 竞速候选必须用请求级快照调用 applyPromptRulesForAudience")
	}
	if strings.Contains(string(raceSrc), "promptRuleListFn(") {
		t.Fatal("race_group.go 不得逐候选读规则表（只允许 loadPromptRulesOrNone 的兜底读取）")
	}
	for name, src := range map[string]string{"relay.go": string(relaySrc), "race_group.go": string(raceSrc)} {
		if strings.Contains(src, "applyPromptOverridesForAudience(") {
			t.Fatalf("%s 生产路径仍引用旧 oracle 函数", name)
		}
	}
	// 生产文件里不得保留旧四层拼接头（拼接核心只在 internal/prompt 与新 oracle）。
	poSrc, err := os.ReadFile("prompt_override.go")
	if err != nil {
		t.Fatalf("read prompt_override.go: %v", err)
	}
	for _, banned := range []string{"promptSlotFor", "anthropicPromptSlot", "applyPromptOverrideText", "promptOverrideLayersForAudience"} {
		if strings.Contains(string(poSrc), banned) {
			t.Fatalf("prompt_override.go 生产文件出现旧函数 %q（应只存在于 oracle）", banned)
		}
	}
	if !strings.Contains(string(poSrc), "loadPromptRulesOrNone") {
		t.Fatal("prompt_override.go 必须提供请求级读取入口 loadPromptRulesOrNone")
	}

	// oracle 独立性：prompt_override_test.go 不得 import/call internal/prompt 的新实现，
	// 也不得复用 model 层的开关/受众/模式归一判定。
	oracleSrc, err := os.ReadFile("prompt_override_test.go")
	if err != nil {
		t.Fatalf("read prompt_override_test.go: %v", err)
	}
	for _, banned := range []string{
		`"github.com/zhizhishu/octopus/internal/prompt"`,
		"prompt.Evaluate(", "prompt.Apply(", "prompt.ApplyText(", "prompt.SlotFor(", "prompt.InsertSystemMessage(", "prompt.Preview(",
		"dbmodel.PromptOverrideActive(", "dbmodel.NormalizePromptOverrideMode(", "dbmodel.PromptAudienceAllows(",
	} {
		if strings.Contains(string(oracleSrc), banned) {
			t.Fatalf("oracle 不再独立：prompt_override_test.go 出现共享实现调用 %q", banned)
		}
	}
	// 旧拼接实现必须在 oracle 文件里自有一份（四层遍历 + per-协议 slot + 镜像 after）。
	for _, want := range []string{
		"func promptOverrideLayersForAudience(",
		"func oraclePromptSlotFor(",
		"func oracleAnthropicPromptSlot(",
		"func oracleApplyPromptOverrideText(",
		"func oracleInsertSystemMessage(",
		"func oraclePromptOverrideActive(",
	} {
		if !strings.Contains(string(oracleSrc), want) {
			t.Fatalf("oracle 缺少 a7fd2e93 拼接实现的一部分：%q", want)
		}
	}
}

func basePromptRule(id int, name string, on bool, text string, pos dbmodel.PromptRulePosition) dbmodel.PromptRule {
	return dbmodel.PromptRule{ID: id, Name: name, Enabled: on, Text: text, Position: pos,
		PlanMode: dbmodel.PromptRulePlanModeAll, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeAll}
}

func decodePromptJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode outbound body: %v", err)
	}
	return m
}
