package op

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupUserTest(t *testing.T) context.Context {
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
	if err := userRefreshCache(ctx); err != nil {
		t.Fatalf("refresh user cache: %v", err)
	}
	return ctx
}

func TestUserRegisterRequiresInviteByDefaultAndAppliesBalance(t *testing.T) {
	ctx := setupUserTest(t)

	if _, err := UserRegister(model.UserRegister{
		Username: "alice",
		Password: "secret",
	}, "203.0.113.10", ctx); err == nil {
		t.Fatalf("expected registration without invite to fail")
	}

	codes, err := RedeemCodeGenerate(model.RedeemCodeGenerateRequest{
		Type:          model.RedeemCodeTypeBalance,
		Count:         1,
		BalanceAmount: 7.5,
	}, 1, ctx)
	if err != nil {
		t.Fatalf("generate invite code: %v", err)
	}

	user, err := UserRegister(model.UserRegister{
		Username:   "alice",
		Password:   "secret",
		InviteCode: codes[0].Code,
	}, "203.0.113.10", ctx)
	if err != nil {
		t.Fatalf("register with invite: %v", err)
	}
	if user.Balance != 7.5 {
		t.Fatalf("expected invite balance 7.5, got %v", user.Balance)
	}
	if user.RegisterIP != "203.0.113.10" {
		t.Fatalf("expected register ip recorded, got %q", user.RegisterIP)
	}

	var code model.RedeemCode
	if err := db.GetDB().WithContext(ctx).Where("code = ?", codes[0].Code).First(&code).Error; err != nil {
		t.Fatalf("load code: %v", err)
	}
	if !code.Used || code.UsedByUserID != user.ID {
		t.Fatalf("expected invite code consumed by new user, got %#v", code)
	}

	if _, err := UserVerify("alice", "secret"); err != nil {
		t.Fatalf("registered user should login: %v", err)
	}
}

