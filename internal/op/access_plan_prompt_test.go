package op

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 方案页（画布、路由编辑、方案设置）只管方案和路由；系统提示词只在提示词页改。
// 这两组测试证明两边各存各的：方案页整行保存不会清掉提示词，提示词页保存不会改路由。

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }
func modep(m model.PromptOverrideMode) *model.PromptOverrideMode {
	return &m
}

type promptTriple struct {
	Text    string
	Mode    model.PromptOverrideMode
	Enabled bool
}

func planPromptInDB(t *testing.T, ctx context.Context, id int) promptTriple {
	t.Helper()
	var p model.AccessPlan
	if err := db.GetDB().WithContext(ctx).First(&p, id).Error; err != nil {
		t.Fatalf("load plan: %v", err)
	}
	return promptTriple{p.SystemPromptOverride, p.PromptOverrideMode, p.PromptOverrideEnabled}
}

func routePromptInDB(t *testing.T, ctx context.Context, planID int, requestModel string) (promptTriple, bool) {
	t.Helper()
	plan, err := accessPlanLoadForWrite(planID, ctx)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	var rule model.AccessRouteRule
	err = db.GetDB().WithContext(ctx).Where("route_profile_id = ? AND request_model = ?", plan.RouteProfileID, requestModel).First(&rule).Error
	if err != nil {
		return promptTriple{}, false
	}
	return promptTriple{rule.SystemPromptOverride, rule.PromptOverrideMode, rule.PromptOverrideEnabled}, true
}

