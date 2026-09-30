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
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	if err := channelRefreshCache(ctx); err != nil {
		t.Fatalf("refresh channel cache: %v", err)
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
	// Disabling every bound plan must NOT brick the key: with no explicit header it now
	// falls back to the enabled default plan / pool (matching an unbound key) instead of
	// hard-failing every request — the fix for "Cursor lists models but every call 403s"
	// after a plan is disabled. The header-scoped bypass below still stays rejected.
	if fallback, err := AccessPlanSelect(apiKey.ID, "", ctx); err != nil {
		t.Fatalf("expected disabled bound plans to fall back to default, got error: %v", err)
	} else if fallback == nil {
		t.Fatalf("expected a default fallback plan when all bound plans are disabled, got nil")
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

func TestAccessPlanGroupForModelInjectsRoutingWeight(t *testing.T) {
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

	channelA := model.Channel{Name: "weight-route-a", Enabled: true, Model: "upstream-a"}
	if err := ChannelCreate(&channelA, ctx); err != nil {
		t.Fatalf("create channel a: %v", err)
	}
	channelB := model.Channel{Name: "weight-route-b", Enabled: true, Model: "upstream-b"}
	if err := ChannelCreate(&channelB, ctx); err != nil {
		t.Fatalf("create channel b: %v", err)
	}

	rule := model.AccessRouteRule{
		RouteProfileID:     svip.RouteProfileID,
		RequestModel:       "weighted-request",
		BillingModelSource: model.AccessBillingModelSourceUpstream,
		FallbackMode:       model.AccessRouteFallbackGroup,
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	for _, target := range []model.AccessRouteTarget{
		{RouteRuleID: rule.ID, ChannelID: channelA.ID, UpstreamModel: "upstream-a", Priority: 1, Weight: 3, Enabled: true},
		{RouteRuleID: rule.ID, ChannelID: channelB.ID, UpstreamModel: "upstream-b", Priority: 1, Weight: 1, Enabled: true},
	} {
		targetCopy := target
		if err := AccessRouteTargetCreate(&targetCopy, ctx); err != nil {
			t.Fatalf("create route target: %v", err)
		}
	}

	plan, err := AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatalf("select svip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(plan, "weighted-request", ctx)
	if err != nil || !ok {
		t.Fatalf("group for model: ok=%v err=%v", ok, err)
	}
	if len(group.Items) != 2 {
		t.Fatalf("expected two route targets, got %#v", group.Items)
	}
	byChannelID := map[int]model.GroupItem{}
	for _, item := range group.Items {
		byChannelID[item.ChannelID] = item
	}
	if byChannelID[channelA.ID].RoutingWeight != 3 || byChannelID[channelA.ID].Weight != 3 {
		t.Fatalf("channel A must carry access-plan weight into RoutingWeight: %#v", byChannelID[channelA.ID])
	}
	if byChannelID[channelB.ID].RoutingWeight != 1 || byChannelID[channelB.ID].Weight != 1 {
		t.Fatalf("channel B must carry access-plan weight into RoutingWeight: %#v", byChannelID[channelB.ID])
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
		Mode:          model.GroupModeFillFirst,
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
	if len(updated.RouteTargets) != 1 || updated.RouteTargets[0].Mode != model.GroupModeFillFirst {
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
	if group.Mode != model.GroupModeFillFirst {
		t.Fatalf("expected weighted route mode, got %d", group.Mode)
	}
}

// TestAccessPlanSyncEnabledChannelsReconcile verifies add+remove reconcile:
//   - Enabling a channel that serves the rule model creates a target.
//   - Disabling that channel deletes the target.
//   - AutoSyncChannels no longer gates reconciliation.
func TestAccessPlanSyncEnabledChannelsReconcile(t *testing.T) {
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

	// Create a channel that serves "sync-test-model".
	ch := model.Channel{Name: "sync-reconcile-channel", Enabled: true, Model: "sync-test-model"}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Reload svip after update so we have the correct RouteProfileID.
	plans, err = AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans after update: %v", err)
	}
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}

	// Create a route rule for "sync-test-model" inside svip's profile.
	rule := model.AccessRouteRule{
		RouteProfileID: svip.RouteProfileID,
		RequestModel:   "sync-test-model",
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}

	// --- Phase 1: sync with channel enabled → target should be added ---
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync (add phase): %v", err)
	}

	var countAfterAdd int64
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessRouteTarget{}).
		Where("route_rule_id = ? AND channel_id = ?", rule.ID, ch.ID).
		Count(&countAfterAdd).Error; err != nil {
		t.Fatalf("count targets after add: %v", err)
	}
	if countAfterAdd != 1 {
		t.Fatalf("expected 1 target after add-sync, got %d", countAfterAdd)
	}

	// --- Phase 2: disable channel then sync → target should be removed ---
	if err := ChannelEnabled(ch.ID, false, ctx); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync (remove phase): %v", err)
	}

	var countAfterRemove int64
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessRouteTarget{}).
		Where("route_rule_id = ? AND channel_id = ?", rule.ID, ch.ID).
		Count(&countAfterRemove).Error; err != nil {
		t.Fatalf("count targets after remove: %v", err)
	}
	if countAfterRemove != 0 {
		t.Fatalf("expected 0 targets after disable+sync, got %d", countAfterRemove)
	}
}

