package prompt

import (
	"reflect"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// promptRuleTestReq 构造与 relay chatReq 同构的内部请求（OpenAI chat 语义）。
func promptRuleTestReq(system string) *transformerModel.InternalLLMRequest {
	user := "ping"
	msgs := []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &user}}}
	if system != "" {
		s := system
		msgs = append([]transformerModel.Message{{Role: "system", Content: transformerModel.MessageContent{Content: &s}}}, msgs...)
	}
	req := &transformerModel.InternalLLMRequest{Model: "m", Messages: msgs}
	req.RawAPIFormat = transformerModel.APIFormatOpenAIChatCompletion
	return req
}

func baseRule(id int, name string, on bool, text string, pos dbmodel.PromptRulePosition) dbmodel.PromptRule {
	return dbmodel.PromptRule{ID: id, Name: name, Enabled: on, Text: text, Position: pos,
		PlanMode: dbmodel.PromptRulePlanModeAll, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeAll}
}

func TestEvaluateOrdersByTierThenSortThenID(t *testing.T) {
	// struct literal 不能混写位置元素与 keyed 字段：先用局部变量取 baseRule 再改作用域。
	planRule := baseRule(5, "plan", true, "P", dbmodel.PromptRulePositionBefore)
	planRule.PlanMode = dbmodel.PromptRulePlanModeSelected
	planRule.PlanIDs = []int{1}
	channelRule := baseRule(7, "channel", true, "C", dbmodel.PromptRulePositionBefore)
	channelRule.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	channelRule.ChannelIDs = []int{3}
	modelRule := baseRule(2, "model", true, "M", dbmodel.PromptRulePositionBefore)
	modelRule.ModelMode = dbmodel.PromptRuleModelModeSelected
	modelRule.Models = []string{"m-*"}
	rules := []dbmodel.PromptRule{
		baseRule(9, "global-b", true, "GB", dbmodel.PromptRulePositionBefore),
		planRule,
		channelRule,
		modelRule,
	}
	got := Evaluate(rules, Scope{PlanID: 1, RequestModel: "m-x", ChannelID: 3})
	var names []string
	for _, m := range got {
		names = append(names, m.Rule.Name)
	}
	// global(9) → plan(5) → model(2) → channel(7)：tier 升序；同 tier 内 sort/id。
	want := []string{"global-b", "plan", "model", "channel"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order = %v, want %v", names, want)
	}
	for _, m := range got {
		if !m.Matched || m.Reason != ReasonMatched {
			t.Fatalf("%s matched=%v reason=%s", m.Rule.Name, m.Matched, m.Reason)
		}
	}
}

func TestEvaluateReasonsForNonMatches(t *testing.T) {
	aud := baseRule(2, "aud", true, "X", dbmodel.PromptRulePositionBefore)
	aud.Audience = dbmodel.PromptAudienceAdminOnly
	pln := baseRule(3, "pln", true, "X", dbmodel.PromptRulePositionBefore)
	pln.PlanMode = dbmodel.PromptRulePlanModeSelected
	pln.PlanIDs = []int{4}
	chn := baseRule(4, "chn", true, "X", dbmodel.PromptRulePositionBefore)
	chn.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	chn.ChannelIDs = []int{8}
	mdl := baseRule(5, "mdl", true, "X", dbmodel.PromptRulePositionBefore)
	mdl.ModelMode = dbmodel.PromptRuleModelModeSelected
	mdl.Models = []string{"gpt*"}
	cases := []struct {
		r    dbmodel.PromptRule
		reas string
	}{
		{baseRule(1, "off", false, "X", dbmodel.PromptRulePositionBefore), ReasonDisabled},
		{aud, ReasonAudience},
		{pln, ReasonPlan},
		{chn, ReasonChannel},
		{mdl, ReasonModel},
	}
	for _, c := range cases {
		got := Evaluate([]dbmodel.PromptRule{c.r}, Scope{PlanID: 1, RequestModel: "claude-x", ChannelID: 2})
		if len(got) != 1 || got[0].Matched || got[0].Reason != c.reas {
			t.Fatalf("%s: %#v", c.reas, got)
		}
	}
}