func planBySlug(t *testing.T, ctx context.Context, slug string) model.AccessPlan {
	t.Helper()
	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	for _, p := range plans {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("plan %s not found", slug)
	return model.AccessPlan{}
}

func promptTestChannels(t *testing.T, ctx context.Context) (int, int) {
	t.Helper()
	a := model.Channel{Name: "prompt-sep-a", Enabled: true, Model: "m-a,m-b"}
	b := model.Channel{Name: "prompt-sep-b", Enabled: true, Model: "m-a,m-c"}
	for _, ch := range []*model.Channel{&a, &b} {
		if err := ChannelCreate(ch, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
	}
	return a.ID, b.ID
}

// 方案页发的整表（不带任何提示词字段），和前端 saveRoutes 发的一样。
func planPageRouteTable(chA, chB int, mode model.GroupMode, extra ...model.AccessRouteTarget) []model.AccessRouteTarget {
	t := []model.AccessRouteTarget{
		{RequestModel: "m-a", ChannelID: chA, UpstreamModel: "m-a", Priority: 1, Weight: 1, Enabled: true, Mode: mode, BillingModelSource: model.AccessBillingModelSourceRequest, FallbackMode: model.AccessRouteFallbackReturnGroup},
		{RequestModel: "m-a", ChannelID: chB, UpstreamModel: "m-a", Priority: 2, Weight: 1, Enabled: true, Mode: mode, BillingModelSource: model.AccessBillingModelSourceRequest, FallbackMode: model.AccessRouteFallbackReturnGroup},
		{RequestModel: "m-b", ChannelID: chA, UpstreamModel: "m-b", Priority: 1, Weight: 1, Enabled: true, Mode: mode, BillingModelSource: model.AccessBillingModelSourceRequest, FallbackMode: model.AccessRouteFallbackReturnGroup},
	}
	return append(t, extra...)
}

func TestPlanPageSaveKeepsPlanPrompt(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	vip := planBySlug(t, ctx, "vip")
	if _, err := AccessPlanUpdatePrompt(vip.ID, model.PromptOverridePatch{
		SystemPromptOverride: strp("VIP 先给结论"), PromptOverrideMode: modep(model.PromptOverrideModeAfter), PromptOverrideEnabled: boolp(true),
	}, ctx); err != nil {
		t.Fatalf("prompt save: %v", err)
	}
	want := promptTriple{"VIP 先给结论", model.PromptOverrideModeAfter, true}

	// 方案设置保存：名称 / slug / 启用 / 默认，不带提示词字段（零值）。
	if err := AccessPlanUpdate(&model.AccessPlan{ID: vip.ID, Slug: "vip", DisplayName: "VIP 改名", Enabled: true, IsDefault: vip.IsDefault}, ctx); err != nil {
		t.Fatalf("plan save: %v", err)
	}
	if got := planPromptInDB(t, ctx, vip.ID); got != want {
		t.Fatalf("plan page save changed the plan prompt: %+v", got)
	}
	cached := planBySlug(t, ctx, "vip")
	if cached.DisplayName != "VIP 改名" {
		t.Fatalf("plan save did not apply: %q", cached.DisplayName)
	}
	if (promptTriple{cached.SystemPromptOverride, cached.PromptOverrideMode, cached.PromptOverrideEnabled}) != want {
		t.Fatalf("cache lost the plan prompt: %+v", cached)
	}

	// 即使有人在方案保存里塞了提示词字段，op 层的方案保存也不改提示词（兼容走 handler 单独写）。
	if err := AccessPlanUpdate(&model.AccessPlan{ID: vip.ID, Slug: "vip", DisplayName: "VIP", Enabled: true, IsDefault: vip.IsDefault, SystemPromptOverride: "", PromptOverrideMode: model.PromptOverrideModeBefore}, ctx); err != nil {
		t.Fatalf("plan save: %v", err)
	}
	if got := planPromptInDB(t, ctx, vip.ID); got != want {
		t.Fatalf("plan save with zeroed prompt fields wiped it: %+v", got)
	}
}

func TestPlanPageRouteTableSaveKeepsRoutePrompt(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	vip := planBySlug(t, ctx, "vip")
	chA, chB := promptTestChannels(t, ctx)
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, planPageRouteTable(chA, chB, model.GroupModeFillFirst), ctx); err != nil {
		t.Fatalf("route save: %v", err)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "M-A", model.PromptOverridePatch{
		SystemPromptOverride: strp("代码给完整"), PromptOverrideMode: modep(model.PromptOverrideModeAfter), PromptOverrideEnabled: boolp(true),
	}, ctx); err != nil {
		t.Fatalf("route prompt save (case-insensitive model): %v", err)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "m-b", model.PromptOverridePatch{
		SystemPromptOverride: strp("关着的"), PromptOverrideMode: modep(model.PromptOverrideModeBefore), PromptOverrideEnabled: boolp(false),
	}, ctx); err != nil {
		t.Fatalf("route prompt save: %v", err)
	}
	wantA := promptTriple{"代码给完整", model.PromptOverrideModeAfter, true}
	wantB := promptTriple{"关着的", model.PromptOverrideModeBefore, false}

	// 方案页改路由：换分配方式、调顺序、加一个新请求模型；整表重建规则，不带提示词。
	table := planPageRouteTable(chB, chA, model.GroupModeSpreadTiered, model.AccessRouteTarget{
		RequestModel: "m-c", ChannelID: chB, UpstreamModel: "m-c", Priority: 1, Weight: 1, Enabled: true,
	})
	plan, err := AccessPlanUpdateRouteTargets(vip.ID, table, ctx)
	if err != nil {
		t.Fatalf("route save: %v", err)
	}
	if got, _ := routePromptInDB(t, ctx, vip.ID, "m-a"); got != wantA {
		t.Fatalf("route table save changed m-a prompt: %+v", got)
	}
	if got, _ := routePromptInDB(t, ctx, vip.ID, "m-b"); got != wantB {
		t.Fatalf("route table save changed m-b prompt (off + text must survive): %+v", got)
	}
	if got, ok := routePromptInDB(t, ctx, vip.ID, "m-c"); !ok || got.Text != "" || got.Enabled {
		t.Fatalf("new route must start without a prompt: %+v ok=%v", got, ok)
	}
	// 路由本身确实改了，并且接口回来的每个目标都带着所属规则的提示词。
	for _, target := range plan.RouteTargets {
		if target.RequestModel == "m-a" {
			if target.Mode != model.GroupModeSpreadTiered {
				t.Fatalf("routing change not applied: mode=%v", target.Mode)
			}
			if target.SystemPromptOverride != wantA.Text || target.PromptOverrideMode != wantA.Mode || !target.PromptOverrideEnabled {
				t.Fatalf("flattened target lost the rule prompt: %+v", target)
			}
		}
	}

	// 删掉一个路由再保存：它的提示词跟着没了（路由不存在，也就没有路由层）。
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, planPageRouteTable(chA, chB, model.GroupModeFillFirst)[:2], ctx); err != nil {
		t.Fatalf("route save: %v", err)
	}
	if _, ok := routePromptInDB(t, ctx, vip.ID, "m-b"); ok {
		t.Fatalf("deleted route still exists")
	}
	if got, _ := routePromptInDB(t, ctx, vip.ID, "m-a"); got != wantA {
		t.Fatalf("m-a prompt lost after deleting m-b: %+v", got)
	}
}