// TestAccessPlanSyncEvictsDeletedChannel verifies that once a channel is deleted, the
// next AccessPlanSyncEnabledChannels pass drops its route targets. This is the reconcile
// the delete handler now schedules, closing the "deleted channel lingers until manual
// rebuild" gap.
func TestAccessPlanSyncEvictsDeletedChannel(t *testing.T) {
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

	// A second channel keeps channelCache non-empty after the delete below, so the sync's
	// "empty cache == not loaded yet" fail-safe (which returns early to avoid nuking every
	// route on a cache miss) doesn't short-circuit the eviction. A real deployment always
	// has other channels, so this only matters for the minimal test fixture. It serves an
	// unrelated model, so it never becomes a target for the "del-test-model" rule.
	keepAlive := model.Channel{Name: "sync-delete-keepalive", Enabled: true, Model: "keepalive-model"}
	if err := ChannelCreate(&keepAlive, ctx); err != nil {
		t.Fatalf("create keepalive channel: %v", err)
	}

	ch := model.Channel{Name: "sync-delete-channel", Enabled: true, Model: "del-test-model"}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	plans, err = AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans after update: %v", err)
	}
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}

	rule := model.AccessRouteRule{
		RouteProfileID: svip.RouteProfileID,
		RequestModel:   "del-test-model",
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}

	// --- Phase 1: sync with channel enabled → target should be added ---
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync (add phase): %v", err)
	}
	var countAfterAdd int64
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessRouteTarget{}).
		Where("route_rule_id = ? AND channel_id = ?", rule.ID, ch.ID).
		Count(&countAfterAdd).Error; err != nil {
		t.Fatalf("count targets after add: %v", err)
	}
	if countAfterAdd != 1 {
		t.Fatalf("expected 1 target after add-sync, got %d", countAfterAdd)
	}

	// --- Phase 2: delete channel then sync → target should be evicted ---
	if err := ChannelDel(ch.ID, ctx); err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync (evict phase): %v", err)
	}
	var countAfterDelete int64
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessRouteTarget{}).
		Where("channel_id = ?", ch.ID).
		Count(&countAfterDelete).Error; err != nil {
		t.Fatalf("count targets after delete: %v", err)
	}
	if countAfterDelete != 0 {
		t.Fatalf("expected 0 targets after delete+sync, got %d", countAfterDelete)
	}
}

// TestAccessPlanSyncHonorsChannelModelMapping verifies that a channel whose
// selected_models only carry the ugly upstream name (e.g. a provider alias
// "provider/deepseek-v4-pro") still joins the pool's canonical route
// ("deepseek-v4-pro") when it declares a model_mapping alias, and that the
// synced target sends the mapped upstream name on the wire.
func TestAccessPlanSyncHonorsChannelModelMapping(t *testing.T) {
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

	// The channel serves only the upstream alias in selected_models, but maps the
	// canonical pool name to it via model_mapping (client "pool-alias" → upstream
	// "upstream-real-model").
	ch := model.Channel{
		Name:         "sync-mapping-channel",
		Enabled:      true,
		Model:        "upstream-real-model",
		ModelMapping: map[string]string{"pool-alias": "upstream-real-model"},
	}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	plans, err = AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans after update: %v", err)
	}
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}

	// A rule for the canonical pool name that the channel does NOT list in
	// selected_models — only its model_mapping key matches.
	rule := model.AccessRouteRule{
		RouteProfileID: svip.RouteProfileID,
		RequestModel:   "pool-alias",
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}

	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var target model.AccessRouteTarget
	if err := db.GetDB().WithContext(ctx).
		Where("route_rule_id = ? AND channel_id = ?", rule.ID, ch.ID).
		First(&target).Error; err != nil {
		t.Fatalf("expected mapping-aware target to be synced, got: %v", err)
	}
	if target.UpstreamModel != "upstream-real-model" {
		t.Fatalf("expected target upstream 'upstream-real-model' (mapped), got %q", target.UpstreamModel)
	}
}

