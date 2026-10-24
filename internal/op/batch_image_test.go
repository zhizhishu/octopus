package op

import (
	"context"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 批处理的持久层契约。
//
// 这几条盯的都是"写成也能跑、但会静默出错"的地方：归属过滤、结算幂等、状态机。
// 每一条都刻意测**反方向**（查不到 / 抢不到 / 拒改），因为正方向即使实现错了
// 也常常碰巧通过。

// newBatchImageTestJob 落一条测试任务。
func newBatchImageTestJob(t *testing.T, ctx context.Context, status string, ownerTokenID, ownerUserID int) model.BatchImageJob {
	t.Helper()
	now := time.Now()
	job := model.BatchImageJob{
		BatchID:      BatchImageNewID(),
		CreatedAt:    now.Unix(),
		UpdatedAt:    now.Unix(),
		ExpiresAt:    now.Add(time.Hour).Unix(),
		Status:       status,
		Model:        "batch-image-test-model",
		RequestModel: "batch-image-test-model",
		Provider:     model.BatchImageProviderGeminiAPI,
		OwnerTokenID: ownerTokenID,
		OwnerUserID:  ownerUserID,
		ChannelID:    11,
		ChannelKeyID: 3,
		ItemCount:    2,
		RequestJSON:  `{}`,
	}
	if err := BatchImageCreate(ctx, &job); err != nil {
		t.Fatalf("create batch image job: %v", err)
	}
	return job
}

// 归属隔离：查询必须按 owner_user_id + owner_token_id 过滤。
//
// 这条最容易写成"假绿"：只测"同用户能查到"的话，一个 `WHERE batch_id = ?` 的实现
// 也会全绿。所以方向是**查不到**——换一把 key、换一个用户都必须拿到 nil。
func TestBatchImageGetForOwnerIsolatesByOwner(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 5, 7)

	// 正向对照：自己的读得到（否则下面的"读不到"可能只是查询写错了）。
	got, err := BatchImageGetForOwner(ctx, job.BatchID, 7, 5)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if got == nil || got.BatchID != job.BatchID {
		t.Fatalf("owner read returned %+v, want the job", got)
	}

	// 同一用户、换一把 key：读不到。
	got, err = BatchImageGetForOwner(ctx, job.BatchID, 7, 6)
	if err != nil {
		t.Fatalf("other token read returned a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("other token read returned %+v, want nil (a different api key must not see this batch)", got)
	}

	// 换一个用户、同一把 key 号：也读不到。
	got, err = BatchImageGetForOwner(ctx, job.BatchID, 8, 5)
	if err != nil {
		t.Fatalf("other user read returned a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("other user read returned %+v, want nil", got)
	}
}

// 状态机：非法跃迁必须被 CAS 拒掉，而不是"写了但状态乱掉"。
func TestBatchImageStatusCASRejectsIllegalTransition(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 1)

	// submitted -> completed 必须被拒：跳过 indexing 就意味着没人去数结果，
	// success_count 永远是 0，结算会按"零成功"把钱全退给用户，而图其实上游已经出了。
	// （注意 queued -> indexing 是**合法**的：上游可以一步到位给内联结果。）
	advanced, err := BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusSubmitted, model.BatchImageJobStatusCompleted, nil)
	if err == nil {
		t.Fatal("submitted -> completed was accepted, want it rejected by the state machine")
	}
	if advanced {
		t.Fatal("illegal transition reported advanced=true")
	}

	// queued -> indexing 是设计内的合法跃迁，必须先确认它真的通
	// （否则上面那条"被拒"可能只是因为整条链都坏了）。
	// 先不真推进，留给下面那条合法路径；这里只断言状态机认可它。
	if !model.CanTransitionBatchImageJob(model.BatchImageJobStatusQueued, model.BatchImageJobStatusIndexing) {
		t.Fatal("queued -> indexing must be legal (upstream can return inline results)")
	}

	// 合法跃迁：真的动了。
	advanced, err = BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusQueued, model.BatchImageJobStatusSubmitted,
		map[string]any{"provider_job_name": "batches/abc"})
	if err != nil {
		t.Fatalf("queued -> submitted: %v", err)
	}
	if !advanced {
		t.Fatal("queued -> submitted did not advance")
	}

	// 再拿旧状态来一次：CAS 必须拒绝（这正是"三方抢一条任务"的仲裁点）。
	advanced, err = BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusQueued, model.BatchImageJobStatusSubmitted, nil)
	if err != nil {
		t.Fatalf("replayed CAS returned an error: %v", err)
	}
	if advanced {
		t.Fatal("replayed CAS advanced again; the from-status must be the judge")
	}
}

