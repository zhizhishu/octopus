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

type promptResult struct {
	body   map[string]any
	raw    []byte
	header http.Header
	snap   promptOverrideSnapshot
}

// promptWire parses body with the inbound, applies the layers exactly like relay.go does
// (applyPromptOverrides, then clearResponsesRawPromptShape when a layer applied), runs the
// real outbound and returns the upstream body/headers. No network.
func promptWire(t *testing.T, in inbound.InboundType, body string, out outbound.OutboundType,
	tweak func(*transformerModel.InternalLLMRequest), plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel,
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
	snap := applyPromptOverrides(req, plan, rule, channel)
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