func TestEvaluatePlanScopes(t *testing.T) {
	sel := baseRule(1, "s", true, "T", dbmodel.PromptRulePositionBefore)
	sel.PlanMode = dbmodel.PromptRulePlanModeSelected
	sel.PlanIDs = []int{2, 4}
	if !Evaluate([]dbmodel.PromptRule{sel}, Scope{PlanID: 4})[0].Matched {
		t.Fatal("plan in list must match")
	}
	if Evaluate([]dbmodel.PromptRule{sel}, Scope{PlanID: 5})[0].Matched {
		t.Fatal("plan out of list must not match")
	}
	if Evaluate([]dbmodel.PromptRule{sel}, Scope{PlanID: 0})[0].Matched {
		t.Fatal("no-plan key must not match selected without include_no_plan")
	}
	sel.IncludeNoPlan = true
	if !Evaluate([]dbmodel.PromptRule{sel}, Scope{PlanID: 0})[0].Matched {
		t.Fatal("include_no_plan must match no-plan key")
	}
	all := baseRule(2, "a", true, "T", dbmodel.PromptRulePositionBefore) // plan_mode=all
	if !Evaluate([]dbmodel.PromptRule{all}, Scope{PlanID: 0})[0].Matched {
		t.Fatal("plan_mode=all must match no-plan key too")
	}
}

func TestEvaluateChannelAndModelScopes(t *testing.T) {
	mk := func(mode string, ids []int) dbmodel.PromptRule {
		return dbmodel.PromptRule{ID: 1, Name: "c", Enabled: true, Text: "T", Position: dbmodel.PromptRulePositionBefore,
			ChannelMode: mode, ChannelIDs: ids, PlanMode: dbmodel.PromptRulePlanModeAll, ModelMode: dbmodel.PromptRuleModelModeAll}
	}
	if !Evaluate([]dbmodel.PromptRule{mk("include", []int{7})}, Scope{ChannelID: 7})[0].Matched {
		t.Fatal("include must hit listed channel")
	}
	if Evaluate([]dbmodel.PromptRule{mk("include", []int{7})}, Scope{ChannelID: 8})[0].Matched {
		t.Fatal("include must not hit unlisted channel")
	}
	if !Evaluate([]dbmodel.PromptRule{mk("exclude", []int{7})}, Scope{ChannelID: 8})[0].Matched {
		t.Fatal("exclude must hit unlisted channel")
	}
	if Evaluate([]dbmodel.PromptRule{mk("exclude", []int{7})}, Scope{ChannelID: 7})[0].Matched {
		t.Fatal("exclude must not hit listed channel")
	}

	mm := func(models []string) dbmodel.PromptRule {
		return dbmodel.PromptRule{ID: 1, Name: "m", Enabled: true, Text: "T", Position: dbmodel.PromptRulePositionBefore,
			PlanMode: dbmodel.PromptRulePlanModeAll, ChannelMode: dbmodel.PromptRuleChannelModeAll, ModelMode: dbmodel.PromptRuleModelModeSelected, Models: models}
	}
	// 精确匹配大小写不敏感 + 1M 别名清洗。
	if !Evaluate([]dbmodel.PromptRule{mm([]string{"CLAUDE-X"})}, Scope{RequestModel: "claude-x[1m]"})[0].Matched {
		t.Fatal("model match must clean 1M alias and ignore case")
	}
	if !Evaluate([]dbmodel.PromptRule{mm([]string{"gpt-4*"})}, Scope{RequestModel: "GPT-4-turbo"})[0].Matched {
		t.Fatal("trailing-* must prefix-match case-insensitively")
	}
	if Evaluate([]dbmodel.PromptRule{mm([]string{"gpt-4*"})}, Scope{RequestModel: "claude-x"})[0].Matched {
		t.Fatal("prefix mismatch must not match")
	}
	if Evaluate([]dbmodel.PromptRule{mm(nil)}, Scope{RequestModel: "anything"})[0].Matched {
		t.Fatal("empty model list in selected mode must not match")
	}
}