// CAS 的 updates 里带 status 不许覆盖目标状态。
//
// 允许覆盖意味着调用方可以借 CAS 的钱（from 校验）去做另一件事（改成任意状态），
// 状态机就形同虚设。
func TestBatchImageStatusCASStripsStatusInUpdates(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 1)

	advanced, err := BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusQueued, model.BatchImageJobStatusSubmitted,
		map[string]any{
			"status":            model.BatchImageJobStatusCancelled,
			"provider_job_name": "batches/xyz",
		})
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if !advanced {
		t.Fatal("CAS did not advance")
	}

	got, err := BatchImageGetByBatchID(ctx, job.BatchID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.Status != model.BatchImageJobStatusSubmitted {
		t.Fatalf("status = %q, want %q (updates must not smuggle a different status)",
			got.Status, model.BatchImageJobStatusSubmitted)
	}
}

// 结算权：只有第一个抢到的人能结算，后来者拿到 false。
//
// 这一条是整条钱路径的地基。三个写者（推进器、客户端取消、过期清扫）都可能
// 同时到终态；抢不到权却继续退款的实现会**重复退款**。
func TestBatchImageClaimSettleIsSingleWinner(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusCompleted, 1, 1)

	first, err := BatchImageClaimSettle(ctx, job.ID)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("first claim lost; nobody could ever settle this job")
	}

	second, err := BatchImageClaimSettle(ctx, job.ID)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("second claim won; two writers would both refund")
	}
}

// 还权之后必须能重新抢到 —— 否则"数不出条目数就还权重试"这条补偿路径是死的。
func TestBatchImageRestoreSettleClaimAllowsRetry(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusCompleted, 1, 1)

	if claimed, err := BatchImageClaimSettle(ctx, job.ID); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := BatchImageRestoreSettleClaim(ctx, job.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	claimed, err := BatchImageClaimSettle(ctx, job.ID)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !claimed {
		t.Fatal("could not reclaim after restoring the claim; the retry compensation is dead")
	}
}

// 幂等键：同 owner 同 key 查得到；不同 owner 查不到；没写过时返回 (nil, nil)。
func TestBatchImageIdempotencyKeyLookup(t *testing.T) {
	ctx := setupQuotaTest(t)
	// helper 已经落库；幂等键要在**建表之前**给，所以这里单独造一条。
	job := model.BatchImageJob{
		BatchID:        BatchImageNewID(),
		CreatedAt:      time.Now().Unix(),
		UpdatedAt:      time.Now().Unix(),
		ExpiresAt:      time.Now().Add(time.Hour).Unix(),
		Status:         model.BatchImageJobStatusQueued,
		Model:          "batch-image-test-model",
		Provider:       model.BatchImageProviderGeminiAPI,
		OwnerTokenID:   3,
		OwnerUserID:    4,
		ChannelID:      11,
		ItemCount:      1,
		IdempotencyKey: "idem-key-1",
	}
	if err := BatchImageCreate(ctx, &job); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := BatchImageGetByIdempotencyKey(ctx, 4, 3, "idem-key-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil || got.BatchID != job.BatchID {
		t.Fatalf("lookup returned %+v, want the job", got)
	}

	// 换个用户查同一个 key：查不到（幂等键不是全局命名空间）。
	got, err = BatchImageGetByIdempotencyKey(ctx, 5, 3, "idem-key-1")
	if err != nil {
		t.Fatalf("cross-owner lookup returned a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("cross-owner lookup returned %+v, want nil", got)
	}

	// 没写过：必须是 (nil, nil) 而不是 ErrRecordNotFound——
	// 提交路径靠这个区分"新请求"和"数据库坏了"。
	got, err = BatchImageGetByIdempotencyKey(ctx, 4, 3, "never-written")
	if err != nil {
		t.Fatalf("absent key returned an error: %v (want nil, nil)", err)
	}
	if got != nil {
		t.Fatalf("absent key returned %+v", got)
	}
}

// 条目索引是**一次性**的：已经定论的行不许被重跑改写。
//
// 【为什么必须这样】结果流重读时（取图、重试）会再扫到同一行。如果允许改写，
// 一个暂时读不到图的重扫就能把 success 改成 failed，用户的图凭空消失，
// 而钱已经按 success 收了。
func TestBatchImageItemIndexResultIsOneShot(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusIndexing, 1, 1)
	items := []model.BatchImageItem{
		{BatchID: job.BatchID, CustomID: "a", Status: model.BatchImageItemStatusPending},
		{BatchID: job.BatchID, CustomID: "b", Status: model.BatchImageItemStatusPending},
	}
	if err := BatchImageItemCreateBatch(ctx, items); err != nil {
		t.Fatalf("create items: %v", err)
	}
	stored, err := BatchImageItemsByBatchID(ctx, job.BatchID)
	if err != nil {
		t.Fatalf("read items: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d items, want 2", len(stored))
	}

	applied, err := BatchImageItemIndexResult(ctx, stored[0].ID, map[string]any{
		"status":      model.BatchImageItemStatusSuccess,
		"mime_type":   "image/png",
		"image_count": 1,
	})
	if err != nil || !applied {
		t.Fatalf("first index: applied=%v err=%v", applied, err)
	}

	// 第二次（模拟重扫）：必须被拒。
	applied, err = BatchImageItemIndexResult(ctx, stored[0].ID, map[string]any{
		"status":     model.BatchImageItemStatusFailed,
		"error_code": "reshoot",
	})
	if err != nil {
		t.Fatalf("second index returned an error: %v", err)
	}
	if applied {
		t.Fatal("second index was applied; a rescan must not rewrite a finalized row")
	}

	final, err := BatchImageItemGet(ctx, job.BatchID, "a")
	if err != nil {
		t.Fatalf("reread item: %v", err)
	}
	if final.Status != model.BatchImageItemStatusSuccess {
		t.Fatalf("status = %q, want success (the finalized row was rewritten)", final.Status)
	}
}

