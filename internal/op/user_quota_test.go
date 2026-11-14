package op

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func quotaTestUser(t *testing.T, ctx context.Context) model.User {
	t.Helper()
	user, err := UserCreate(model.UserCreateRequest{
		Username: "quota-user", Password: "test-password", Role: model.UserRoleUser,
		Status: model.UserStatusActive, Balance: 100,
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func quotaTestPolicy(t *testing.T, ctx context.Context, userID int, cost float64, count int64) *model.UserQuotaPolicy {
	t.Helper()
	p, err := UserQuotaPolicySave(ctx, model.UserQuotaPolicy{
		UserID: userID, Enabled: true, Timezone: "America/Los_Angeles",
		FiveHourCost: cost, FiveHourRequests: count,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func quotaClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	old := userQuotaNow
	clock := at
	userQuotaNow = func() time.Time { return clock }
	t.Cleanup(func() { userQuotaNow = old })
	return &clock
}

func TestUserQuotaConcurrentCountAndFailedAdmission(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	quotaTestPolicy(t, ctx, user.ID, 0, 5)
	var accepted atomic.Int64
	var rejected atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{
				SourceKey: fmt.Sprintf("relay:test-%d", i), SourceKind: "relay",
				UserID: user.ID, RequestModel: "m", Units: 1,
			}, nil)
			if errors.Is(err, ErrUserQuotaExceeded) {
				rejected.Add(1)
				return
			}
			if err != nil {
				errs <- err
				return
			}
			if receipt == nil {
				errs <- errors.New("missing receipt")
				return
			}
			accepted.Add(1)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if accepted.Load() != 5 || rejected.Load() != 59 {
		t.Fatalf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	state, err := UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range state.Windows {
		if w.Accepted != 5 {
			t.Fatalf("%s accepted=%d", w.Period, w.Accepted)
		}
	}
}

func TestUserQuotaWindowsAcrossDSTAndMonth(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	p := model.UserQuotaPolicy{UserID: 1, Timezone: loc.String(), AnchorAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix()}
	cases := []struct {
		at        time.Time
		weekHours int64
		monthDays int
	}{
		{time.Date(2026, 3, 8, 12, 0, 0, 0, loc), 167, 31},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, loc), 169, 30},
		{time.Date(2028, 2, 29, 12, 0, 0, 0, loc), 168, 29},
	}
	for _, tc := range cases {
		windows, err := userQuotaWindows(p, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if windows[0].EndAt-windows[0].StartAt != 18000 {
			t.Fatal("five-hour duration changed with DST")
		}
		if (windows[0].StartAt-p.AnchorAt)%18000 != 0 {
			t.Fatal("anchor moved")
		}
		week := windows[1]
		if (week.EndAt-week.StartAt)/3600 != tc.weekHours {
			t.Fatalf("week=%+v", week)
		}
		start := time.Unix(week.StartAt, 0).In(loc)
		if start.Weekday() != time.Monday || start.Hour() != 0 {
			t.Fatalf("week boundary=%v", start)
		}
		monthStart := time.Unix(windows[2].StartAt, 0).In(loc)
		monthEnd := time.Unix(windows[2].EndAt, 0).In(loc)
		if monthStart.Day() != 1 || monthEnd.Day() != 1 || monthEnd.AddDate(0, 0, -1).Day() != tc.monthDays {
			t.Fatalf("month boundary=%v %v", monthStart, monthEnd)
		}
	}
	exact := time.Unix(p.AnchorAt+18000, 0)
	windows, _ := userQuotaWindows(p, exact)
	if windows[0].StartAt != exact.Unix() {
		t.Fatal("exact boundary did not enter next window")
	}
	windows, _ = userQuotaWindows(p, time.Unix(p.AnchorAt-1, 0))
	if windows[0].StartAt != p.AnchorAt-18000 {
		t.Fatal("negative offset floor is incorrect")
	}
}

func TestUserQuotaDisableEnableDoesNotReset(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	p := quotaTestPolicy(t, ctx, user.ID, 10, 1)
	receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{SourceKey: "relay:once", SourceKind: "relay", UserID: user.ID, RequestModel: "m", Units: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UserQuotaSettle(ctx, receipt.SourceKey, user.ID, 2, 1, "failed", nil); err != nil {
		t.Fatal(err)
	}
	anchor := p.AnchorAt
	p.Enabled = false
	p, err = UserQuotaPolicySave(ctx, *p)
	if err != nil {
		t.Fatal(err)
	}
	p.Enabled = true
	p, err = UserQuotaPolicySave(ctx, *p)
	if err != nil {
		t.Fatal(err)
	}
	if p.AnchorAt != anchor {
		t.Fatal("reenabling moved anchor")
	}
	if _, err := UserQuotaAdmit(ctx, UserQuotaAdmission{SourceKey: "relay:again", SourceKind: "relay", UserID: user.ID, RequestModel: "m", Units: 1}, nil); !errors.Is(err, ErrUserQuotaExceeded) {
		t.Fatalf("reset bypass: %v", err)
	}
	changed := *p
	changed.Timezone = "UTC"
	if _, err := UserQuotaPolicySave(ctx, changed); err == nil {
		t.Fatal("changed timezone after usage")
	}
	changed = *p
	changed.AnchorAt++
	if _, err := UserQuotaPolicySave(ctx, changed); err == nil {
		t.Fatal("changed anchor after usage")
	}
	state, _ := UserQuotaStatusGet(ctx, user.ID)
	for _, w := range state.Windows {
		if w.Accepted != 1 || w.FailedAccepted != 1 || w.CostUsed != 2 {
			t.Fatalf("lost counters: %+v", w)
		}
	}
}

func TestUserQuotaRefundUsesOriginalWindowsAndIsIdempotent(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	clock := quotaClock(t, time.Date(2026, 4, 30, 23, 59, 0, 0, time.UTC))
	quotaTestPolicy(t, ctx, user.ID, 20, 20)
	receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{SourceKey: "image:late", SourceKind: "image", UserID: user.ID, RequestModel: "m", Units: 1, InitialCost: 5, KnownCost: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.AddDate(0, 1, 3)
	for i := 0; i < 3; i++ {
		if handled, err := UserQuotaSettle(ctx, receipt.SourceKey, user.ID, 1, 0, "completed", nil); err != nil || !handled {
			t.Fatalf("settle=%v %v", handled, err)
		}
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 99 {
		t.Fatalf("duplicate refund changed balance: %v", got.Balance)
	}
	for period, start := range userQuotaReceiptStarts(*receipt) {
		var window model.UserQuotaWindow
		if err := db.GetDB().First(&window, "user_id = ? AND period = ? AND start_at = ?", user.ID, period, start).Error; err != nil {
			t.Fatal(err)
		}
		if window.CostUsed != 1 || window.Accepted != 1 || window.CostReserved != 0 {
			t.Fatalf("original window=%+v", window)
		}
	}
	state, err := UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range state.Windows {
		if window.CostUsed != 0 || window.Accepted != 0 {
			t.Fatalf("refund touched current window: %+v", window)
		}
	}
}

func TestUserQuotaTransactionRollbackDoesNotSplitMoney(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	quotaTestPolicy(t, ctx, user.ID, 20, 20)
	injected := errors.New("injected persistence failure")
	input := UserQuotaAdmission{SourceKey: "image:atomic", SourceKind: "image", UserID: user.ID, RequestModel: "m", Units: 1, InitialCost: 5, KnownCost: true}
	if _, err := UserQuotaAdmit(ctx, input, func(*gorm.DB) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("admit err=%v", err)
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 100 {
		t.Fatalf("rollback cache balance=%v", got.Balance)
	}
	var stored model.User
	db.GetDB().First(&stored, user.ID)
	if stored.Balance != 100 {
		t.Fatalf("rollback DB balance=%v", stored.Balance)
	}
	exists, err := UserQuotaReceiptExists(ctx, input.SourceKey, user.ID)
	if err != nil || exists {
		t.Fatalf("rolled back receipt exists=%v err=%v", exists, err)
	}
	if _, err := UserQuotaAdmit(ctx, input, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := UserQuotaSettle(ctx, input.SourceKey, user.ID, 0, 1, "failed", func(*gorm.DB) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("settle err=%v", err)
	}
	got, _ = UserGet(user.ID)
	if got.Balance != 95 {
		t.Fatalf("failed settlement changed cache=%v", got.Balance)
	}
	if _, err := UserQuotaSettle(ctx, input.SourceKey, user.ID, 0, 1, "failed", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = UserGet(user.ID)
	if got.Balance != 100 {
		t.Fatalf("refund did not apply=%v", got.Balance)
	}
	state, _ := UserQuotaStatusGet(ctx, user.ID)
	for _, w := range state.Windows {
		if w.CostUsed != 0 || w.Accepted != 1 || w.FailedAccepted != 1 {
			t.Fatalf("failed accepted accounting=%+v", w)
		}
	}
}

func TestUserQuotaUnknownCostOverrunAndModelConstraint(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	p := quotaTestPolicy(t, ctx, user.ID, 1, 10)
	p.AllowedModels = "allowed"
	if _, err := UserQuotaPolicySave(ctx, *p); err != nil {
		t.Fatal(err)
	}
	input := UserQuotaAdmission{SourceKey: "relay:unknown", SourceKind: "relay", UserID: user.ID, RequestModel: "denied", Units: 1}
	if _, err := UserQuotaAdmit(ctx, input, nil); !errors.Is(err, ErrUserQuotaModel) {
		t.Fatalf("model restriction=%v", err)
	}
	input.RequestModel = "allowed"
	receipt, err := UserQuotaAdmit(ctx, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Reserved <= 0 {
		t.Fatal("unknown request has no reservation")
	}
	if _, err := UserQuotaSettle(ctx, receipt.SourceKey, user.ID, 3, 0, "completed", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := UserGet(user.ID)
	if math.Abs(got.Balance-97) > 1e-9 {
		t.Fatalf("actual cost was clamped: %v", got.Balance)
	}
	input.SourceKey = "relay:next"
	if _, err := UserQuotaAdmit(ctx, input, nil); !errors.Is(err, ErrUserQuotaExceeded) {
		t.Fatalf("overrun did not close next admission: %v", err)
	}
}

func TestUserQuotaUnconfiguredPathAndUserShape(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	beforeUser, _ := UserGet(user.ID)
	before, _ := json.Marshal(model.NewUserResponse(beforeUser))
	called := false
	receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{SourceKey: "relay:legacy", SourceKind: "relay", UserID: user.ID, RequestModel: "any", Units: 1}, func(*gorm.DB) error { called = true; return nil })
	if err != nil || receipt != nil || called {
		t.Fatalf("legacy changed: %v %v %v", receipt, err, called)
	}
	quotaTestPolicy(t, ctx, user.ID, 0, 10)
	afterUser, _ := UserGet(user.ID)
	after, _ := json.Marshal(model.NewUserResponse(afterUser))
	if string(before) != string(after) {
		t.Fatalf("existing user JSON changed\nbefore=%s\nafter=%s", before, after)
	}
}

func TestUserQuotaImageTaskTerminalAndRefundAreAtomic(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	quotaTestPolicy(t, ctx, user.ID, 20, 20)
	task := model.ImageTask{TaskID: "imgtask_quota", OwnerUserID: user.ID, RequestModel: "m", Cost: 4, BillingMode: "per_image", ExpiresAt: time.Now().Add(time.Hour).Unix(), Status: model.ImageTaskStatusProcessing}
	if handled, err := UserQuotaImageTaskCreate(ctx, &task); err != nil || !handled {
		t.Fatalf("create: %v %v", handled, err)
	}
	for i := 0; i < 2; i++ {
		handled, changed, err := UserQuotaImageTaskFinish(ctx, task, model.ImageTaskStatusFailed, map[string]any{"completed_at": time.Now().Unix(), "error_message": "test failure"}, 0)
		if err != nil || !handled || changed != (i == 0) {
			t.Fatalf("finish %d: %v %v %v", i, handled, changed, err)
		}
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 100 {
		t.Fatalf("refund repeated: %v", got.Balance)
	}
	var final model.ImageTask
	db.GetDB().First(&final, task.ID)
	if final.Status != model.ImageTaskStatusFailed || final.Cost != 0 {
		t.Fatalf("terminal row=%+v", final)
	}
}

func TestUserQuotaBatchCreatesAllItemsAndCountsEachInput(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	quotaTestPolicy(t, ctx, user.ID, 20, 2)
	job := model.BatchImageJob{BatchID: "imgbatch_quota", Status: model.BatchImageJobStatusQueued, Provider: model.BatchImageProviderGeminiAPI, OwnerUserID: user.ID, RequestModel: "m", ItemCount: 2, HoldAmount: 4}
	items := []model.BatchImageItem{
		{BatchID: job.BatchID, CustomID: "a", Status: model.BatchImageItemStatusSuccess},
		{BatchID: job.BatchID, CustomID: "b", Status: model.BatchImageItemStatusFailed},
	}
	if handled, err := UserQuotaBatchCreate(ctx, &job, items); err != nil || !handled {
		t.Fatalf("create: %v %v", handled, err)
	}
	job.Status = model.BatchImageJobStatusCompleted
	if err := db.GetDB().Model(&job).Update("status", job.Status).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		handled, changed, err := UserQuotaBatchSettle(ctx, &job, 2, 1, 1, 0)
		if err != nil || !handled || changed != (i == 0) {
			t.Fatalf("settle %d: %v %v %v", i, handled, changed, err)
		}
	}
	state, _ := UserQuotaStatusGet(ctx, user.ID)
	for _, w := range state.Windows {
		if w.Accepted != 2 || w.FailedAccepted != 1 || w.CostUsed != 2 {
			t.Fatalf("batch window=%+v", w)
		}
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 98 {
		t.Fatalf("batch charge=%v", got.Balance)
	}
}

func TestUserQuotaConcurrentKnownHoldsCannotReuseBalance(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	quotaTestPolicy(t, ctx, user.ID, 0, 0)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{
				SourceKey: fmt.Sprintf("batch:hold-%d", i), SourceKind: "batch", UserID: user.ID,
				RequestModel: "m", Units: 1, InitialCost: 80, KnownCost: true, RequireBalance: true,
			}, nil)
			if errors.Is(err, ErrUserQuotaExceeded) {
				return
			}
			if err != nil {
				errs <- err
				return
			}
			if receipt != nil {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("accepted holds=%d", accepted.Load())
	}
	got, _ := UserGet(user.ID)
	if got.Balance != 20 {
		t.Fatalf("reused balance=%v", got.Balance)
	}
	state, err := UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range state.Windows {
		if w.Accepted != 1 || w.CostUsed != 80 {
			t.Fatalf("failed admission affected counters=%+v", w)
		}
	}
}

func TestUserQuotaFirstEnableOwnsAnchorAndAdminBypass(t *testing.T) {
	ctx := setupUserTest(t)
	user := quotaTestUser(t, ctx)
	clock := quotaClock(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	p, err := UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: false, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	p.Enabled = true
	p.AnchorAt = 123
	*clock = clock.Add(time.Hour)
	p, err = UserQuotaPolicySave(ctx, *p)
	if err != nil {
		t.Fatal(err)
	}
	if p.AnchorAt != clock.Unix() {
		t.Fatalf("client overrode first-enable anchor: %d", p.AnchorAt)
	}
	role := model.UserRoleAdmin
	if _, err := UserPatch(model.UserPatchRequest{ID: user.ID, Role: &role}, 0, ctx); err != nil {
		t.Fatal(err)
	}
	enabled, err := UserQuotaEnabled(ctx, user.ID)
	if err != nil || enabled {
		t.Fatalf("promoted admin is unexpectedly restricted: %v %v", enabled, err)
	}
}