// TestAccessPlanSyncSkipsMappingToUnselectedUpstream verifies that a model_mapping
// whose upstream target is NOT one of the channel's selected models is NOT synced as a
// route target. accessRouteTargetAvailable validates a target's UpstreamModel against
// selected_models, so such an alias would only leave a dead target row that never routes.
func TestAccessPlanSyncSkipsMappingToUnselectedUpstream(t *testing.T) {
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

	// The channel serves only "real-a", but maps "alias-x" → "not-selected", an upstream
	// the channel does not actually serve.
	ch := model.Channel{
		Name:         "sync-bad-mapping-channel",
		Enabled:      true,
		Model:        "real-a",
		ModelMapping: map[string]string{"alias-x": "not-selected"},
	}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	plans, err = AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans after update: %v", err)
	}
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}

	rule := model.AccessRouteRule{
		RouteProfileID: svip.RouteProfileID,
		RequestModel:   "alias-x",
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}

	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var count int64
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessRouteTarget{}).
		Where("route_rule_id = ? AND channel_id = ?", rule.ID, ch.ID).
		Count(&count).Error; err != nil {
		t.Fatalf("count targets: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 targets for a mapping to an unselected upstream, got %d", count)
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

func TestAccessPlanSyncCreatesMappedRuleWithGlobalRouteMode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		override       string
		wantMode       model.GroupMode
		wantPriorities []int
	}{
		{name: "spread creates parallel targets", override: "spread", wantMode: model.GroupModeSpread, wantPriorities: []int{1, 1}},
		{name: "fill first follows channel order", override: "fill_first", wantMode: model.GroupModeFillFirst, wantPriorities: []int{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupAccessPlanTest(t)
			if err := SettingSetString(model.SettingKeyRouteModeOverride, tc.override); err != nil {
				t.Fatalf("set route mode override: %v", err)
			}

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
				t.Fatal("svip plan not found")
			}
			channels := []model.Channel{
				{Name: "mapped-rule-a", Enabled: true, Model: "upstream-a", ModelMapping: map[string]string{"client-alias": "upstream-a"}},
				{Name: "mapped-rule-b", Enabled: true, Model: "upstream-b", ModelMapping: map[string]string{"client-alias": "upstream-b"}},
			}
			for i := range channels {
				if err := ChannelCreate(&channels[i], ctx); err != nil {
					t.Fatalf("create channel %d: %v", i, err)
				}
			}
			if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
				t.Fatalf("sync enabled channels: %v", err)
			}

			var rule model.AccessRouteRule
			if err := db.GetDB().WithContext(ctx).
				Where("route_profile_id = ? AND request_model = ?", svip.RouteProfileID, "client-alias").
				First(&rule).Error; err != nil {
				t.Fatalf("expected client-facing mapping rule: %v", err)
			}
			if rule.Mode != tc.wantMode {
				t.Fatalf("rule mode = %d, want %d", rule.Mode, tc.wantMode)
			}

			var targets []model.AccessRouteTarget
			if err := db.GetDB().WithContext(ctx).
				Where("route_rule_id = ?", rule.ID).
				Order("channel_id ASC").
				Find(&targets).Error; err != nil {
				t.Fatalf("list created targets: %v", err)
			}
			if len(targets) != 2 {
				t.Fatalf("target count = %d, want 2", len(targets))
			}
			for i, target := range targets {
				if target.Priority != tc.wantPriorities[i] {
					t.Fatalf("target %d priority = %d, want %d", i, target.Priority, tc.wantPriorities[i])
				}
				wantUpstream := channels[i].Model
				if target.UpstreamModel != wantUpstream {
					t.Fatalf("target %d upstream = %q, want %q", i, target.UpstreamModel, wantUpstream)
				}
			}
		})
	}
}

