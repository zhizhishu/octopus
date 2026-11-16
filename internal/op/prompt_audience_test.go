package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func TestPromptAudiencePersistsAndRoutingEditsKeepIt(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	plan := planBySlug(t, ctx, "vip")
	admin := model.PromptAudienceAdminOnly

	updated, err := AccessPlanUpdatePrompt(plan.ID, model.PromptOverridePatch{
		SystemPromptOverride:  strp("trial-only"),
		PromptOverrideEnabled: boolp(true),
		PromptAudience:        &admin,
	}, ctx)
	if err != nil || updated.PromptAudience != admin {
		t.Fatalf("plan audience update: %+v %v", updated.PromptAudience, err)
	}
	if err := AccessPlanUpdate(&model.AccessPlan{
		ID: plan.ID, Slug: plan.Slug, DisplayName: plan.DisplayName,
		Enabled: true, IsDefault: plan.IsDefault,
	}, ctx); err != nil {
		t.Fatalf("plan save: %v", err)
	}
	var persisted model.AccessPlan
	if err := db.GetDB().WithContext(ctx).First(&persisted, plan.ID).Error; err != nil || persisted.PromptAudience != admin {
		t.Fatalf("normal plan save wiped audience: %+v %v", persisted.PromptAudience, err)
	}

	chA, chB := promptTestChannels(t, ctx)
	targets := planPageRouteTable(chA, chB, model.GroupModeFillFirst)
	if _, err := AccessPlanUpdateRouteTargets(plan.ID, targets, ctx); err != nil {
		t.Fatalf("initial routes: %v", err)
	}
	_, err = AccessPlanUpdateRoutePrompt(plan.ID, "m-a", model.PromptOverridePatch{
		SystemPromptOverride: strp("route-private"), PromptOverrideEnabled: boolp(true),
		PromptAudience: &admin,
	}, ctx)
	if err != nil {
		t.Fatalf("save route audience: %v", err)
	}
	// The route editing UI emits the route table again without prompt fields.
	if _, err := AccessPlanUpdateRouteTargets(plan.ID, targets, ctx); err != nil {
		t.Fatalf("normal route-table save: %v", err)
	}
	got := planBySlug(t, ctx, "vip")
	found := false
	for _, target := range got.RouteTargets {
		if target.RequestModel == "m-a" {
			found = true
			if target.PromptAudience != admin {
				t.Fatalf("route target lost audience after table rebuild: %q", target.PromptAudience)
			}
		}
	}
	if !found {
		t.Fatal("missing m-a route")
	}

	if _, err := AccessPlanUpdatePrompt(plan.ID, model.PromptOverridePatch{
		PromptAudience: ptrPromptAudience("svip"),
	}, ctx); err == nil {
		t.Fatal("invalid plan role/plan slug treated as privileged audience")
	}
}

func ptrPromptAudience(value model.PromptAudience) *model.PromptAudience { return &value }

func TestChannelPromptAudienceUpdateAndLegacyDefault(t *testing.T) {
	ctx := setupAccessPlanTest(t)
	ch := model.Channel{Name: "prompt-audience-test", Enabled: true, Model: "m"}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatal(err)
	}
	if ch.PromptAudience != "" && ch.PromptAudience != model.PromptAudienceAll {
		t.Fatalf("created channel audience unexpected: %q", ch.PromptAudience)
	}
	admin := model.PromptAudienceAdminOnly
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID: ch.ID, PromptAudience: &admin,
	}, ctx); err != nil {
		t.Fatal(err)
	}
	var persisted model.Channel
	if err := db.GetDB().WithContext(ctx).First(&persisted, ch.ID).Error; err != nil || persisted.PromptAudience != admin {
		t.Fatalf("channel audience not stored: %q %v", persisted.PromptAudience, err)
	}
	invalid := model.PromptAudience("premium_user")
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{ID: ch.ID, PromptAudience: &invalid}, ctx); err == nil {
		t.Fatal("invalid channel audience accepted")
	}
}
