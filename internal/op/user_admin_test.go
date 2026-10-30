package op

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func newAdminTestUser(t *testing.T, ctx context.Context, name string, role model.UserRole, balance float64) model.User {
	t.Helper()
	user, err := UserCreate(model.UserCreateRequest{
		Username: name,
		Password: "test-password-123",
		Role:     role,
		Status:   model.UserStatusActive,
		Balance:  balance,
		Note:     "note-" + name,
	}, ctx)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return user
}

func dbBalance(t *testing.T, id int) float64 {
	t.Helper()
	var balance float64
	if err := db.GetDB().Model(&model.User{}).Where("id = ?", id).Pluck("balance", &balance).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// 只发 status 的部分更新不能把备注、余额、月卡、方案清掉。
func TestUserPatchKeepsFieldsNotSent(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "patch-keep", model.UserRoleUser, 12.5)
	limit, expire := 5.0, time.Now().Add(72*time.Hour).Unix()
	if _, err := UserPatch(model.UserPatchRequest{ID: user.ID, MonthlyLimit: &limit, MonthlyExpireAt: &expire}, 0, ctx); err != nil {
		t.Fatalf("set monthly: %v", err)
	}
	before, _ := UserGet(user.ID)
	attachUserAccessPlans(&before)

	disabled := model.UserStatusDisabled
	got, err := UserPatch(model.UserPatchRequest{ID: user.ID, Status: &disabled}, 0, ctx)
	if err != nil {
		t.Fatalf("patch status: %v", err)
	}
	if got.Status != model.UserStatusDisabled {
		t.Fatalf("status not applied: %s", got.Status)
	}
	if got.Note != "note-patch-keep" || got.Balance != 12.5 || got.MonthlyLimit != limit || got.MonthlyExpireAt != expire || got.Username != "patch-keep" {
		t.Fatalf("partial update wiped fields: %#v", got)
	}
	if len(got.AccessPlanIDs) == 0 || len(got.AccessPlanIDs) != len(before.AccessPlanIDs) {
		t.Fatalf("partial update changed plans: before %v after %v", before.AccessPlanIDs, got.AccessPlanIDs)
	}
	var row model.User
	if err := db.GetDB().First(&row, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Note != "note-patch-keep" || row.Balance != 12.5 || row.MonthlyLimit != limit {
		t.Fatalf("db row wiped: %#v", row)
	}
}

// 余额加减和扣费同时发生，一分钱都不能丢。
func TestUserBalanceAdjustConcurrentWithCharges(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "concurrent", model.UserRoleUser, 100)

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, 3*n)
	for i := 0; i < n; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs <- UserRecordUsage(user.ID, 0.5, ctx)
		}()
		go func() {
			defer wg.Done()
			_, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: user.ID, Mode: "add", Amount: 1}, 0, ctx)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: user.ID, Mode: "subtract", Amount: 0.25}, 0, ctx)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("op failed: %v", err)
		}
	}
	want := 100 + n*1.0 - n*0.5 - n*0.25
	cached, _ := UserGet(user.ID)
	if !nearly(cached.Balance, want) || !nearly(dbBalance(t, user.ID), want) {
		t.Fatalf("lost update: cache %v db %v want %v", cached.Balance, dbBalance(t, user.ID), want)
	}
	logs, err := UserBalanceLogList(user.ID, 200, ctx)
	if err != nil || len(logs) != 2*n {
		t.Fatalf("want %d balance logs, got %d (%v)", 2*n, len(logs), err)
	}
}