func TestAccessPlanSyncPreservesOverriddenFillFirstOrder(t *testing.T) {
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

	first := model.Channel{Name: "override-first", Enabled: true, Model: "override-model"}
	second := model.Channel{Name: "override-second", Enabled: true, Model: "override-model"}
	if err := ChannelCreate(&first, ctx); err != nil {
		t.Fatalf("create first channel: %v", err)
	}
	if err := ChannelCreate(&second, ctx); err != nil {
		t.Fatalf("create second channel: %v", err)
	}

	saved, err := AccessPlanUpdateRouteTargets(svip.ID, []model.AccessRouteTarget{
		{
			RequestModel:  "override-model",
			ChannelID:     second.ID,
			UpstreamModel: "override-model",
			Priority:      1,
			Weight:        7,
			Enabled:       true,
			Mode:          model.GroupModeFillFirst,
		},
		{
			RequestModel:  "override-model",
			ChannelID:     first.ID,
			UpstreamModel: "override-model",
			Priority:      4,
			Weight:        3,
			Enabled:       true,
			Mode:          model.GroupModeFillFirst,
		},
	}, ctx)
	if err != nil {
		t.Fatalf("save overridden fill-first order: %v", err)
	}
	if len(saved.RouteTargets) != 2 {
		t.Fatalf("expected 2 saved targets, got %d", len(saved.RouteTargets))
	}
	if !saved.RouteTargets[0].PriorityOverridden {
		t.Fatalf("fill-first full save must echo priority_overridden=true")
	}

	third := model.Channel{Name: "override-third", Enabled: true, Model: "override-model"}
	if err := ChannelCreate(&third, ctx); err != nil {
		t.Fatalf("create third channel: %v", err)
	}
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync after new channel: %v", err)
	}

	var rule model.AccessRouteRule
	if err := db.GetDB().WithContext(ctx).
		Where("route_profile_id = ? AND request_model = ?", svip.RouteProfileID, "override-model").
		First(&rule).Error; err != nil {
		t.Fatalf("load rule: %v", err)
	}
	if !rule.PriorityOverridden {
		t.Fatalf("overridden fill-first rule must stay overridden")
	}

	var targets []model.AccessRouteTarget
	if err := db.GetDB().WithContext(ctx).
		Where("route_rule_id = ?", rule.ID).
		Order("priority ASC, channel_id ASC").
		Find(&targets).Error; err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets after append, got %d", len(targets))
	}

	byChannel := map[int]model.AccessRouteTarget{}
	for _, target := range targets {
		byChannel[target.ChannelID] = target
	}
	if got := byChannel[second.ID]; got.Priority != 1 || got.Weight != 7 {
		t.Fatalf("survivor second = %+v, want priority 1 weight 7", got)
	}
	if got := byChannel[first.ID]; got.Priority != 4 || got.Weight != 3 {
		t.Fatalf("survivor first = %+v, want priority 4 weight 3", got)
	}
	if got := byChannel[third.ID]; got.Priority != 5 || got.Weight != 1 {
		t.Fatalf("appended third = %+v, want priority 5 weight 1", got)
	}
}

func TestAccessPlanSyncDeduplicatesSameChannelAndUpdatesUpstream(t *testing.T) {
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

	ch := model.Channel{
		Name:         "dup-channel",
		Enabled:      true,
		Model:        "Canonical-Upstream",
		ModelMapping: map[string]string{"dup-request": "Canonical-Upstream"},
	}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rule := model.AccessRouteRule{
		RouteProfileID:     svip.RouteProfileID,
		RequestModel:       "dup-request",
		Mode:               model.GroupModeFillFirst,
		PriorityOverridden: true,
	}
	if err := AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	keeper := model.AccessRouteTarget{
		RouteRuleID:   rule.ID,
		ChannelID:     ch.ID,
		UpstreamModel: "stale-upstream",
		Priority:      8,
		Weight:        2,
		Enabled:       true,
	}
	if err := AccessRouteTargetCreate(&keeper, ctx); err != nil {
		t.Fatalf("create keeper: %v", err)
	}
	dup := model.AccessRouteTarget{
		RouteRuleID:   rule.ID,
		ChannelID:     ch.ID,
		UpstreamModel: "other-stale",
		Priority:      1,
		Weight:        9,
		Enabled:       true,
	}
	if err := AccessRouteTargetCreate(&dup, ctx); err != nil {
		t.Fatalf("create duplicate: %v", err)
	}

	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var targets []model.AccessRouteTarget
	if err := db.GetDB().WithContext(ctx).
		Where("route_rule_id = ?", rule.ID).
		Find(&targets).Error; err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 keeper after dedupe, got %d", len(targets))
	}
	if targets[0].ID != keeper.ID {
		t.Fatalf("kept id=%d, want lowest id %d", targets[0].ID, keeper.ID)
	}
	if targets[0].UpstreamModel != "Canonical-Upstream" {
		t.Fatalf("upstream = %q, want Canonical-Upstream", targets[0].UpstreamModel)
	}
	if targets[0].Priority != 8 || targets[0].Weight != 2 {
		t.Fatalf("keeper fields changed: %+v", targets[0])
	}
}