// 计数：三种状态各算各的，不能漏也不能串。
func TestBatchImageCountItems(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusIndexing, 1, 1)
	items := []model.BatchImageItem{
		{BatchID: job.BatchID, CustomID: "s1", Status: model.BatchImageItemStatusSuccess},
		{BatchID: job.BatchID, CustomID: "s2", Status: model.BatchImageItemStatusSuccess},
		{BatchID: job.BatchID, CustomID: "f1", Status: model.BatchImageItemStatusFailed},
		{BatchID: job.BatchID, CustomID: "c1", Status: model.BatchImageItemStatusCancelled},
		{BatchID: job.BatchID, CustomID: "p1", Status: model.BatchImageItemStatusPending},
	}
	if err := BatchImageItemCreateBatch(ctx, items); err != nil {
		t.Fatalf("create items: %v", err)
	}

	success, failed, cancelled, err := BatchImageCountItems(ctx, job.BatchID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if success != 2 || failed != 1 || cancelled != 1 {
		t.Fatalf("counts = (%d,%d,%d), want (2,1,1)", success, failed, cancelled)
	}
}

// 可执行列表：终态任务不许再被捡起来（否则每一轮都白跑一次上游）。
func TestBatchImageListExecutableSkipsTerminalAndExpired(t *testing.T) {
	ctx := setupQuotaTest(t)
	live := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 1)
	done := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusCompleted, 1, 1)
	// 每条任务都要有子项：没有子项的任务对推进器不可见（见下面那个专门的测试）。
	addBatchImageTestItem(t, ctx, live.BatchID)

	// 过期时间要在落库前给，所以单独造。
	expired := model.BatchImageJob{
		BatchID:      BatchImageNewID(),
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		Status:       model.BatchImageJobStatusQueued,
		Model:        "batch-image-test-model",
		Provider:     model.BatchImageProviderGeminiAPI,
		OwnerTokenID: 1,
		OwnerUserID:  1,
		ChannelID:    11,
		ItemCount:    1,
	}
	if err := BatchImageCreate(ctx, &expired); err != nil {
		t.Fatalf("create expired job: %v", err)
	}
	addBatchImageTestItem(t, ctx, expired.BatchID)

	jobs, err := BatchImageListExecutable(ctx, 50)
	if err != nil {
		t.Fatalf("list executable: %v", err)
	}
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		seen[job.BatchID] = true
	}
	if !seen[live.BatchID] {
		t.Error("live queued job is missing from the executable list")
	}
	if seen[done.BatchID] {
		t.Error("completed job showed up in the executable list")
	}
	if seen[expired.BatchID] {
		t.Error("expired job showed up in the executable list; the sweeper must handle it, not the advancer")
	}
}

