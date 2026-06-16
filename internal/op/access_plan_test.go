package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupAccessPlanTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})

	ctx := context.Background()
	if err := channelRefreshCache(ctx); err != nil {
		t.Fatalf("refresh channel cache: %v", err)
	}
	if err := groupRefreshCache(ctx); err != nil {
		t.Fatalf("refresh group cache: %v", err)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		t.Fatalf("refresh access plan cache: %v", err)
	}
	return ctx
}

func TestAccessPlanDefaultsAndAPIKeySelection(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	bySlug := map[string]model.AccessPlan{}
	for _, plan := range plans {
		bySlug[plan.Slug] = plan
	}
	for _, slug := range []string{"vip", "svip", "ssvip"} {
		if _, ok := bySlug[slug]; !ok {
			t.Fatalf("expected default plan %q", slug)
		}
	}

	plan, err := AccessPlanSelect(0, "", ctx)
	if err != nil {
		t.Fatalf("select default plan: %v", err)
	}
	if plan == nil || plan.Slug != "vip" {
		t.Fatalf("expected vip as system default, got %#v", plan)
	}

	apiKey := model.APIKey{Name: "access plan scoped key", APIKey: "sk-access-plan", Enabled: true}
	if err := db.GetDB().WithContext(ctx).Create(&apiKey).Error; err != nil {
		t.Fatalf("create API key fixture: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("refresh API key cache: %v", err)
	}
	if err := APIKeyAccessPlanSet(apiKey.ID, []int{bySlug["vip"].ID, bySlug["svip"].ID}, bySlug["svip"].ID, ctx); err != nil {
		t.Fatalf("bind plans: %v", err)
	}
	plan, err = AccessPlanSelect(apiKey.ID, "", ctx)
	if err != nil {
		t.Fatalf("select key default plan: %v", err)
	}
	if plan == nil || plan.Slug != "svip" {
		t.Fatalf("expected svip API key default, got %#v", plan)
	}
	if _, err := AccessPlanSelect(apiKey.ID, "ssvip", ctx); err == nil {
		t.Fatalf("expected unbound header plan to be rejected")
	}

	disabled := bySlug["svip"]
	if err := AccessPlanUpdate(&model.AccessPlan{
		ID:          disabled.ID,
		Slug:        disabled.Slug,
		DisplayName: disabled.DisplayName,
		Enabled:     false,
		IsDefault:   false,
	}, ctx); err != nil {
		t.Fatalf("disable bound plan: %v", err)
	}
	disabled = bySlug["vip"]
	if err := AccessPlanUpdate(&model.AccessPlan{
		ID:          disabled.ID,
		Slug:        disabled.Slug,
		DisplayName: disabled.DisplayName,
		Enabled:     false,
		IsDefault:   false,
	}, ctx); err != nil {
		t.Fatalf("disable second bound plan: %v", err)
	}
	if _, err := AccessPlanSelect(apiKey.ID, "", ctx); err == nil {
		t.Fatalf("expected disabled bound plan to block default selection")
	}
	if _, err := AccessPlanSelect(apiKey.ID, "ssvip", ctx); err == nil {
		t.Fatalf("expected disabled bound plan to block header bypass")
	}
	if err := APIKeyAccessPlanSet(9999, []int{bySlug["vip"].ID}, bySlug["vip"].ID, ctx); err == nil {
		t.Fatalf("expected binding a missing API key to fail")
	}
}

func TestUserAccessPlansConstrainAPIKeys(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	bySlug := map[string]model.AccessPlan{}
	for _, plan := range plans {
		bySlug[plan.Slug] = plan
	}

	user, err := UserCreate(model.UserCreateRequest{
		Username:            "plan-user",
		Password:            "secret",
		Role:                model.UserRoleUser,
		Status:              model.UserStatusActive,
		AccessPlanIDs:       []int{bySlug["svip"].ID},
		DefaultAccessPlanID: bySlug["svip"].ID,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if len(user.AccessPlanIDs) != 1 || user.AccessPlanIDs[0] != bySlug["svip"].ID {
		t.Fatalf("unexpected user access plans: %#v", user.AccessPlanIDs)
	}

	apiKey := model.APIKey{
		UserID:  user.ID,
		Name:    "user scoped key",
		APIKey:  "sk-user-plan",
		Enabled: true,
	}
	if err := APIKeyCreate(&apiKey, ctx); err != nil {
		t.Fatalf("create API key: %v", err)
	}
	created, err := APIKeyGet(apiKey.ID, ctx)
	if err != nil {
		t.Fatalf("get API key: %v", err)
	}
	if created.DefaultAccessPlanID != bySlug["svip"].ID {
		t.Fatalf("expected user default svip, got %#v", created)
	}
	if err := APIKeyAccessPlanSet(apiKey.ID, []int{bySlug["vip"].ID}, bySlug["vip"].ID, ctx); err == nil {
		t.Fatalf("expected API key binding outside user plans to fail")
	}
	if err := APIKeyAccessPlanSet(apiKey.ID, []int{bySlug["svip"].ID}, bySlug["svip"].ID, ctx); err != nil {
		t.Fatalf("expected allowed user plan binding to pass: %v", err)
	}
}

func TestAccessPlanRouteBuildsGroupTargets(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var svip model.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}
	if svip.ID == 0 {
		t.Fatalf("svip plan not found")
	}

	channel := model.Channel{Name: "access-route-channel", Enabled: true, Model: "upstream-model"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rule := model.AccessRouteRule{
		RouteProfileID:     svip.RouteProfileID,
		RequestModel:       "request-model",
		BillingModelSource: model.AccessBillingModelSourceUpstream,
		FallbackMode:       model.AccessRouteFallbackGroup,
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	target := model.AccessRouteTarget{
		RouteRuleID:   rule.ID,
		ChannelID:     channel.ID,
		UpstreamModel: "upstream-model",
		Priority:      1,
		Weight:        2,
		Enabled:       true,
	}
	if err := AccessRouteTargetCreate(&target, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}

	plan, err := AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatalf("select svip: %v", err)
	}
	group, matchedRule, ok, err := AccessPlanGroupForModel(plan, "request-model", ctx)
	if err != nil {
		t.Fatalf("group for model: %v", err)
	}
	if !ok || matchedRule == nil {
		t.Fatalf("expected access route to match")
	}
	if len(group.Items) != 1 {
		t.Fatalf("expected one route target, got %d", len(group.Items))
	}
	if group.Items[0].ChannelID != channel.ID || group.Items[0].ModelName != "upstream-model" {
		t.Fatalf("unexpected target: %#v", group.Items[0])
	}
}

func TestAccessPlanRouteModelsHideStaleChannelModels(t *testing.T) {
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

	channel := model.Channel{Name: "stale-route-channel", Enabled: true, Model: "live-upstream"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel:  "stale-request",
		ChannelID:     channel.ID,
		UpstreamModel: "removed-upstream",
		Enabled:       true,
	}}, ctx); err != nil {
		t.Fatalf("create stale route target: %v", err)
	}
	models, err := AccessPlanRouteModels(ctx)
	if err != nil {
		t.Fatalf("list route models: %v", err)
	}
	if containsString(models, "stale-request") {
		t.Fatalf("stale route model should be hidden, got %v", models)
	}

	plan, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	if _, _, ok, err := AccessPlanGroupForModel(plan, "stale-request", ctx); err != nil || ok {
		t.Fatalf("stale route should not be usable, ok=%v err=%v", ok, err)
	}
}

func TestAccessPlanRouteTargetsRequireSelectedChannelModels(t *testing.T) {
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

	channel := model.Channel{
		Name:             "discovered-only-route-channel",
		Enabled:          true,
		DiscoveredModels: []string{"discovered-only-model"},
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel:  "discovered-request",
		ChannelID:     channel.ID,
		UpstreamModel: "discovered-only-model",
		Enabled:       true,
	}}, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}

	models, err := AccessPlanRouteModels(ctx)
	if err != nil {
		t.Fatalf("list route models: %v", err)
	}
	if containsString(models, "discovered-request") {
		t.Fatalf("discovered-only model must not authorize route target, got %v", models)
	}

	selected := []string{"discovered-only-model"}
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:             channel.ID,
		SelectedModels: &selected,
	}, ctx); err != nil {
		t.Fatalf("select discovered model: %v", err)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		t.Fatalf("refresh access plan cache: %v", err)
	}
	models, err = AccessPlanRouteModels(ctx)
	if err != nil {
		t.Fatalf("list route models after selection: %v", err)
	}
	if !containsString(models, "discovered-request") {
		t.Fatalf("selected model should authorize route target, got %v", models)
	}
}