func TestAccessPlanUpdateRouteTargetsDedupesAndEchoesOverride(t *testing.T) {
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

	ch := model.Channel{Name: "save-dup-channel", Enabled: true, Model: "save-model"}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	spread, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel:  "save-model",
			ChannelID:     ch.ID,
			UpstreamModel: "save-model",
			Priority:      9,
			Enabled:       true,
			Mode:          model.GroupModeSpread,
		},
		{
			RequestModel:  "save-model",
			ChannelID:     ch.ID,
			UpstreamModel: "ignored-dup",
			Priority:      2,
			Enabled:       true,
			Mode:          model.GroupModeSpread,
		},
	}, ctx)
	if err != nil {
		t.Fatalf("spread save: %v", err)
	}
	if len(spread.RouteTargets) != 1 {
		t.Fatalf("expected first-wins channel dedupe, got %d targets", len(spread.RouteTargets))
	}
	if spread.RouteTargets[0].Priority != 1 {
		t.Fatalf("spread priority = %d, want 1", spread.RouteTargets[0].Priority)
	}
	if spread.RouteTargets[0].PriorityOverridden {
		t.Fatalf("spread save must echo priority_overridden=false")
	}
	if spread.RouteTargets[0].UpstreamModel != "save-model" {
		t.Fatalf("dedupe should keep first upstream, got %q", spread.RouteTargets[0].UpstreamModel)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestAccessPlanTieredSpreadKeepsCanvasNumbers 是「画布上的数字必须真的影响选路」这条需求的回归闸。
// 分层轮询(6) 下管理员抬过的数字，保存路径与同步路径都不许抹平 —— 保存路径此前把整个轮询家族一律
// 归一成 priority=1、同步路径此前按渠道顺序重排优先级；任一条没守住，节点上的 −/+ 就退化成纯装饰。
func TestAccessPlanTieredSpreadKeepsCanvasNumbers(t *testing.T) {
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
		{Name: "tier-channel-a", Enabled: true, Model: "tier-upstream-a"},
		{Name: "tier-channel-b", Enabled: true, Model: "tier-upstream-b"},
	}
	for i := range channels {
		if err := ChannelCreate(&channels[i], ctx); err != nil {
			t.Fatalf("create channel %d: %v", i, err)
		}
	}

	// 画布上把 a 抬到 2、b 留在 1：保存后必须原样落库，而不是被归一成两条 1。
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel: "tier-request", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[0].ID, UpstreamModel: "tier-upstream-a",
			Priority: 2, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
		{
			RequestModel: "tier-request", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[1].ID, UpstreamModel: "tier-upstream-b",
			Priority: 1, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
	}, ctx); err != nil {
		t.Fatalf("save tiered targets: %v", err)
	}

	selected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(selected, "tier-request", ctx)
	if err != nil || !ok {
		t.Fatalf("expected tiered route group, ok=%v err=%v", ok, err)
	}
	if group.Mode != model.GroupModeSpreadTiered {
		t.Fatalf("rule mode = %d, want %d (分层轮询)", group.Mode, model.GroupModeSpreadTiered)
	}
	priorities := map[int]int{}
	for _, item := range group.Items {
		priorities[item.ChannelID] = item.Priority
	}
	if priorities[channels[0].ID] != 2 || priorities[channels[1].ID] != 1 {
		t.Fatalf("画布数字没有传到选路分组: %v", priorities)
	}

	// 纯轮询(1) 仍然忽略条目数字（拖拽序号天然唯一，当硬边界会让轮询退化成固定顺序），
	// 这条老语义必须原样保留，否则既有轮询规则全部变形。
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{{
		RequestModel: "pure-spread-request", Mode: model.GroupModeSpread,
		ChannelID: channels[0].ID, UpstreamModel: "tier-upstream-a",
		Priority: 5, Weight: 1, Enabled: true,
	}}, ctx); err != nil {
		t.Fatalf("save pure spread target: %v", err)
	}
	pureSelected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("re-select vip: %v", err)
	}
	pureGroup, _, ok, err := AccessPlanGroupForModel(pureSelected, "pure-spread-request", ctx)
	if err != nil || !ok {
		t.Fatalf("expected pure spread group, ok=%v err=%v", ok, err)
	}
	if pureGroup.Mode != model.GroupModeSpread {
		t.Fatalf("纯轮询模式被改写: %d", pureGroup.Mode)
	}
	for _, item := range pureGroup.Items {
		if item.Priority != 1 {
			t.Fatalf("纯轮询必须把数字归一成 1, got %d", item.Priority)
		}
	}
}

