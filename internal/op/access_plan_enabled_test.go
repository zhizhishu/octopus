package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 方案页「使用」开关关掉的线路、计费里关掉的单独倍率，整表保存后必须仍是关着的
// （以前 GORM 的 default:true 会把 false 吞掉，开关形同虚设）。
func TestAccessPlanSaveKeepsDisabledLinesAndBillingRules(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var vip model.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}

	channels := []model.Channel{
		{Name: "off-channel-a", Enabled: true, Model: "off-model"},
		{Name: "off-channel-b", Enabled: true, Model: "off-model"},
	}
	for i := range channels {
		if err := ChannelCreate(&channels[i], ctx); err != nil {
			t.Fatalf("create channel %d: %v", i, err)
		}
	}

	saved, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{RequestModel: "off-model", Mode: model.GroupModeFillFirst, ChannelID: channels[0].ID, UpstreamModel: "off-model", Priority: 1, Weight: 1, Enabled: true},
		{RequestModel: "off-model", Mode: model.GroupModeFillFirst, ChannelID: channels[1].ID, UpstreamModel: "off-model", Priority: 2, Weight: 1, Enabled: false},
	}, ctx)
	if err != nil {
		t.Fatalf("save targets: %v", err)
	}
	enabled := map[int]bool{}
	for _, target := range saved.RouteTargets {
		if target.RequestModel == "off-model" {
			enabled[target.ChannelID] = target.Enabled
		}
	}
	if !enabled[channels[0].ID] || enabled[channels[1].ID] {
		t.Fatalf("enabled flags after save = %v, want a=true b=false", enabled)
	}

	// 关掉的线路真的不参与选路。
	selected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(selected, "off-model", ctx)
	if err != nil || !ok {
		t.Fatalf("expected route group, ok=%v err=%v", ok, err)
	}
	if len(group.Items) != 1 || group.Items[0].ChannelID != channels[0].ID {
		t.Fatalf("group items = %+v, want only channel a", group.Items)
	}

	billed, err := AccessPlanUpdateBillingRules(vip.ID, 1, []model.AccessBillingModelRule{
		{ModelName: "on-model", Multiplier: 1.5, Enabled: true},
		{ModelName: "off-model", Multiplier: 2, Enabled: false},
	}, ctx)
	if err != nil {
		t.Fatalf("save billing rules: %v", err)
	}
	ruleEnabled := map[string]bool{}
	for _, rule := range billed.BillingRules {
		ruleEnabled[rule.ModelName] = rule.Enabled
	}
	if !ruleEnabled["on-model"] || ruleEnabled["off-model"] {
		t.Fatalf("billing enabled flags = %v, want on=true off=false", ruleEnabled)
	}
}