func TestAccessPlanRouteModelsForAPIKeyUsesBoundPlans(t *testing.T) {
	ctx := setupAccessPlanTest(t)

	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	bySlug := map[string]model.AccessPlan{}
	for _, plan := range plans {
		bySlug[plan.Slug] = plan
	}

	channel := model.Channel{Name: "scoped-route-channel", Enabled: true, Model: "vip-upstream,svip-upstream"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := AccessPlanUpdateRouteTargets(bySlug["vip"].ID, []model.AccessRouteTarget{{
		RequestModel:  "vip-request",
		ChannelID:     channel.ID,
		UpstreamModel: "vip-upstream",
		Enabled:       true,
	}}, ctx); err != nil {
		t.Fatalf("create vip route: %v", err)
	}
	if _, err := AccessPlanUpdateRouteTargets(bySlug["svip"].ID, []model.AccessRouteTarget{{
		RequestModel:  "svip-request",
		ChannelID:     channel.ID,
		UpstreamModel: "svip-upstream",
		Enabled:       true,
	}}, ctx); err != nil {
		t.Fatalf("create svip route: %v", err)
	}

	apiKey := model.APIKey{Name: "route scoped key", APIKey: "sk-route-scoped", Enabled: true}
	if err := db.GetDB().WithContext(ctx).Create(&apiKey).Error; err != nil {
		t.Fatalf("create API key fixture: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("refresh API key cache: %v", err)
	}
	if err := APIKeyAccessPlanSet(apiKey.ID, []int{bySlug["vip"].ID}, bySlug["vip"].ID, ctx); err != nil {
		t.Fatalf("bind vip plan: %v", err)
	}

	routeModels, err := AccessPlanRouteModelsForAPIKey(apiKey.ID, ctx)
	if err != nil {
		t.Fatalf("list API key route models: %v", err)
	}
	if !containsString(routeModels, "vip-request") {
		t.Fatalf("expected bound vip route model, got %v", routeModels)
	}
	if containsString(routeModels, "svip-request") {
		t.Fatalf("unbound svip route model should be hidden, got %v", routeModels)
	}

	allModels, err := GroupListModelForAPIKey(apiKey.ID, ctx)
	if err != nil {
		t.Fatalf("list API key models: %v", err)
	}
	if !containsString(allModels, "vip-request") || containsString(allModels, "svip-request") {
		t.Fatalf("unexpected API key model list: %v", allModels)
	}

	vipOnlyModels, err := GroupListModelForAPIKeyPlan(apiKey.ID, "vip", ctx)
	if err != nil {
		t.Fatalf("list vip header models: %v", err)
	}
	if !containsString(vipOnlyModels, "vip-request") || containsString(vipOnlyModels, "svip-request") {
		t.Fatalf("vip header should expose only vip route models, got %v", vipOnlyModels)
	}
	if _, err := GroupListModelForAPIKeyPlan(apiKey.ID, "svip", ctx); err == nil {
		t.Fatalf("unbound svip header should be rejected")
	}
}

func TestAccessRouteBillingDefaultsToRequestModel(t *testing.T) {
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

	rule := model.AccessRouteRule{
		RouteProfileID: vip.RouteProfileID,
		RequestModel:   "request-default-billing",
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	if rule.BillingModelSource != model.AccessBillingModelSourceRequest {
		t.Fatalf("expected create default request_model, got %q", rule.BillingModelSource)
	}

	channel := model.Channel{Name: "default-billing-route-channel", Enabled: true, Model: "upstream-model"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	updated, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel:  "request-default-billing-bulk",
			ChannelID:     channel.ID,
			UpstreamModel: "upstream-model",
			Enabled:       true,
		},
	}, ctx)
	if err != nil {
		t.Fatalf("update route targets: %v", err)
	}
	if len(updated.RouteTargets) != 1 {
		t.Fatalf("expected one route target, got %d", len(updated.RouteTargets))
	}
	if updated.RouteTargets[0].BillingModelSource != model.AccessBillingModelSourceRequest {
		t.Fatalf("expected bulk default request_model, got %q", updated.RouteTargets[0].BillingModelSource)
	}
}

func TestAccessPlanUpdateRouteTargetsPreservesRouteMode(t *testing.T) {
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

	channel := model.Channel{Name: "route-mode-channel", Enabled: true, Model: "upstream-one,upstream-two"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel:  "mode-request",
		Mode:          model.GroupModeWeighted,
		ChannelID:     channel.ID,
		UpstreamModel: "upstream-one",
		Enabled:       true,
		Weight:        2,
	}}, ctx); err != nil {
		t.Fatalf("create weighted route: %v", err)
	}
	updated, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel:  "mode-request",
		ChannelID:     channel.ID,
		UpstreamModel: "upstream-two",
		Enabled:       true,
		Weight:        3,
	}}, ctx)
	if err != nil {
		t.Fatalf("update route without mode: %v", err)
	}
	if len(updated.RouteTargets) != 1 || updated.RouteTargets[0].Mode != model.GroupModeWeighted {
		t.Fatalf("expected route mode to be preserved in flattened targets, got %#v", updated.RouteTargets)
	}

	selected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(selected, "mode-request", ctx)
	if err != nil || !ok {
		t.Fatalf("expected route group, ok=%v err=%v", ok, err)
	}
	if group.Mode != model.GroupModeWeighted {
		t.Fatalf("expected weighted route mode, got %d", group.Mode)
	}
}