func TestUserBalanceAdjustModesAndLog(t *testing.T) {
	ctx := setupUserTest(t)
	admin := newAdminTestUser(t, ctx, "boss", model.UserRoleAdmin, 0)
	user := newAdminTestUser(t, ctx, "modes", model.UserRoleUser, 10)

	if _, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: user.ID, Mode: "subtract", Amount: 10.01}, admin.ID, ctx); err == nil {
		t.Fatal("subtracting more than the balance must fail")
	}
	if !nearly(dbBalance(t, user.ID), 10) {
		t.Fatalf("failed subtract changed balance: %v", dbBalance(t, user.ID))
	}
	for _, bad := range []model.UserBalanceAdjustRequest{
		{ID: user.ID, Mode: "add", Amount: 0},
		{ID: user.ID, Mode: "add", Amount: -1},
		{ID: user.ID, Mode: "set", Amount: -1},
		{ID: user.ID, Mode: "double", Amount: 1},
		{ID: user.ID, Mode: "add", Amount: math.Inf(1)},
	} {
		if _, _, err := UserBalanceAdjust(bad, admin.ID, ctx); err == nil {
			t.Fatalf("expected %#v to fail", bad)
		}
	}
	got, entry, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: user.ID, Mode: "set", Amount: 3, Reason: "  线下转账  "}, admin.ID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Balance != 3 || entry.BalanceBefore != 10 || entry.BalanceAfter != 3 || entry.Delta != -7 || entry.Reason != "线下转账" || entry.ActorName != "boss" || entry.Mode != "set" {
		t.Fatalf("bad set result: user %v entry %#v", got.Balance, entry)
	}
	logs, _ := UserBalanceLogList(user.ID, 10, ctx)
	if len(logs) != 1 || logs[0].ID != entry.ID {
		t.Fatalf("failed ops must not be logged; got %#v", logs)
	}
}

// 加了余额以后，有效月卡仍然先扣月卡；管理员仍然不扣费。
func TestUserBalanceAdjustKeepsChargeRules(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "monthly", model.UserRoleUser, 1)
	limit, expire := 5.0, time.Now().Add(48*time.Hour).Unix()
	if _, err := UserPatch(model.UserPatchRequest{ID: user.ID, MonthlyLimit: &limit, MonthlyExpireAt: &expire}, 0, ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: user.ID, Mode: "add", Amount: 4}, 0, ctx); err != nil {
		t.Fatal(err)
	}
	if err := UserRecordUsage(user.ID, 2, ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 5 || got.MonthlyUsed != 2 {
		t.Fatalf("monthly card must be charged first: balance %v used %v", got.Balance, got.MonthlyUsed)
	}

	admin := newAdminTestUser(t, ctx, "free-admin", model.UserRoleAdmin, 0)
	if _, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{ID: admin.ID, Mode: "add", Amount: 2}, 0, ctx); err != nil {
		t.Fatal(err)
	}
	if err := UserRecordUsage(admin.ID, 1, ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := UserGet(admin.ID); got.Balance != 2 {
		t.Fatalf("admin must not be charged, balance %v", got.Balance)
	}
}

// 并发批量停用所有管理员：至少要留一个。
func TestUserBatchCannotDisableEveryAdmin(t *testing.T) {
	ctx := setupUserTest(t)
	a := newAdminTestUser(t, ctx, "adm-a", model.UserRoleAdmin, 0)
	b := newAdminTestUser(t, ctx, "adm-b", model.UserRoleAdmin, 0)
	c := newAdminTestUser(t, ctx, "adm-c", model.UserRoleAdmin, 0)

	var wg sync.WaitGroup
	for _, id := range []int{a.ID, b.ID, c.ID} {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = UserBatch(model.UserBatchRequest{IDs: []int{id}, Action: model.UserBatchDisable}, 0, ctx)
		}(id)
	}
	wg.Wait()
	var active int64
	if err := db.GetDB().Model(&model.User{}).Where("role = ? AND status = ?", model.UserRoleAdmin, model.UserStatusActive).Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active < 1 {
		t.Fatal("parallel batch disable removed every active admin")
	}

	user := newAdminTestUser(t, ctx, "plain", model.UserRoleUser, 0)
	res, err := UserBatch(model.UserBatchRequest{IDs: []int{user.ID, user.ID, 999999}, Action: model.UserBatchDisable}, 0, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Done) != 1 || res.Done[0] != user.ID || len(res.Failed) != 1 || res.Failed[0].ID != 999999 {
		t.Fatalf("unexpected batch result %#v", res)
	}
	if _, err := UserBatch(model.UserBatchRequest{IDs: []int{user.ID}, Action: "explode"}, 0, ctx); err == nil {
		t.Fatal("unknown action must fail")
	}
}