func TestRouteRuleUpdateKeepsPrompt(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	vip := planBySlug(t, ctx, "vip")
	chA, chB := promptTestChannels(t, ctx)
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, planPageRouteTable(chA, chB, model.GroupModeFillFirst), ctx); err != nil {
		t.Fatalf("route save: %v", err)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "m-a", model.PromptOverridePatch{SystemPromptOverride: strp("X"), PromptOverrideEnabled: boolp(true)}, ctx); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	plan, _ := accessPlanLoadForWrite(vip.ID, ctx)
	var rule model.AccessRouteRule
	if err := db.GetDB().Where("route_profile_id = ? AND request_model = ?", plan.RouteProfileID, "m-a").First(&rule).Error; err != nil {
		t.Fatal(err)
	}
	rule.SystemPromptOverride, rule.PromptOverrideEnabled = "", false
	rule.FallbackMode = model.AccessRouteFallbackNone
	if err := AccessRouteRuleUpdate(&rule, ctx); err != nil {
		t.Fatalf("rule update: %v", err)
	}
	if got, _ := routePromptInDB(t, ctx, vip.ID, "m-a"); got != (promptTriple{"X", model.PromptOverrideModeBefore, true}) {
		t.Fatalf("rule update changed the prompt: %+v", got)
	}
}

// 提示词页保存只写提示词三列：方案和路由的其他字段一点不变。
func TestPromptPageSaveDoesNotChangeRouting(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	vip := planBySlug(t, ctx, "vip")
	chA, chB := promptTestChannels(t, ctx)
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, planPageRouteTable(chA, chB, model.GroupModeSpreadTiered), ctx); err != nil {
		t.Fatalf("route save: %v", err)
	}
	type routing struct {
		Plan    model.AccessPlan
		Rules   []model.AccessRouteRule
		Targets []model.AccessRouteTarget
	}
	snapshot := func() routing {
		var r routing
		db.GetDB().First(&r.Plan, vip.ID)
		r.Plan.SystemPromptOverride, r.Plan.PromptOverrideMode, r.Plan.PromptOverrideEnabled = "", "", false
		db.GetDB().Where("route_profile_id = ?", r.Plan.RouteProfileID).Order("id").Find(&r.Rules)
		ids := make([]int, 0, len(r.Rules))
		for i := range r.Rules {
			ids = append(ids, r.Rules[i].ID)
			r.Rules[i].SystemPromptOverride, r.Rules[i].PromptOverrideMode, r.Rules[i].PromptOverrideEnabled = "", "", false
		}
		db.GetDB().Where("route_rule_id IN ?", ids).Order("id").Find(&r.Targets)
		sort.Slice(r.Targets, func(i, j int) bool { return r.Targets[i].ID < r.Targets[j].ID })
		return r
	}
	before := snapshot()
	if len(before.Rules) != 2 || len(before.Targets) != 3 {
		t.Fatalf("setup: %d rules %d targets", len(before.Rules), len(before.Targets))
	}
	if _, err := AccessPlanUpdatePrompt(vip.ID, model.PromptOverridePatch{SystemPromptOverride: strp("P"), PromptOverrideMode: modep(model.PromptOverrideModeAfter), PromptOverrideEnabled: boolp(true)}, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "m-a", model.PromptOverridePatch{SystemPromptOverride: strp("R"), PromptOverrideEnabled: boolp(true)}, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "m-a", model.PromptOverridePatch{PromptOverrideEnabled: boolp(false)}, ctx); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("prompt save changed routing\nbefore=%+v\nafter=%+v", before, after)
	}
	// 只关开关：文字和位置保留。
	if got, _ := routePromptInDB(t, ctx, vip.ID, "m-a"); got != (promptTriple{"R", model.PromptOverrideModeBefore, false}) {
		t.Fatalf("switch-only patch: %+v", got)
	}
}

func TestPromptPatchSemantics(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	vip := planBySlug(t, ctx, "vip")
	if _, err := AccessPlanUpdatePrompt(vip.ID, model.PromptOverridePatch{PromptOverrideMode: modep("fill_missing")}, ctx); err == nil {
		t.Fatalf("unknown mode must be rejected")
	}
	// 旧客户端只传旧模式值：有文字算「开 + 前面」。
	if _, err := AccessPlanUpdatePrompt(vip.ID, model.PromptOverridePatch{SystemPromptOverride: strp("  旧  "), PromptOverrideMode: modep(model.PromptOverrideModeAppendSystem)}, ctx); err != nil {
		t.Fatal(err)
	}
	if got := planPromptInDB(t, ctx, vip.ID); got != (promptTriple{"旧", model.PromptOverrideModeBefore, true}) {
		t.Fatalf("legacy patch: %+v", got)
	}
	if _, err := AccessPlanUpdateRoutePrompt(vip.ID, "no-such-model", model.PromptOverridePatch{SystemPromptOverride: strp("x")}, ctx); err == nil {
		t.Fatalf("unknown route must be an error")
	}
}