func TestApplySkipAndOrderRules(t *testing.T) {
	rules := []dbmodel.PromptRule{
		baseRule(1, "on-blank", true, "   ", dbmodel.PromptRulePositionBefore),
		baseRule(2, "off", false, "OFF", dbmodel.PromptRulePositionBefore),
	}
	aud := baseRule(3, "aud", true, "AUD", dbmodel.PromptRulePositionBefore)
	aud.Audience = dbmodel.PromptAudienceAdminOnly
	rules = append(rules, aud)

	req := promptRuleTestReq("CLIENT")
	snap := Apply(req, Evaluate(rules, Scope{}))
	if *req.Messages[0].Content.Content != "CLIENT" || len(snap.Sources) != 0 || snap.Mode != "" {
		t.Fatalf("blank/off/admin-only must all be skipped: %#v %q", snap, *req.Messages[0].Content.Content)
	}

	// before 前插、after 后插；tier 越具体越外；同一请求多规则组合。
	planRule := baseRule(2, "plan", true, "P", dbmodel.PromptRulePositionBefore)
	planRule.PlanMode = dbmodel.PromptRulePlanModeSelected
	planRule.PlanIDs = []int{1}
	channelRule := baseRule(3, "channel", true, "C", dbmodel.PromptRulePositionBefore)
	channelRule.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	channelRule.ChannelIDs = []int{5}
	rules = []dbmodel.PromptRule{
		baseRule(1, "global", true, "GB", dbmodel.PromptRulePositionBefore),
		planRule,
		channelRule,
	}
	req = promptRuleTestReq("CLIENT")
	snap = Apply(req, Evaluate(rules, Scope{PlanID: 1, ChannelID: 5}))
	if got := *req.Messages[0].Content.Content; got != "C\nP\nGB\nCLIENT" {
		t.Fatalf("all-before order = %q, want C\nP\nGB\nCLIENT", got)
	}
	if snap.Mode != dbmodel.PromptOverrideModeBefore || len(snap.Sources) != 3 {
		t.Fatalf("snap = %#v", snap)
	}

	afters := []dbmodel.PromptRule{
		baseRule(1, "global", true, "GA", dbmodel.PromptRulePositionAfter),
	}
	ca := baseRule(2, "channel", true, "CA", dbmodel.PromptRulePositionAfter)
	ca.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	ca.ChannelIDs = []int{5}
	afters = append(afters, ca)
	req2 := promptRuleTestReq("CLIENT")
	snap2 := Apply(req2, Evaluate(afters, Scope{ChannelID: 5}))
	if got := *req2.Messages[0].Content.Content; got != "CLIENT\nGA\nCA" {
		t.Fatalf("all-after order = %q, want CLIENT\nGA\nCA", got)
	}
	if snap2.Mode != dbmodel.PromptOverrideModeAfter {
		t.Fatalf("snap2 = %#v", snap2)
	}

	mixed := []dbmodel.PromptRule{baseRule(1, "gb", true, "GB", dbmodel.PromptRulePositionBefore), baseRule(2, "ga", true, "GA", dbmodel.PromptRulePositionAfter)}
	req3 := promptRuleTestReq("")
	snap3 := Apply(req3, Evaluate(mixed, Scope{}))
	if got := *req3.Messages[0].Content.Content; got != "GB\nGA" {
		t.Fatalf("no-client mixed = %q, want GB\nGA", got)
	}
	if snap3.Mode != dbmodel.PromptOverrideMode("before+after") {
		t.Fatalf("snap3 mode = %v", snap3.Mode)
	}
}

func TestPreviewSegmentsAndText(t *testing.T) {
	ca := baseRule(2, "ca", true, "CA", dbmodel.PromptRulePositionAfter)
	ca.ChannelMode = dbmodel.PromptRuleChannelModeInclude
	ca.ChannelIDs = []int{5}
	rules := []dbmodel.PromptRule{
		baseRule(1, "gb", true, "GB", dbmodel.PromptRulePositionBefore),
		ca,
		baseRule(3, "off", false, "OFF", dbmodel.PromptRulePositionBefore),
	}
	res := Preview(rules, Scope{ChannelID: 5}, "CLIENT")
	if res.Text != "GB\nCLIENT\nCA" {
		t.Fatalf("preview text = %q, want GB\nCLIENT\nCA", res.Text)
	}
	if len(res.Matches) != 3 || res.Matches[2].Matched || res.Matches[2].Reason != ReasonDisabled {
		t.Fatalf("preview matches must include non-matches with reason: %#v", res.Matches)
	}
	// 段序 = 阅读序：before（GB）→ client → after（CA），关闭规则不进段。
	if len(res.Segments) != 3 {
		t.Fatalf("segments = %#v", res.Segments)
	}
	if res.Segments[0].RuleID != 1 || res.Segments[1].RuleID != 0 || res.Segments[2].RuleID != 2 {
		t.Fatalf("segment rule ids = %#v", res.Segments)
	}
	if res.Segments[1].Name != clientSegmentName || res.Segments[1].Position != clientSegmentPosition {
		t.Fatalf("client segment = %#v", res.Segments[1])
	}
}