func TestUserUsageRangeDailyAndTopModels(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "usage", model.UserRoleUser, 0)
	other := newAdminTestUser(t, ctx, "other", model.UserRoleUser, 0)

	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.Local)
	day := func(back int) int64 { return now.AddDate(0, 0, -back).Unix() }
	rows := []model.RelayLog{
		{ID: 1, UserID: user.ID, Time: day(0), RequestModelName: "m-big", InputTokens: 100, OutputTokens: 10, Cost: 3},
		{ID: 2, UserID: user.ID, Time: day(0), RequestModelName: "m-small", InputTokens: 10, OutputTokens: 1, Cost: 1},
		{ID: 3, UserID: user.ID, Time: day(2), RequestModelName: "m-big", InputTokens: 50, OutputTokens: 5, FinalInputCost: 1, FinalOutputCost: 1, Error: "boom"},
		{ID: 4, UserID: user.ID, Time: day(10), RequestModelName: "m-old", Cost: 7},
		{ID: 5, UserID: other.ID, Time: day(0), RequestModelName: "m-big", Cost: 100},
		// 客户端自己断开：与模型排行同口径，不算。
		{ID: 6, UserID: user.ID, Time: day(0), RequestModelName: "m-big", Cost: 50, Error: "canceled", ErrorCode: "octopus_client_canceled"},
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// 还没落库的内存日志也要算进去。
	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, model.RelayLog{ID: 7, UserID: user.ID, Time: day(1), RequestModelName: "m-small", Cost: 0.5})
	relayLogCacheLock.Unlock()
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = relayLogCache[:0]
		relayLogCacheLock.Unlock()
	})

	week, err := userUsageAt(user.ID, "7d", now, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if week.RequestSuccess != 3 || week.RequestFailed != 1 || !nearly(week.TotalCost, 6.5) || week.InputTokens != 160 || week.OutputTokens != 16 {
		t.Fatalf("bad 7d totals %#v", week)
	}
	if len(week.Daily) != 7 || week.Daily[6].Date != "2026-10-30" || !nearly(week.Daily[6].Cost, 4) || week.Daily[6].Requests != 2 || !nearly(week.Daily[5].Cost, 0.5) || !nearly(week.Daily[4].Cost, 2) {
		t.Fatalf("bad daily series %#v", week.Daily)
	}
	if len(week.TopModels) != 2 || week.TopModels[0].Model != "m-big" || !nearly(week.TopModels[0].Cost, 5) || week.TopModels[0].Requests != 2 {
		t.Fatalf("bad top models %#v", week.TopModels)
	}

	today, _ := userUsageAt(user.ID, "today", now, ctx)
	if today.RequestSuccess+today.RequestFailed != 2 || len(today.Daily) != 1 {
		t.Fatalf("bad today %#v", today)
	}
	all, _ := userUsageAt(user.ID, "all", now, ctx)
	if !nearly(all.TotalCost, 13.5) || len(all.Daily) != 30 {
		t.Fatalf("bad all %#v", all)
	}
	if _, err := userUsageAt(user.ID, "1y", now, ctx); err == nil {
		t.Fatal("unknown range must fail")
	}
}

// 兑换码 / 签到 / 加余额也走 balance = balance + ?，与扣费并发时不丢钱。
func TestUserApplyBalanceConcurrentWithCharges(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "apply", model.UserRoleUser, 50)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = UserApplyBalance(user.ID, 1, ctx) }()
		go func() { defer wg.Done(); _ = UserRecordUsage(user.ID, 1, ctx) }()
	}
	wg.Wait()
	cached, _ := UserGet(user.ID)
	if !nearly(cached.Balance, 50) || !nearly(dbBalance(t, user.ID), 50) {
		t.Fatalf("lost update: cache %v db %v", cached.Balance, dbBalance(t, user.ID))
	}
}

func TestRedeemBalanceCodeConcurrentWithCharges(t *testing.T) {
	ctx := setupUserTest(t)
	user := newAdminTestUser(t, ctx, "redeemer", model.UserRoleUser, 20)
	codes, err := RedeemCodeGenerate(model.RedeemCodeGenerateRequest{Type: model.RedeemCodeTypeBalance, Count: 20, BalanceAmount: 1}, 1, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, code := range codes {
		wg.Add(2)
		go func(c string) {
			defer wg.Done()
			if _, err := RedeemCodeRedeem(c, user.ID, ctx); err != nil {
				t.Errorf("redeem: %v", err)
			}
		}(code.Code)
		go func() { defer wg.Done(); _ = UserRecordUsage(user.ID, 1, ctx) }()
	}
	wg.Wait()
	cached, _ := UserGet(user.ID)
	if !nearly(cached.Balance, 20) || !nearly(dbBalance(t, user.ID), 20) {
		t.Fatalf("lost update: cache %v db %v", cached.Balance, dbBalance(t, user.ID))
	}
}