func TestAccessPlanUpdatePreservesProfilesAndAllowsDefaultSlugRename(t *testing.T) {
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

	channel := model.Channel{Name: "preserve-route-channel", Enabled: true, Model: "upstream-before-rename"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel:       "request-before-rename",
			ChannelID:          channel.ID,
			UpstreamModel:      "upstream-before-rename",
			Priority:           1,
			Weight:             1,
			Enabled:            true,
			BillingModelSource: model.AccessBillingModelSourceUpstream,
			FallbackMode:       model.AccessRouteFallbackGroup,
		},
	}, ctx); err != nil {
		t.Fatalf("create route target through access plan: %v", err)
	}

	if err := AccessPlanUpdate(&model.AccessPlan{
		ID:          vip.ID,
		Slug:        "premium",
		DisplayName: "Premium",
		Enabled:     true,
		IsDefault:   true,
	}, ctx); err != nil {
		t.Fatalf("rename plan: %v", err)
	}

	plans, err = AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list renamed plans: %v", err)
	}
	var renamed model.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			t.Fatalf("renaming the seeded vip plan should not recreate the old slug")
		}
		if plan.Slug == "premium" {
			renamed = plan
		}
	}
	if renamed.ID != vip.ID {
		t.Fatalf("renamed plan not found")
	}
	if renamed.RouteProfileID != vip.RouteProfileID || renamed.BillingProfileID != vip.BillingProfileID {
		t.Fatalf("profile ids were not preserved: before route=%d billing=%d after route=%d billing=%d",
			vip.RouteProfileID, vip.BillingProfileID, renamed.RouteProfileID, renamed.BillingProfileID)
	}
	if renamed.Sort != vip.Sort {
		t.Fatalf("sort was not preserved: before %d after %d", vip.Sort, renamed.Sort)
	}
	if len(renamed.RouteTargets) != 1 || renamed.RouteTargets[0].RequestModel != "request-before-rename" {
		t.Fatalf("route targets were not preserved: %#v", renamed.RouteTargets)
	}
}
