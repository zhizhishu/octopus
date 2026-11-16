package relay

import (
	"reflect"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func TestPromptAudienceFiltersFourBusinessLayersWithoutRewritingClient(t *testing.T) {
	plan, rule, ch := layers(before, before, before)
	plan.PromptAudience = dbmodel.PromptAudienceAdminOnly
	rule.PromptAudience = dbmodel.PromptAudienceAll
	ch.PromptAudience = dbmodel.PromptAudienceAdminOnly

	ordinary := chatReq("CLIENT")
	snap := applyPromptOverridesForAudience(ordinary, plan, rule, ch, false)
	if got := *ordinary.Messages[0].Content.Content; got != "ROUTE\nCLIENT" {
		t.Fatalf("ordinary prompt leaked admin-only text: %q", got)
	}
	if !reflect.DeepEqual(snap.Sources, []string{"route_rule:m"}) {
		t.Fatalf("ordinary snapshot: %#v", snap)
	}

	admin := chatReq("CLIENT")
	snap = applyPromptOverridesForAudience(admin, plan, rule, ch, true)
	if got := *admin.Messages[0].Content.Content; got != "CHAN\nROUTE\nPLAN\nCLIENT" {
		t.Fatalf("admin order lost: %q", got)
	}
	if !reflect.DeepEqual(snap.Sources, []string{"access_plan:vip", "route_rule:m", "channel:ch"}) {
		t.Fatalf("admin snapshot: %#v", snap)
	}

	legacy := chatReq("CLIENT")
	applyPromptOverrides(legacy, plan, rule, ch)
	if got := *legacy.Messages[0].Content.Content; got != "ROUTE\nCLIENT" {
		t.Fatalf("default direct path must fail closed for admin-only: %q", got)
	}
}

func TestPromptAudienceGlobalAdminOnlyAndDefaultFallback(t *testing.T) {
	setupRelayErrorDB(t)
	set := func(k dbmodel.SettingKey, v string) {
		t.Helper()
		if err := op.SettingSetString(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	t.Cleanup(func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(before))
		_ = op.SettingSetString(dbmodel.SettingKeyPromptAudience, string(dbmodel.PromptAudienceAll))
	})
	set(dbmodel.SettingKeyPromptOverrideEnabled, "true")
	set(dbmodel.SettingKeyPromptOverrideSystem, "ADMIN-ONLY")
	set(dbmodel.SettingKeyPromptAudience, "admin_only")

	ordinary := chatReq("USER")
	snap := applyPromptOverridesForAudience(ordinary, nil, nil, nil, false)
	if got := *ordinary.Messages[0].Content.Content; got != "USER" || len(snap.Sources) > 0 {
		t.Fatalf("global admin-only leaked to ordinary: %q %#v", got, snap)
	}
	admin := chatReq("USER")
	snap = applyPromptOverridesForAudience(admin, nil, nil, nil, true)
	if got := *admin.Messages[0].Content.Content; got != "ADMIN-ONLY\nUSER" || len(snap.Sources) != 1 {
		t.Fatalf("global admin should apply: %q %#v", got, snap)
	}
	set(dbmodel.SettingKeyPromptAudience, "all")
	ordinary = chatReq("USER")
	applyPromptOverridesForAudience(ordinary, nil, nil, nil, false)
	if got := *ordinary.Messages[0].Content.Content; got != "ADMIN-ONLY\nUSER" {
		t.Fatalf("all-user setting must retain backwards compatible behavior: %q", got)
	}
}