// TestAccessPlanSyncKeepsTieredSpreadNumbers 守住同步路径：新渠道补进最低层(1)，
// 既有分层数字一个都不动（同步抹平数字 = 画布按钮失效，正是这条需求的根因）。
func TestAccessPlanSyncKeepsTieredSpreadNumbers(t *testing.T) {
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
		t.Fatal("svip plan not found")
	}

	// 前两个渠道已建好分层规则（数字 3 / 1），第三个渠道稍后才启用 -> 应以数字 1 补入。
	channels := []model.Channel{
		{Name: "tier-sync-a", Enabled: true, Model: "sync-upstream-a", ModelMapping: map[string]string{"tier-sync-alias": "sync-upstream-a"}},
		{Name: "tier-sync-b", Enabled: true, Model: "sync-upstream-b", ModelMapping: map[string]string{"tier-sync-alias": "sync-upstream-b"}},
	}
	for i := range channels {
		if err := ChannelCreate(&channels[i], ctx); err != nil {
			t.Fatalf("create channel %d: %v", i, err)
		}
	}
	if _, err := AccessPlanUpdateRouteTargets(svip.ID, []model.AccessRouteTarget{
		{
			RequestModel: "tier-sync-alias", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[0].ID, UpstreamModel: "sync-upstream-a",
			Priority: 3, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
		{
			RequestModel: "tier-sync-alias", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[1].ID, UpstreamModel: "sync-upstream-b",
			Priority: 1, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
	}, ctx); err != nil {
		t.Fatalf("save tiered sync targets: %v", err)
	}

	newcomer := model.Channel{
		Name: "tier-sync-c", Enabled: true, Model: "sync-upstream-c",
		ModelMapping: map[string]string{"tier-sync-alias": "sync-upstream-c"},
	}
	if err := ChannelCreate(&newcomer, ctx); err != nil {
		t.Fatalf("create newcomer channel: %v", err)
	}
	if err := AccessPlanSyncEnabledChannels(ctx); err != nil {
		t.Fatalf("sync enabled channels: %v", err)
	}

	var rule model.AccessRouteRule
	if err := db.GetDB().WithContext(ctx).
		Where("route_profile_id = ? AND request_model = ?", svip.RouteProfileID, "tier-sync-alias").
		First(&rule).Error; err != nil {
		t.Fatalf("expected tiered sync rule: %v", err)
	}
	if rule.Mode != model.GroupModeSpreadTiered {
		t.Fatalf("同步改写了规则模式: %d, want %d", rule.Mode, model.GroupModeSpreadTiered)
	}

	var targets []model.AccessRouteTarget
	if err := db.GetDB().WithContext(ctx).
		Where("route_rule_id = ?", rule.ID).
		Order("channel_id ASC").
		Find(&targets).Error; err != nil {
		t.Fatalf("list synced targets: %v", err)
	}
	got := map[int]int{}
	for _, target := range targets {
		got[target.ChannelID] = target.Priority
	}
	if got[channels[0].ID] != 3 || got[channels[1].ID] != 1 {
		t.Fatalf("同步抹掉了画布数字: %v", got)
	}
	if got[newcomer.ID] != 1 {
		t.Fatalf("新渠道应补进最低层 1, got %d", got[newcomer.ID])
	}
}

// TestAccessPlanTieredSpreadClampsIllegalPriority 守住分层轮询的入参下界：
// 画布的 +/− 不可能发出 0 或负数，但裸 API / 导入 JSON 可以。后端必须把非法数字钳到
// 最低层 1 —— 原样存负数会让这一条排在所有层之后（选路里它永远最后），
// 而 UI 又只显示 1..N，等于存进去一个看不见的坑。
func TestAccessPlanTieredSpreadClampsIllegalPriority(t *testing.T) {
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
		{Name: "clamp-channel-a", Enabled: true, Model: "clamp-upstream-a"},
		{Name: "clamp-channel-b", Enabled: true, Model: "clamp-upstream-b"},
	}
	for i := range channels {
		if err := ChannelCreate(&channels[i], ctx); err != nil {
			t.Fatalf("create channel %d: %v", i, err)
		}
	}

	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel: "clamp-request", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[0].ID, UpstreamModel: "clamp-upstream-a",
			Priority: -7, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
		{
			RequestModel: "clamp-request", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[1].ID, UpstreamModel: "clamp-upstream-b",
			Priority: 0, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
	}, ctx); err != nil {
		t.Fatalf("save clamped targets: %v", err)
	}

	selected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(selected, "clamp-request", ctx)
	if err != nil || !ok {
		t.Fatalf("expected clamped route group, ok=%v err=%v", ok, err)
	}
	if group.Mode != model.GroupModeSpreadTiered {
		t.Fatalf("rule mode = %d, want %d (分层轮询)", group.Mode, model.GroupModeSpreadTiered)
	}
	if len(group.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(group.Items))
	}
	for _, item := range group.Items {
		if item.Priority != 1 {
			t.Fatalf("非法数字必须钳到最低层 1, got %d (channel %d)", item.Priority, item.ChannelID)
		}
	}
}