func TestUserRegisterOpenRegistrationAndRelayIP(t *testing.T) {
	ctx := setupUserTest(t)

	if err := SettingSetString(model.SettingKeyUserRegistrationEnabled, "true"); err != nil {
		t.Fatalf("enable open registration: %v", err)
	}

	user, err := UserRegister(model.UserRegister{
		Username: "bob",
		Password: "secret",
	}, "198.51.100.8", ctx)
	if err != nil {
		t.Fatalf("register without invite: %v", err)
	}
	if user.Balance != 0 {
		t.Fatalf("expected no free balance for open registration, got %v", user.Balance)
	}

	if err := UserRecordRelayIP(user.ID, "198.51.100.99", 12345, ctx); err != nil {
		t.Fatalf("record relay ip: %v", err)
	}
	user, err = UserGet(user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.LastRelayIP != "198.51.100.99" || user.LastRelayAt != 12345 {
		t.Fatalf("expected relay ip audit fields, got ip=%q at=%d", user.LastRelayIP, user.LastRelayAt)
	}
}

// relay IP 审计改成异步: UserRecordRelayIP 只更新内存缓存(读接口立刻看到新值),
// DB 由 UserRelayIPSaveDB 批量补齐, 转发路径上不再每请求 UPDATE users。
func TestUserRecordRelayIPDefersDatabaseWrite(t *testing.T) {
	ctx := setupUserTest(t)

	userRelayIPPendingLock.Lock()
	userRelayIPPending = make(map[int]userRelayIPRecord)
	userRelayIPPendingLock.Unlock()

	user, err := UserCreate(model.UserCreateRequest{
		Username: "relay-ip-async",
		Password: "secret",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := UserRecordRelayIP(user.ID, "203.0.113.77", 20260804, ctx); err != nil {
		t.Fatalf("record relay ip: %v", err)
	}

	cached, err := UserGet(user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if cached.LastRelayIP != "203.0.113.77" || cached.LastRelayAt != 20260804 {
		t.Fatalf("expected cached user to carry the fresh relay ip, got ip=%q at=%d", cached.LastRelayIP, cached.LastRelayAt)
	}

	var stored model.User
	if err := db.GetDB().WithContext(ctx).Where("id = ?", user.ID).First(&stored).Error; err != nil {
		t.Fatalf("load user row: %v", err)
	}
	if stored.LastRelayIP != "" || stored.LastRelayAt != 0 {
		t.Fatalf("expected no synchronous db write, got ip=%q at=%d", stored.LastRelayIP, stored.LastRelayAt)
	}

	if err := UserRelayIPSaveDB(ctx); err != nil {
		t.Fatalf("save relay ip: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Where("id = ?", user.ID).First(&stored).Error; err != nil {
		t.Fatalf("reload user row: %v", err)
	}
	if stored.LastRelayIP != "203.0.113.77" || stored.LastRelayAt != 20260804 {
		t.Fatalf("expected batched relay ip to land in db, got ip=%q at=%d", stored.LastRelayIP, stored.LastRelayAt)
	}

	userRelayIPPendingLock.Lock()
	remaining := len(userRelayIPPending)
	userRelayIPPendingLock.Unlock()
	if remaining != 0 {
		t.Fatalf("expected pending relay ip queue drained, got %d", remaining)
	}
}

func TestUserCheckInFixedReward(t *testing.T) {
	ctx := setupUserTest(t)
	user, err := UserCreate(model.UserCreateRequest{
		Username: "checkin-fixed",
		Password: "secret",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Date(2026, 5, 17, 9, 30, 0, 0, time.UTC)
	status, err := UserCheckInStatusAt(user.ID, now, ctx)
	if err != nil {
		t.Fatalf("check status: %v", err)
	}
	if status.Enabled || status.CheckedToday {
		t.Fatalf("expected check-in disabled and unchecked by default, got %#v", status)
	}
	if _, err := UserCheckInAt(user.ID, now, ctx); err == nil {
		t.Fatalf("expected disabled check-in to fail")
	}

	if err := SettingSetString(model.SettingKeyCheckInEnabled, "true"); err != nil {
		t.Fatalf("enable check-in: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInRewardMode, string(model.CheckInRewardModeFixed)); err != nil {
		t.Fatalf("set fixed mode: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInRewardAmount, "12.5"); err != nil {
		t.Fatalf("set amount: %v", err)
	}

	result, err := UserCheckInAt(user.ID, now, ctx)
	if err != nil {
		t.Fatalf("check in: %v", err)
	}
	if result.Reward != 12.5 || result.User.Balance != 12.5 || !result.Status.CheckedToday {
		t.Fatalf("unexpected check-in response: %#v", result)
	}
	if _, err := UserCheckInAt(user.ID, now, ctx); err == nil {
		t.Fatalf("expected duplicate same-day check-in to fail")
	}
	status, err = UserCheckInStatusAt(user.ID, now, ctx)
	if err != nil {
		t.Fatalf("check status after check-in: %v", err)
	}
	if !status.CheckedToday || status.LastAmount != 12.5 || status.Balance != 12.5 {
		t.Fatalf("expected status to reflect today's reward, got %#v", status)
	}
}

func TestUserCheckInRandomRewardRange(t *testing.T) {
	ctx := setupUserTest(t)
	user, err := UserCreate(model.UserCreateRequest{
		Username: "checkin-random",
		Password: "secret",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInEnabled, "true"); err != nil {
		t.Fatalf("enable check-in: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInRewardMode, string(model.CheckInRewardModeRandom)); err != nil {
		t.Fatalf("set random mode: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInRewardMin, "100"); err != nil {
		t.Fatalf("set min: %v", err)
	}
	if err := SettingSetString(model.SettingKeyCheckInRewardMax, "200"); err != nil {
		t.Fatalf("set max: %v", err)
	}

	result, err := UserCheckInAt(user.ID, time.Date(2026, 5, 18, 9, 30, 0, 0, time.UTC), ctx)
	if err != nil {
		t.Fatalf("random check-in: %v", err)
	}
	if result.Reward < 100 || result.Reward > 200 {
		t.Fatalf("expected random reward within [100, 200], got %v", result.Reward)
	}
	if result.User.Balance != result.Reward {
		t.Fatalf("expected balance to equal reward, got balance=%v reward=%v", result.User.Balance, result.Reward)
	}
}

func TestUserMonthlyQuotaSummaryUsesDailyQuotaFields(t *testing.T) {
	now := time.Date(2026, 5, 14, 10, 30, 0, 0, time.UTC)
	nextReset := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	user := model.User{
		MonthlyLimit:    10,
		MonthlyUsed:     3.25,
		MonthlyExpireAt: now.Add(48 * time.Hour).Unix(),
		MonthlyResetAt:  now.AddDate(0, 1, 0).Unix(),
	}

	summary := user.MonthlyQuotaSummary(now)
	if !summary.MonthlyActive || summary.MonthlyStatus != "active" {
		t.Fatalf("expected active monthly card, got %#v", summary)
	}
	if summary.DailyLimit != 10 || summary.DailyUsed != 3.25 || math.Abs(summary.DailyRemaining-6.75) > 0.000001 {
		t.Fatalf("unexpected daily quota summary: %#v", summary)
	}
	if summary.DaysLeft != 2 {
		t.Fatalf("expected 2 days left, got %d", summary.DaysLeft)
	}
	if summary.NextResetAtISO != nextReset || summary.MonthlyResetAtISO != nextReset {
		t.Fatalf("expected stale monthly reset to be shown as next daily reset %s, got %#v", nextReset, summary)
	}
}

func TestUserUpdateAcceptsHumanMonthlyAliases(t *testing.T) {
	ctx := setupUserTest(t)
	user, err := UserCreate(model.UserCreateRequest{
		Username: "daily-alias",
		Password: "secret",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	dailyLimit := 8.5
	dailyUsed := 1.25
	expireAt := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	updated, err := UserUpdate(model.UserUpdateRequest{
		ID:                 user.ID,
		Username:           user.Username,
		Role:               user.Role,
		Status:             user.Status,
		Balance:            user.Balance,
		DailyLimit:         &dailyLimit,
		DailyUsed:          &dailyUsed,
		MonthlyExpireAtISO: expireAt,
	}, user.ID, ctx)
	if err != nil {
		t.Fatalf("update with human aliases: %v", err)
	}
	if updated.MonthlyLimit != dailyLimit || updated.MonthlyUsed != dailyUsed {
		t.Fatalf("expected daily aliases to populate monthly fields, got limit=%v used=%v", updated.MonthlyLimit, updated.MonthlyUsed)
	}
	now := time.Now()
	if updated.MonthlyResetAt <= now.Unix() || updated.MonthlyResetAt > now.Add(24*time.Hour).Unix() {
		t.Fatalf("expected reset within next daily window, got %d", updated.MonthlyResetAt)
	}
}

func TestRedeemMonthlyCodeUsesDailyQuotaAliasAndDailyReset(t *testing.T) {
	ctx := setupUserTest(t)
	user, err := UserCreate(model.UserCreateRequest{
		Username: "monthly-redeem",
		Password: "secret",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	dailyQuota := 12.5
	validDays := 3
	codes, err := RedeemCodeGenerate(model.RedeemCodeGenerateRequest{
		Type:       model.RedeemCodeTypeMonthly,
		Count:      1,
		DailyQuota: &dailyQuota,
		ValidDays:  &validDays,
	}, 1, ctx)
	if err != nil {
		t.Fatalf("generate monthly code: %v", err)
	}
	if codes[0].MonthlyLimit != dailyQuota || codes[0].MonthlyDays != validDays {
		t.Fatalf("expected daily quota alias to fill stored compatibility fields, got %#v", codes[0])
	}

	responseBody, err := json.Marshal(model.NewRedeemCodeResponse(codes[0]))
	if err != nil {
		t.Fatalf("marshal redeem response: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(responseBody, &response); err != nil {
		t.Fatalf("unmarshal redeem response: %v", err)
	}
	if response["daily_quota"] != dailyQuota || response["valid_days"] != float64(validDays) {
		t.Fatalf("expected human redeem response aliases, got %s", string(responseBody))
	}

	start := time.Now()
	redeemed, err := RedeemCodeRedeem(codes[0].Code, user.ID, ctx)
	if err != nil {
		t.Fatalf("redeem monthly code: %v", err)
	}
	now := time.Now()
	if redeemed.MonthlyLimit != dailyQuota || redeemed.MonthlyUsed != 0 {
		t.Fatalf("expected redeemed user daily quota, got limit=%v used=%v", redeemed.MonthlyLimit, redeemed.MonthlyUsed)
	}
	if redeemed.MonthlyExpireAt < start.Add(time.Duration(validDays)*24*time.Hour-2*time.Second).Unix() ||
		redeemed.MonthlyExpireAt > now.Add(time.Duration(validDays)*24*time.Hour+2*time.Second).Unix() {
		t.Fatalf("expected expire in about %d days, got %d", validDays, redeemed.MonthlyExpireAt)
	}
	if redeemed.MonthlyResetAt <= start.Unix() || redeemed.MonthlyResetAt > start.Add(24*time.Hour).Unix() {
		t.Fatalf("expected reset within the next daily window, got %d", redeemed.MonthlyResetAt)
	}

	summary := redeemed.MonthlyQuotaSummary(now)
	if !summary.MonthlyActive || summary.DailyRemaining != dailyQuota {
		t.Fatalf("expected active daily quota summary after redeem, got %#v", summary)
	}
}
