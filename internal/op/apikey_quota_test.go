package op

import (
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 并发准入：上限只剩 2 个量子时，100 个并发请求最多放进 2 个，其余在落账前就被挡住。
func TestAPIKeyReserveCapsConcurrentRequests(t *testing.T) {
	key := model.APIKey{ID: 91001, MaxCost: 1}
	t.Cleanup(func() { statsAPIKeyCache.Del(key.ID) })
	if err := StatsAPIKeyUpdate(key.ID, model.StatsMetrics{InputCost: 1 - 2*userQuotaReserveDefault}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted []float64
	)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := APIKeyReserve(key, 0); err == nil {
				mu.Lock()
				granted = append(granted, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(granted) != 2 {
		t.Fatalf("granted %d concurrent requests, want 2", len(granted))
	}
	if _, err := APIKeyReserve(key, 0); err != ErrAPIKeyMaxCost {
		t.Fatalf("cap with in-flight full must reject, got %v", err)
	}
	for _, r := range granted {
		APIKeyReleaseReserve(key.ID, r)
	}
	if got := APIKeyInflight(key.ID); got != 0 {
		t.Fatalf("inflight after release = %v, want 0", got)
	}
	r, err := APIKeyReserve(key, 0)
	if err != nil {
		t.Fatalf("after release a request must pass again: %v", err)
	}
	APIKeyReleaseReserve(key.ID, r)
}

// 已经花到上限（等于也算）就拒绝；没设上限的 Key 不占不拦。
func TestAPIKeyReserveAtCapAndUncapped(t *testing.T) {
	key := model.APIKey{ID: 91002, MaxCost: 1}
	t.Cleanup(func() { statsAPIKeyCache.Del(key.ID) })
	if err := StatsAPIKeyUpdate(key.ID, model.StatsMetrics{InputCost: 0.4, OutputCost: 0.6}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	if _, err := APIKeyReserve(key, 0); err != ErrAPIKeyMaxCost {
		t.Fatalf("spent == cap must reject, got %v", err)
	}
	if r, err := APIKeyReserve(model.APIKey{ID: key.ID}, 0); err != nil || r != 0 {
		t.Fatalf("uncapped key: r=%v err=%v", r, err)
	}
}

// 异步出图：每条任务按预扣金额占住 Key 上限，直到终态归还。上限 1、每条 0.4：只能同时提交 2 条。
func TestImageTaskKeyReservationHoldsUntilTerminal(t *testing.T) {
	key := model.APIKey{ID: 91003, MaxCost: 1, Guest: true}
	t.Cleanup(func() { statsAPIKeyCache.Del(key.ID) })

	if _, err := ImageTaskKeyReservationTake("imgtask_key_a", key, 0.4); err != nil {
		t.Fatalf("task a: %v", err)
	}
	if _, err := ImageTaskKeyReservationTake("imgtask_key_b", key, 0.4); err != nil {
		t.Fatalf("task b: %v", err)
	}
	if _, err := ImageTaskKeyReservationTake("imgtask_key_c", key, 0.4); err == nil {
		t.Fatal("third task would exceed the cap and must be refused")
	}
	if !ImageTaskReservationRelease("imgtask_key_a") {
		t.Fatal("releasing task a should return its key reservation")
	}
	if ImageTaskReservationRelease("imgtask_key_a") {
		t.Fatal("second release must be a no-op")
	}
	if _, err := ImageTaskKeyReservationTake("imgtask_key_c", key, 0.4); err != nil {
		t.Fatalf("after a finished, c fits: %v", err)
	}
	ImageTaskReservationRelease("imgtask_key_b")
	ImageTaskReservationRelease("imgtask_key_c")
	if got := APIKeyInflight(key.ID); got > userQuotaInflightEpsilon {
		t.Fatalf("inflight after all terminal = %v, want 0", got)
	}
}