// TestAccessPlanTieredSpreadModeSurvivesUnspecifiedFirstTarget 守住规则级 mode 的取值：
// 裸 API / 手写 JSON 会让同一 request_model 的第一条 target 漏传 mode（Go/JSON 里都是 0）。
// 建桶时 normalizeAccessRouteRule 会把 0 定成优先填充(3)，所以「只看第一条 target 的 mode」
// 会把整条分层规则静默存成优先填充，画布上的层级数字随之失效。规则级 mode 必须取
// 桶内第一个**显式**传上来的值，与它在数组里的位置无关。
func TestAccessPlanTieredSpreadModeSurvivesUnspecifiedFirstTarget(t *testing.T) {
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
		{Name: "mixed-mode-channel-a", Enabled: true, Model: "mixed-mode-upstream-a"},
		{Name: "mixed-mode-channel-b", Enabled: true, Model: "mixed-mode-upstream-b"},
	}
	for i := range channels {
		if err := ChannelCreate(&channels[i], ctx); err != nil {
			t.Fatalf("create channel %d: %v", i, err)
		}
	}

	// 第一条刻意不传 mode（零值 0），第二条才显式声明分层轮询。
	if _, err := AccessPlanUpdateRouteTargets(vip.ID, []model.AccessRouteTarget{
		{
			RequestModel: "mixed-mode-request",
			ChannelID:    channels[0].ID, UpstreamModel: "mixed-mode-upstream-a",
			Priority: 4, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
		{
			RequestModel: "mixed-mode-request", Mode: model.GroupModeSpreadTiered,
			ChannelID: channels[1].ID, UpstreamModel: "mixed-mode-upstream-b",
			Priority: 2, Weight: 1, Enabled: true, PriorityOverridden: true,
		},
	}, ctx); err != nil {
		t.Fatalf("save mixed-mode targets: %v", err)
	}

	selected, err := AccessPlanSelect(0, "vip", ctx)
	if err != nil {
		t.Fatalf("select vip: %v", err)
	}
	group, _, ok, err := AccessPlanGroupForModel(selected, "mixed-mode-request", ctx)
	if err != nil || !ok {
		t.Fatalf("expected mixed-mode route group, ok=%v err=%v", ok, err)
	}
	if group.Mode != model.GroupModeSpreadTiered {
		t.Fatalf("规则 mode = %d, want %d（第一条漏传 mode 不能把分层吞成优先填充）",
			group.Mode, model.GroupModeSpreadTiered)
	}
	priorities := map[int]int{}
	for _, item := range group.Items {
		priorities[item.ChannelID] = item.Priority
	}
	if priorities[channels[0].ID] != 4 || priorities[channels[1].ID] != 2 {
		t.Fatalf("分层数字必须在混传 mode 时也保留: %v", priorities)
	}
}
