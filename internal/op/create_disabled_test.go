package op

import (
	"context"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 新建时显式传 enabled:false 必须真的停用：返回值、数据库、重新读库（等同重启）三处都要是 false。
// 回归：这些表的 Enabled 带 gorm default:true，false 曾被当成"没填"落成 true。

func setupCreateDisabledTest(t *testing.T) (context.Context, model.User) {
	t.Helper()
	ctx := setupAccessPlanTest(t)
	owner := model.User{Username: "owner", Password: "x", Role: model.UserRoleAdmin}
	if err := db.GetDB().Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := userRefreshCache(ctx); err != nil {
		t.Fatalf("refresh users: %v", err)
	}
	return ctx, owner
}

func dbEnabled(t *testing.T, table any, id int) bool {
	t.Helper()
	var row struct{ Enabled bool }
	if err := db.GetDB().Model(table).Select("enabled").Where("id = ?", id).Scan(&row).Error; err != nil {
		t.Fatalf("read enabled: %v", err)
	}
	return row.Enabled
}

func TestAPIKeyCreateKeepsDisabled(t *testing.T) {
	ctx, owner := setupCreateDisabledTest(t)
	off := model.APIKey{UserID: owner.ID, Name: "off", APIKey: "sk-octopus-test-off", Enabled: false}
	if err := APIKeyCreate(&off, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	on := model.APIKey{UserID: owner.ID, Name: "on", APIKey: "sk-octopus-test-on", Enabled: true}
	if err := APIKeyCreate(&on, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if off.Enabled || !on.Enabled {
		t.Fatalf("returned enabled off=%v on=%v", off.Enabled, on.Enabled)
	}
	if dbEnabled(t, &model.APIKey{}, off.ID) || !dbEnabled(t, &model.APIKey{}, on.ID) {
		t.Fatalf("db enabled wrong")
	}
	if got, _ := APIKeyGetByAPIKey("sk-octopus-test-off", ctx); got.Enabled {
		t.Fatalf("cache: off key enabled")
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := APIKeyGetByAPIKey("sk-octopus-test-off", ctx); got.Enabled {
		t.Fatalf("after reload: off key enabled")
	}
	if got, _ := APIKeyGetByAPIKey("sk-octopus-test-on", ctx); !got.Enabled {
		t.Fatalf("after reload: on key disabled")
	}
}

func TestAccessPlanCreateKeepsDisabled(t *testing.T) {
	ctx, _ := setupCreateDisabledTest(t)
	plan := model.AccessPlan{Slug: "offplan", DisplayName: "off", Enabled: false}
	if err := AccessPlanCreate(&plan, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if plan.Enabled || dbEnabled(t, &model.AccessPlan{}, plan.ID) {
		t.Fatalf("plan enabled after create: struct=%v", plan.Enabled)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := accessPlanGetCached(plan.ID, ctx)
	if err != nil || got.Enabled {
		t.Fatalf("after reload: err=%v enabled=%v", err, got.Enabled)
	}
	// 设为默认的方案仍会被强制启用（原有规则不变）。
	def := model.AccessPlan{Slug: "defplan", DisplayName: "def", Enabled: false, IsDefault: true}
	if err := AccessPlanCreate(&def, ctx); err != nil {
		t.Fatalf("create default: %v", err)
	}
	if !dbEnabled(t, &model.AccessPlan{}, def.ID) {
		t.Fatalf("default plan must stay enabled")
	}
}

func TestChannelCreateKeepsDisabled(t *testing.T) {
	ctx, _ := setupCreateDisabledTest(t)
	ch := model.Channel{
		Name: "off-channel", Enabled: false, Model: "m1",
		BaseUrls: []model.BaseUrl{{URL: "https://upstream.example/v1"}},
		Keys: []model.ChannelKey{
			{Enabled: true, ChannelKey: "sk-on"},
			{Enabled: false, ChannelKey: "sk-off"},
		},
	}
	if err := ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ch.Enabled || dbEnabled(t, &model.Channel{}, ch.ID) {
		t.Fatalf("channel enabled after create")
	}
	if !dbEnabled(t, &model.ChannelKey{}, ch.Keys[0].ID) || dbEnabled(t, &model.ChannelKey{}, ch.Keys[1].ID) {
		t.Fatalf("channel key enabled flags wrong in db")
	}
	if ch.Keys[1].Enabled {
		t.Fatalf("returned key 2 enabled")
	}

	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:        ch.ID,
		KeysToAdd: []model.ChannelKeyAddRequest{{Enabled: false, ChannelKey: "sk-added-off"}, {Enabled: true, ChannelKey: "sk-added-on"}},
	}, ctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	var added []model.ChannelKey
	if err := db.GetDB().Where("channel_id = ? AND channel_key IN ?", ch.ID, []string{"sk-added-off", "sk-added-on"}).Order("id").Find(&added).Error; err != nil || len(added) != 2 {
		t.Fatalf("load added keys: err=%v n=%d", err, len(added))
	}
	if added[0].Enabled || !added[1].Enabled {
		t.Fatalf("added keys enabled = %v,%v want false,true", added[0].Enabled, added[1].Enabled)
	}

	if err := channelRefreshCache(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := ChannelGet(ch.ID, ctx)
	if err != nil || got.Enabled {
		t.Fatalf("after reload: err=%v enabled=%v", err, got != nil && got.Enabled)
	}
	for _, k := range got.Keys {
		want := k.ChannelKey == "sk-on" || k.ChannelKey == "sk-added-on"
		if k.Enabled != want {
			t.Fatalf("after reload key %s enabled=%v want %v", k.ChannelKey, k.Enabled, want)
		}
	}
}

func TestRedeemCodeGenerateKeepsDisabled(t *testing.T) {
	ctx, owner := setupCreateDisabledTest(t)
	off := false
	codes, err := RedeemCodeGenerate(model.RedeemCodeGenerateRequest{Type: model.RedeemCodeTypeBalance, Count: 3, BalanceAmount: 1, Enabled: &off}, owner.ID, ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, c := range codes {
		if c.Enabled || dbEnabled(t, &model.RedeemCode{}, c.ID) {
			t.Fatalf("code %d enabled after generate", c.ID)
		}
	}
	if _, err := RedeemCodeRedeem(codes[0].Code, owner.ID, ctx); err == nil {
		t.Fatalf("disabled code must not redeem")
	}
	// 不传 enabled = 启用（老行为）。
	def, err := RedeemCodeGenerate(model.RedeemCodeGenerateRequest{Type: model.RedeemCodeTypeBalance, Count: 1, BalanceAmount: 1}, owner.ID, ctx)
	if err != nil || !def[0].Enabled || !dbEnabled(t, &model.RedeemCode{}, def[0].ID) {
		t.Fatalf("default generate should be enabled: err=%v", err)
	}
}
