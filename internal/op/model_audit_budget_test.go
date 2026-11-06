package op

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 抽检预算的运行时证据：跨天归零、超额拒绝且不扣、并发不超发、上限读不到时按 200。
//
// 这几条都是"钱"的性质，不是文案：一个预算模块只要在其中一条上是错的，抽检就会
// 在无人察觉的情况下多打上游。

// setupModelAuditBudgetTest 起一个临时 SQLite 库并刷新设置缓存。
//
// 预算账本就存在 settings 表里（model_audit_budget_usage），所以要真实的设置读写路径。
func setupModelAuditBudgetTest(t *testing.T) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := settingRefreshCache(context.Background()); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
}

func setModelAuditBudgetLimit(t *testing.T, limit int) {
	t.Helper()
	if err := SettingSetInt(model.SettingKeyModelAuditDailyRequestLimit, limit); err != nil {
		t.Fatalf("set %s=%d: %v", model.SettingKeyModelAuditDailyRequestLimit, limit, err)
	}
}

// TestModelAuditBudgetResetsOnNewDay 账本按本地自然日归零：跨天不需要清理任务，
// 日期对不上就当今天还没用。
func TestModelAuditBudgetResetsOnNewDay(t *testing.T) {
	setupModelAuditBudgetTest(t)
	day1 := time.Date(2026, 3, 4, 9, 30, 0, 0, time.Local)

	ok, used, limit := ModelAuditBudgetReserve(5, day1)
	if !ok {
		t.Fatalf("first reservation of the day must succeed")
	}
	if used != 5 || limit != modelAuditBudgetDefaultLimit {
		t.Fatalf("after reserving 5: used=%d limit=%d, want 5/%d", used, limit, modelAuditBudgetDefaultLimit)
	}

	// 同一天的晚些时候：接着累计，不清零。
	ok, used, _ = ModelAuditBudgetReserve(1, day1.Add(2*time.Hour))
	if !ok || used != 6 {
		t.Fatalf("same-day reservation should accumulate to 6, got ok=%v used=%d", ok, used)
	}
	if got, _ := ModelAuditBudgetStatus(day1.Add(3 * time.Hour)); got != 6 {
		t.Fatalf("status on the same day = %d, want 6", got)
	}

	// 第二天：读到 0，并且新的用量从 0 开始记。
	next := day1.AddDate(0, 0, 1)
	if got, _ := ModelAuditBudgetStatus(next); got != 0 {
		t.Fatalf("status on the next day = %d, want 0 (cross-day reset)", got)
	}
	ok, used, _ = ModelAuditBudgetReserve(3, next)
	if !ok || used != 3 {
		t.Fatalf("next-day reservation should start from 0, got ok=%v used=%d", ok, used)
	}
	if got, _ := ModelAuditBudgetStatus(next); got != 3 {
		t.Fatalf("status on the next day after reserving 3 = %d, want 3", got)
	}
}

// TestModelAuditBudgetRejectsOverLimitWithoutDeducting 扣不到额度时必须**不扣**：
// 一次被拒的请求不能把账本往前推，否则一次额度不够会连带吃掉之后所有轮次。
func TestModelAuditBudgetRejectsOverLimitWithoutDeducting(t *testing.T) {
	setupModelAuditBudgetTest(t)
	setModelAuditBudgetLimit(t, 10)
	now := time.Date(2026, 3, 4, 9, 0, 0, 0, time.Local)

	ok, used, limit := ModelAuditBudgetReserve(8, now)
	if !ok || used != 8 || limit != 10 {
		t.Fatalf("reserve 8/10: ok=%v used=%d limit=%d", ok, used, limit)
	}

	ok, used, limit = ModelAuditBudgetReserve(5, now)
	if ok {
		t.Fatal("reservation that would exceed the limit must be refused")
	}
	if used != 8 || limit != 10 {
		t.Fatalf("refused reservation must not move the ledger: used=%d limit=%d, want 8/10", used, limit)
	}
	if got, _ := ModelAuditBudgetStatus(now); got != 8 {
		t.Fatalf("ledger after refused reservation = %d, want 8", got)
	}

	// 刚好填满放行。
	if ok, used, _ := ModelAuditBudgetReserve(2, now); !ok || used != 10 {
		t.Fatalf("reservation up to the limit must succeed, got ok=%v used=%d", ok, used)
	}
	// 满了就再也不放行。
	if ok, used, _ := ModelAuditBudgetReserve(1, now); ok || used != 10 {
		t.Fatalf("exhausted budget must refuse, got ok=%v used=%d", ok, used)
	}
}

// TestModelAuditBudgetConcurrentReserveNeverOverspends 多 goroutine 同时扣额度时，
// 放行次数必须**正好**等于上限：读-判断-写必须是一个不可分割的动作。
func TestModelAuditBudgetConcurrentReserveNeverOverspends(t *testing.T) {
	setupModelAuditBudgetTest(t)
	const limit = 20
	setModelAuditBudgetLimit(t, limit)
	now := time.Date(2026, 3, 4, 9, 0, 0, 0, time.Local)

	const attempts = 60
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if ok, _, _ := ModelAuditBudgetReserve(1, now); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != limit {
		t.Fatalf("granted %d reservations with limit %d (concurrent overspend)", granted, limit)
	}
	if u, l := ModelAuditBudgetStatus(now); u != limit || l != limit {
		t.Fatalf("ledger after concurrency: used=%d limit=%d, want %d/%d", u, l, limit, limit)
	}
}

// TestModelAuditBudgetLimitFallsBackTo200 上限读不到、或填了 ≤0 时一律按 200：
// 钱包闸门不允许因为一次读配置抖动而消失。
func TestModelAuditBudgetLimitFallsBackTo200(t *testing.T) {
	setupModelAuditBudgetTest(t)
	now := time.Date(2026, 3, 4, 9, 0, 0, 0, time.Local)

	// 出厂默认值就是 200。
	if _, limit := ModelAuditBudgetStatus(now); limit != modelAuditBudgetDefaultLimit {
		t.Fatalf("seeded limit = %d, want %d", limit, modelAuditBudgetDefaultLimit)
	}

	previous, existed := settingCache.Get(model.SettingKeyModelAuditDailyRequestLimit)
	t.Cleanup(func() {
		if existed {
			settingCache.Set(model.SettingKeyModelAuditDailyRequestLimit, previous)
			return
		}
		settingCache.Del(model.SettingKeyModelAuditDailyRequestLimit)
	})

	for _, raw := range []string{"", "0", "-5", "not-a-number"} {
		if raw == "" {
			// 空串 = 键读不到（SettingGetInt 报错）。
			settingCache.Del(model.SettingKeyModelAuditDailyRequestLimit)
		} else {
			settingCache.Set(model.SettingKeyModelAuditDailyRequestLimit, raw)
		}
		if _, limit := ModelAuditBudgetStatus(now); limit != modelAuditBudgetDefaultLimit {
			t.Fatalf("limit with stored value %q = %d, want %d", raw, limit, modelAuditBudgetDefaultLimit)
		}
	}
}
