package op

import (
	"context"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func setupAPIKeyAdminTest(t *testing.T) (context.Context, model.User, model.User, model.User) {
	t.Helper()
	ctx := setupAccessPlanTest(t)
	if err := userRefreshCache(ctx); err != nil {
		t.Fatalf("refresh users: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("refresh keys: %v", err)
	}
	admin := newAdminTestUser(t, ctx, "boss", model.UserRoleAdmin, 0)
	alice := newAdminTestUser(t, ctx, "alice", model.UserRoleUser, 0)
	bob := newAdminTestUser(t, ctx, "bob", model.UserRoleUser, 0)
	return ctx, admin, alice, bob
}

func newTestKey(t *testing.T, ctx context.Context, owner model.User, name string) model.APIKey {
	t.Helper()
	key := model.APIKey{UserID: owner.ID, Name: name, APIKey: "sk-octopus-" + name, Enabled: true}
	if err := APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create key %s: %v", name, err)
	}
	return key
}

func TestAPIKeyBatchEnableDisableDeleteScoped(t *testing.T) {
	ctx, admin, alice, bob := setupAPIKeyAdminTest(t)
	a1 := newTestKey(t, ctx, alice, "a1")
	a2 := newTestKey(t, ctx, alice, "a2")
	b1 := newTestKey(t, ctx, bob, "b1")

	// alice 停用自己的两把 + bob 的一把：bob 的那把失败，不影响她自己的。
	res, err := APIKeyBatch(model.APIKeyBatchRequest{IDs: []int{a1.ID, a2.ID, b1.ID, a1.ID, 0}, Action: "disable"}, alice, ctx)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Done) != 2 || len(res.Failed) != 1 || res.Failed[0].ID != b1.ID {
		t.Fatalf("unexpected result %+v", res)
	}
	for _, id := range []int{a1.ID, a2.ID} {
		if dbEnabled(t, &model.APIKey{}, id) {
			t.Fatalf("key %d still enabled in db", id)
		}
		if k, _ := APIKeyGet(id, ctx); k.Enabled {
			t.Fatalf("key %d still enabled in cache", id)
		}
	}
	if !dbEnabled(t, &model.APIKey{}, b1.ID) {
		t.Fatal("bob's key must be untouched")
	}

	// 管理员能动所有人的；启用后重新读库也是启用。
	res, err = APIKeyBatch(model.APIKeyBatchRequest{IDs: []int{a1.ID, b1.ID}, Action: "enable"}, admin, ctx)
	if err != nil || len(res.Done) != 2 {
		t.Fatalf("admin enable: %+v %v", res, err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	if k, _ := APIKeyGet(a1.ID, ctx); !k.Enabled {
		t.Fatal("a1 should be enabled after reload")
	}

	// 删除与单个删除一样（同一个函数）。
	res, err = APIKeyBatch(model.APIKeyBatchRequest{IDs: []int{a2.ID, b1.ID}, Action: "delete"}, alice, ctx)
	if err != nil || len(res.Done) != 1 || res.Done[0] != a2.ID || len(res.Failed) != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if _, err := APIKeyGet(a2.ID, ctx); err == nil {
		t.Fatal("a2 should be gone")
	}
	var n int64
	db.GetDB().Model(&model.APIKey{}).Where("id = ?", a2.ID).Count(&n)
	if n != 0 {
		t.Fatal("a2 still in db")
	}

	if _, err := APIKeyBatch(model.APIKeyBatchRequest{IDs: []int{a1.ID}, Action: "rotate"}, admin, ctx); err == nil {
		t.Fatal("unknown action must fail")
	}
	if _, err := APIKeyBatch(model.APIKeyBatchRequest{IDs: nil, Action: "enable"}, admin, ctx); err == nil {
		t.Fatal("empty ids must fail")
	}
}

func TestAPIKeyRecordUseSurvivesSaveAndReload(t *testing.T) {
	ctx, admin, alice, _ := setupAPIKeyAdminTest(t)
	key := newTestKey(t, ctx, alice, "used")

	APIKeyRecordUse(key.ID, "203.0.113.24", 1_800_000_000)
	APIKeyRecordUse(key.ID, "198.51.100.7", 1_700_000_000) // 更早的一次不能把新的盖掉
	got, _ := APIKeyGet(key.ID, ctx)
	if got.LastUsedAt != 1_800_000_000 || got.LastUsedIP != "203.0.113.24" {
		t.Fatalf("cache last used = %d %q", got.LastUsedAt, got.LastUsedIP)
	}

	// 还没落库时从库重载缓存，不能丢。
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := APIKeyGet(key.ID, ctx); got.LastUsedAt != 1_800_000_000 {
		t.Fatalf("lost pending last used on reload: %d", got.LastUsedAt)
	}

	// 编辑保存时请求体里没有 last_used_*，不能被清零（缓存和库都不能）。
	edit := got
	edit.Name = "renamed"
	edit.LastUsedAt = 0
	edit.LastUsedIP = ""
	if err := APIKeyUpdate(&edit, admin, ctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ := APIKeyGet(key.ID, ctx); got.LastUsedAt != 1_800_000_000 || got.Name != "renamed" {
		t.Fatalf("update wiped last used: %+v", got)
	}

	if err := APIKeyLastUsedSaveDB(ctx); err != nil {
		t.Fatalf("save: %v", err)
	}
	var row model.APIKey
	if err := db.GetDB().First(&row, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastUsedAt != 1_800_000_000 || row.LastUsedIP != "203.0.113.24" || row.Name != "renamed" {
		t.Fatalf("db row %+v", row)
	}
	// 落库后再编辑一次，库里的值仍保留。
	edit.Name = "again"
	if err := APIKeyUpdate(&edit, admin, ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().First(&row, key.ID).Error; err != nil || row.LastUsedAt != 1_800_000_000 {
		t.Fatalf("db last used after second edit = %d (%v)", row.LastUsedAt, err)
	}
	APIKeyRecordUse(0, "1.1.1.1", 0) // 无 Key 的请求不记
}

func TestAPIKeyUsageScopedToKey(t *testing.T) {
	ctx, admin, alice, bob := setupAPIKeyAdminTest(t)
	k1 := newTestKey(t, ctx, alice, "k1")
	k2 := newTestKey(t, ctx, alice, "k2")
	kb := newTestKey(t, ctx, bob, "kb")

	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.Local)
	rows := []model.RelayLog{
		{ID: 1, UserID: alice.ID, APIKeyID: k1.ID, Time: now.Unix(), RequestModelName: "m-a", InputTokens: 10, OutputTokens: 1, Cost: 2},
		{ID: 2, UserID: alice.ID, APIKeyID: k1.ID, Time: now.AddDate(0, 0, -1).Unix(), RequestModelName: "m-b", Cost: 1, Error: "boom"},
		{ID: 3, UserID: alice.ID, APIKeyID: k2.ID, Time: now.Unix(), RequestModelName: "m-a", Cost: 5},
		{ID: 4, UserID: bob.ID, APIKeyID: kb.ID, Time: now.Unix(), RequestModelName: "m-a", Cost: 9},
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, model.RelayLog{ID: 5, UserID: alice.ID, APIKeyID: k1.ID, Time: now.Unix(), RequestModelName: "m-a", Cost: 0.5})
	relayLogCacheLock.Unlock()
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = relayLogCache[:0]
		relayLogCacheLock.Unlock()
	})

	u, err := apiKeyUsageAt(k1.ID, "7d", alice, now, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.APIKeyID != k1.ID || u.UserID != alice.ID || u.RequestSuccess != 2 || u.RequestFailed != 1 || !nearly(u.TotalCost, 3.5) || len(u.Daily) != 7 {
		t.Fatalf("bad k1 usage %+v", u)
	}
	if len(u.TopModels) != 2 || u.TopModels[0].Model != "m-a" {
		t.Fatalf("bad top models %+v", u.TopModels)
	}
	// 用户用量不受这次重构影响：alice 两把 Key 合计。
	uu, err := userUsageAt(alice.ID, "7d", now, ctx)
	if err != nil || !nearly(uu.TotalCost, 8.5) || uu.APIKeyID != 0 || uu.UserID != alice.ID {
		t.Fatalf("user usage %+v %v", uu, err)
	}
	if _, err := apiKeyUsageAt(kb.ID, "7d", alice, now, ctx); err == nil {
		t.Fatal("alice must not read bob's key usage")
	}
	if got, err := apiKeyUsageAt(kb.ID, "today", admin, now, ctx); err != nil || !nearly(got.TotalCost, 9) {
		t.Fatalf("admin read bob key: %+v %v", got, err)
	}
	if _, err := apiKeyUsageAt(k1.ID, "1y", alice, now, ctx); err == nil {
		t.Fatal("unknown range must fail")
	}
}

func TestAPIKeyModelsForUserFollowsUserPlans(t *testing.T) {
	ctx, _, alice, bob := setupAPIKeyAdminTest(t)
	plans, err := AccessPlanList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]model.AccessPlan{}
	for _, plan := range plans {
		bySlug[plan.Slug] = plan
	}
	channel := model.Channel{Name: "models-for-user", Enabled: true, Model: "vip-up,svip-up"}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}
	for slug, req := range map[string]string{"vip": "vip-model", "svip": "svip-model"} {
		if _, err := AccessPlanUpdateRouteTargets(bySlug[slug].ID, []model.AccessRouteTarget{{
			RequestModel: req, ChannelID: channel.ID, UpstreamModel: slug + "-up", Enabled: true,
		}}, ctx); err != nil {
			t.Fatalf("route %s: %v", slug, err)
		}
	}
	if err := UserAccessPlanSet(alice.ID, []int{bySlug["vip"].ID}, bySlug["vip"].ID, ctx); err != nil {
		t.Fatal(err)
	}
	if err := UserAccessPlanSet(bob.ID, []int{bySlug["vip"].ID, bySlug["svip"].ID}, bySlug["vip"].ID, ctx); err != nil {
		t.Fatal(err)
	}
	am, err := APIKeyModelsForUser(alice.ID, ctx)
	if err != nil || !containsString(am, "vip-model") || containsString(am, "svip-model") {
		t.Fatalf("alice models %v %v", am, err)
	}
	bm, _ := APIKeyModelsForUser(bob.ID, ctx)
	if !containsString(bm, "vip-model") || !containsString(bm, "svip-model") {
		t.Fatalf("bob models %v", bm)
	}
	if _, err := APIKeyModelsForUser(99999, ctx); err == nil {
		t.Fatal("unknown user must fail")
	}
}