// 每个人在跑的批次数要数对（并发上限靠它）。
func TestBatchImageCountActiveForOwner(t *testing.T) {
	ctx := setupQuotaTest(t)
	newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 42)
	newBatchImageTestJob(t, ctx, model.BatchImageJobStatusSubmitted, 1, 42)
	newBatchImageTestJob(t, ctx, model.BatchImageJobStatusCompleted, 1, 42)

	active, err := BatchImageCountActiveForOwner(ctx, 42)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 2 {
		t.Fatalf("active = %d, want 2 (completed must not count against the limit)", active)
	}
}

// 提交是"先落主记录、再落子项"，中间有一个空窗。
//
// 推进器若在空窗里捡到这条任务，loadItems 会看到 0 条子项，于是按"没东西可提交"
// abort + 退款；而 HTTP 那边可能随后成功返回，客户端拿到一个已经失败的任务。
// 所以"没有子项 = 不可执行"必须是查询层的硬规则，不能靠时序侥幸。
func TestBatchImageListExecutableSkipsJobsWithoutItems(t *testing.T) {
	ctx := setupQuotaTest(t)
	published := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 1)
	addBatchImageTestItem(t, ctx, published.BatchID)
	halfWritten := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusQueued, 1, 1)

	jobs, err := BatchImageListExecutable(ctx, 50)
	if err != nil {
		t.Fatalf("list executable: %v", err)
	}
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		seen[job.BatchID] = true
	}
	if !seen[published.BatchID] {
		t.Error("a queued job with items must be executable")
	}
	if seen[halfWritten.BatchID] {
		t.Error("a queued job with zero items is mid-submit and must not be executable")
	}
}

// addBatchImageTestItem 给某批加一条 pending 子项。
func addBatchImageTestItem(t *testing.T, ctx context.Context, batchID string) {
	t.Helper()
	items := []model.BatchImageItem{{
		BatchID:          batchID,
		CustomID:         "item-1",
		Status:           model.BatchImageItemStatusPending,
		SourceLineNumber: 1,
	}}
	if err := BatchImageItemCreateBatch(ctx, items); err != nil {
		t.Fatalf("create item for %s: %v", batchID, err)
	}
}

// 取消必须"取消即止"：正在跑的索引不能把已取消的条目改回 success。
//
// 【这是真的会漏钱】取消时 Settle 按当时计数退款（取消的条目不计费）。
// 若之后索引把那条从 cancelled 改成 success，任务的 success_count 没变、
// 钱也没再补扣——那张图白送。BatchImageItemIndexResult 的
// `WHERE status = pending` 是唯一的挡板，所以必须钉住它。
func TestBatchImageItemIndexResultCannotResurrectCancelledItem(t *testing.T) {
	ctx := setupQuotaTest(t)
	job := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusIndexing, 1, 1)
	items := []model.BatchImageItem{{
		BatchID:          job.BatchID,
		CustomID:         "cancelled-already",
		Status:           model.BatchImageItemStatusCancelled,
		SourceLineNumber: 1,
	}}
	if err := BatchImageItemCreateBatch(ctx, items); err != nil {
		t.Fatalf("create items: %v", err)
	}

	ok, err := BatchImageItemIndexResult(ctx, items[0].ID, map[string]any{
		"status": model.BatchImageItemStatusSuccess,
	})
	if err != nil {
		t.Fatalf("index result: %v", err)
	}
	if ok {
		t.Fatal("an already-cancelled item was overwritten by the indexer; a cancelled (unbilled) item must stay cancelled")
	}

	// 反向确认：pending 的条目仍然可以正常落结果（别把挡板做成"什么都不写"）。
	pendingJob := newBatchImageTestJob(t, ctx, model.BatchImageJobStatusIndexing, 1, 1)
	pendingItems := []model.BatchImageItem{{
		BatchID:          pendingJob.BatchID,
		CustomID:         "still-pending",
		Status:           model.BatchImageItemStatusPending,
		SourceLineNumber: 1,
	}}
	if err := BatchImageItemCreateBatch(ctx, pendingItems); err != nil {
		t.Fatalf("create pending items: %v", err)
	}
	ok, err = BatchImageItemIndexResult(ctx, pendingItems[0].ID, map[string]any{
		"status": model.BatchImageItemStatusSuccess,
	})
	if err != nil {
		t.Fatalf("index pending: %v", err)
	}
	if !ok {
		t.Fatal("a pending item must accept its result")
	}
}
