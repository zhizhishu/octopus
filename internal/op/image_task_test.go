package op

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func newImageTaskTestTask(t *testing.T, ctx context.Context, taskID string, ownerTokenID, ownerUserID int, cost float64) model.ImageTask {
	t.Helper()
	now := time.Now()
	task := model.ImageTask{
		TaskID:       taskID,
		CreatedAt:    now.Unix(),
		ExpiresAt:    now.Add(time.Hour).Unix(),
		Status:       model.ImageTaskStatusProcessing,
		OwnerTokenID: ownerTokenID,
		OwnerUserID:  ownerUserID,
		ChannelID:    7,
		ChannelKeyID: 3,
		Model:        "image-test-model",
		Endpoint:     model.ImageTaskEndpointGenerations,
		RequestBody:  `{"model":"image-test-model","prompt":"a quiet lake"}`,
		Cost:         cost,
	}
	if err := ImageTaskCreate(ctx, &task); err != nil {
		t.Fatalf("create image task %s: %v", taskID, err)
	}
	return task
}

// 契约第四节第 2 条（归属隔离，op 层）：查询必须按 owner_token_id + owner_user_id
// 过滤，不是只按 TaskID。
//
// 这条是整份契约里最容易被写成"假绿"的一条：只测"同用户能查到"的话，一个
// `WHERE task_id = ?` 的实现也会全绿。所以这里的方向是**查不到**：
// 同一个 TaskID 换一把 key / 换一个用户查，必须返回 nil。
func TestImageTaskGetForOwnerIsolatesByOwner(t *testing.T) {
	ctx := setupQuotaTest(t)
	task := newImageTaskTestTask(t, ctx, "imgtask_owner_isolation", 5, 7, 0)

	// 正向对照：自己的任务读得到（否则下面的"读不到"可能只是查询写错了）。
	got, err := ImageTaskGetForOwner(ctx, task.TaskID, 5, 7)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if got == nil || got.TaskID != task.TaskID {
		t.Fatalf("owner read returned %+v, want the task", got)
	}

	// 同一个 TaskID、不同的 key（同一用户）：读不到。
	got, err = ImageTaskGetForOwner(ctx, task.TaskID, 6, 7)
	if err != nil {
		t.Fatalf("other token read returned a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("another API key of the same user read the task; owner_token_id filter is missing")
	}

	// 同一个 TaskID、不同的用户：读不到。
	got, err = ImageTaskGetForOwner(ctx, task.TaskID, 5, 8)
	if err != nil {
		t.Fatalf("other user read returned a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("another user read the task; owner_user_id filter is missing")
	}

	// 完全没给归属：必须报错（fail-closed），不许退化成"不限归属"。
	if _, err := ImageTaskGetForOwner(ctx, task.TaskID, 0, 0); !errors.Is(err, ErrImageTaskOwnerScopeRequired) {
		t.Fatalf("ownerless query error = %v, want ErrImageTaskOwnerScopeRequired", err)
	}
}

// 契约第四节第 1 条（写入侧）：状态 CAS 只接受三个合法目标值，其余一律报错。
func TestImageTaskStatusCASRejectsUnknownStatus(t *testing.T) {
	ctx := setupQuotaTest(t)
	task := newImageTaskTestTask(t, ctx, "imgtask_cas_guard", 5, 7, 0)

	for _, bad := range []string{"pending", "queued", "done", "", "PROCESSING"} {
		if _, err := ImageTaskStatusCAS(ctx, task.ID, model.ImageTaskStatusProcessing, bad, nil); err == nil {
			t.Fatalf("CAS to %q must be rejected", bad)
		}
	}

	// 合法目标值照常生效。
	ok, err := ImageTaskStatusCAS(ctx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusFailed, map[string]any{
		"error_message": "upstream said no",
	})
	if err != nil || !ok {
		t.Fatalf("CAS processing->failed = %v, %v (want true, nil)", ok, err)
	}
	// 已经不是 processing 了：同一个 CAS 必须落空（这就是"不许互相覆盖"）。
	ok, err = ImageTaskStatusCAS(ctx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusCompleted, nil)
	if err != nil {
		t.Fatalf("second CAS: %v", err)
	}
	if ok {
		t.Fatalf("CAS advanced an already-terminal task; the from-state precondition is missing")
	}
}

// 契约第三节第 4 条：退款路径必须幂等——重复退不能退两次。
func TestImageTaskRefundIsIdempotent(t *testing.T) {
	ctx := setupQuotaTest(t)
	user := newQuotaTestUser(t, ctx, 1.0)

	// 模拟提交时的预扣。
	if err := UserRecordUsage(user.ID, 0.5, ctx); err != nil {
		t.Fatalf("pre-charge: %v", err)
	}
	if after := mustUser(t, user.ID); after.Balance != 0.5 {
		t.Fatalf("balance after pre-charge = %v, want 0.5", after.Balance)
	}

	task := newImageTaskTestTask(t, ctx, "imgtask_refund_idempotent", 5, user.ID, 0.5)

	refunded, err := ImageTaskRefund(ctx, task)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if !refunded {
		t.Fatalf("first refund reported false (the claim must succeed once)")
	}
	if after := mustUser(t, user.ID); after.Balance != 1.0 {
		t.Fatalf("balance after refund = %v, want 1.0", after.Balance)
	}

	// 第二次：抢不到退款权，钱不能再动。
	refunded, err = ImageTaskRefund(ctx, task)
	if err != nil {
		t.Fatalf("second refund: %v", err)
	}
	if refunded {
		t.Fatalf("second refund refunded again; the cost claim is not idempotent")
	}
	if after := mustUser(t, user.ID); after.Balance != 1.0 {
		t.Fatalf("balance after the duplicate refund = %v, want 1.0 (must not pay twice)", after.Balance)
	}

	// Cost 列已被清零：这是"这一单已经结算过"的唯一凭据。
	var stored model.ImageTask
	if err := db.GetDB().WithContext(ctx).Where("id = ?", task.ID).First(&stored).Error; err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if stored.Cost != 0 {
		t.Fatalf("cost after refund = %v, want 0", stored.Cost)
	}
}

// 契约第三节第 5 条（账本层）：在途预留的归还必须幂等，重复归还不得放大可用额度。
func TestImageTaskReservationReleaseIsIdempotent(t *testing.T) {
	ctx := setupQuotaTest(t)
	// 余额恰好等于一个预留量子：第一笔预留就会把可用额度占满。
	user := newQuotaTestUser(t, ctx, userQuotaReserveDefault)

	taskID := "imgtask_reservation_idempotent"
	reservation, err := ImageTaskReservationTake(ctx, taskID, user.ID)
	if err != nil {
		t.Fatalf("take reservation: %v", err)
	}
	if reservation <= 0 {
		t.Fatalf("reservation = %v, want > 0", reservation)
	}
	if _, err := UserCanRelay(user.ID, ctx); err == nil {
		t.Fatalf("the task reservation did not hold any quota: a second borrow was allowed")
	}

	if !ImageTaskReservationRelease(taskID) {
		t.Fatalf("first release returned false")
	}
	if ImageTaskReservationRelease(taskID) {
		t.Fatalf("second release returned true; the ledger is not idempotent")
	}

	mu := lockUser(user.ID)
	mu.Lock()
	inflight := userQuotaInflightOf(user.ID)
	mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight after release = %v, want 0", inflight)
	}

	// 释放之后这笔余额又能借出去（真实语义："可以再发一次请求"）。
	second, err := UserCanRelay(user.ID, ctx)
	if err != nil {
		t.Fatalf("borrow after release: %v", err)
	}
	UserReleaseQuota(user.ID, second)
}

// 两条查询必须互补：processing 的行要么"还没过期、该去执行"，要么"已过期、该被收口"，
// 不能有任何一行两边都不在（那样它会永远停在 processing 且永远不退钱）。
func TestImageTaskListsCoverEveryProcessingRow(t *testing.T) {
	ctx := setupQuotaTest(t)
	now := time.Now()

	live := model.ImageTask{
		TaskID:    "imgtask_live",
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
		Status:    model.ImageTaskStatusProcessing,
	}
	stale := model.ImageTask{
		TaskID:    "imgtask_stale",
		CreatedAt: now.Add(-2 * time.Hour).Unix(),
		ExpiresAt: now.Add(-time.Hour).Unix(),
		Status:    model.ImageTaskStatusProcessing,
	}
	for _, task := range []*model.ImageTask{&live, &stale} {
		if err := ImageTaskCreate(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.TaskID, err)
		}
	}

	executable, err := ImageTaskListExecutable(ctx, now.Unix(), 10)
	if err != nil {
		t.Fatalf("list executable: %v", err)
	}
	expired, err := ImageTaskListExpired(ctx, now.Unix(), 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}

	seen := map[string]string{}
	for _, task := range executable {
		seen[task.TaskID] = "executable"
	}
	for _, task := range expired {
		if prev, ok := seen[task.TaskID]; ok {
			t.Fatalf("task %s appeared in both lists (%s and expired)", task.TaskID, prev)
		}
		seen[task.TaskID] = "expired"
	}
	if seen["imgtask_live"] != "executable" {
		t.Fatalf("live task classified as %q, want executable", seen["imgtask_live"])
	}
	if seen["imgtask_stale"] != "expired" {
		t.Fatalf("stale task classified as %q, want expired", seen["imgtask_stale"])
	}
}

func mustUser(t *testing.T, userID int) model.User {
	t.Helper()
	user, err := UserGet(userID)
	if err != nil {
		t.Fatalf("get user %d: %v", userID, err)
	}
	return user
}
