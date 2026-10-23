package op

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func setupQuotaTest(t *testing.T) context.Context {
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
	if err := UserInit(); err != nil {
		t.Fatalf("init user cache: %v", err)
	}
	// 在途预留是进程级内存态，跨用例必须清空。
	t.Cleanup(func() {
		for _, u := range userCache.GetAll() {
			UserReleaseQuota(u.ID, userQuotaInflightOf(u.ID))
		}
	})
	return ctx
}

func newQuotaTestUser(t *testing.T, ctx context.Context, balance float64) model.User {
	t.Helper()
	user, err := UserCreate(model.UserCreateRequest{
		Username: "quota-user-" + t.Name(),
		Password: "test-password-123",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
		Balance:  balance,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// 这条测试盯的是钱：余额只剩 0.001 时并发放行 N 个请求，曾经 N 个全部通过。
//
// 旧行为：UserCanRelay 只判断 `Balance > 0`，扣费在请求结束才发生，两次读改写之间
// 没有耦合 → 并发 100 个请求全部读到「余额 0.001 > 0」而放行，实际消耗可达 100 倍。
//
// 新行为：准入即在锁内扣一笔在途预留（min(默认预留, 剩余余额)），available 不足则拒绝。
// 余额 0.001 只能支撑**一个**预留，所以放行数必须为 1。
func TestUserCanRelayReservesQuotaAgainstConcurrentBurst(t *testing.T) {
	ctx := setupQuotaTest(t)
	user := newQuotaTestUser(t, ctx, 0.001)

	const burst = 100
	var granted int64
	var wg sync.WaitGroup
	reservations := make([]float64, burst)

	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			res, err := UserCanRelay(user.ID, ctx)
			if err == nil {
				atomic.AddInt64(&granted, 1)
				reservations[idx] = res
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&granted); got != 1 {
		t.Fatalf("余额 0.001 时放行了 %d 个并发请求，期望 1：并发超支没有收敛", got)
	}

	// 归还后应能再放行一个（余额没变，只是预留释放了）。
	for i := 0; i < burst; i++ {
		UserReleaseQuota(user.ID, reservations[i])
	}
	if _, err := UserCanRelay(user.ID, ctx); err != nil {
		t.Fatalf("归还预留后仍被拒绝：%v（预留泄漏，用户会被永久卡死）", err)
	}
}

// 富余用户不能被串行化：余额 10、预留 0.01 → 1000 个并发全部放行。
// 这条是防"修了漏洞、误伤了正常用户"。
func TestUserCanRelayDoesNotSerializeHealthyUser(t *testing.T) {
	ctx := setupQuotaTest(t)
	user := newQuotaTestUser(t, ctx, 10)

	const burst = 1000
	var granted int64
	var wg sync.WaitGroup
	reservations := make([]float64, burst)

	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			res, err := UserCanRelay(user.ID, ctx)
			if err == nil {
				atomic.AddInt64(&granted, 1)
				reservations[idx] = res
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&granted); got != burst {
		t.Fatalf("余额 10 的健康用户只放行 %d/%d：预留额度太大，误伤了正常并发", got, burst)
	}
	for i := 0; i < burst; i++ {
		UserReleaseQuota(user.ID, reservations[i])
	}
	if left := userQuotaInflightOf(user.ID); left != 0 {
		t.Fatalf("全部归还后仍残留 %v 在途预留", left)
	}
}

// 管理员不受额度限制，也不该占用预留。
func TestUserCanRelayAdminTakesNoReservation(t *testing.T) {
	ctx := setupQuotaTest(t)
	admin, err := UserCreate(model.UserCreateRequest{
		Username: "quota-admin",
		Password: "test-password-123",
		Role:     model.UserRoleAdmin,
		Status:   model.UserStatusActive,
		Balance:  0,
	}, ctx)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	res, err := UserCanRelay(admin.ID, ctx)
	if err != nil {
		t.Fatalf("余额 0 的管理员被拒绝：%v", err)
	}
	if res != 0 {
		t.Fatalf("管理员占用了 %v 预留，期望 0", res)
	}
}

// 归还幂等且不会把在途额算成负数（那会凭空放大可用额度）。
func TestUserReleaseQuotaNeverGoesNegative(t *testing.T) {
	ctx := setupQuotaTest(t)
	user := newQuotaTestUser(t, ctx, 1)

	res, err := UserCanRelay(user.ID, ctx)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	UserReleaseQuota(user.ID, res)
	UserReleaseQuota(user.ID, res) // 重复归还
	UserReleaseQuota(user.ID, 999) // 超额归还

	if left := userQuotaInflightOf(user.ID); left != 0 {
		t.Fatalf("在途预留残留 %v，期望 0", left)
	}
}
