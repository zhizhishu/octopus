package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func setupAuditQueueTest(t *testing.T) context.Context {
	t.Helper()
	ctx := setupRelayLogTest(t)
	if err := db.GetDB().AutoMigrate(&model.ModelAuditJob{}, &model.ModelAuditResult{}); err != nil {
		t.Fatalf("migrate audit tables: %v", err)
	}
	return ctx
}

// ensureAuditChannel 走 op 层的建渠道入口而不是直接写 DB：
// ChannelGet 读的是内存 channelCache，直接 Create 到库里查不到。
func ensureAuditChannel(t *testing.T, name string) *model.Channel {
	t.Helper()
	ch := model.Channel{Name: name, Type: 0, Enabled: true}
	if err := ChannelCreate(&ch, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return &ch
}

// TestAuditEnqueueCreatesJobAndResults 守入队的基本形态：
// 一个任务 + 每个模型一行 pending 结果。
func TestAuditEnqueueCreatesJobAndResults(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "测试渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-4o", "claude-3"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if jobID <= 0 {
		t.Fatalf("应返回有效任务 id")
	}

	job, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if job.Total != 2 || job.Status != model.AuditJobQueued {
		t.Fatalf("任务应 queued 且 total=2，得到 status=%s total=%d", job.Status, job.Total)
	}
	if len(results) != 2 {
		t.Fatalf("应有 2 行结果，得到 %d", len(results))
	}
	for _, r := range results {
		if r.Status != model.AuditResultPending {
			t.Fatalf("新入队结果应为 pending，得到 %s", r.Status)
		}
		if r.ChannelName != "测试渠道" {
			t.Fatalf("渠道名应快照进结果行，得到 %q", r.ChannelName)
		}
	}
}

// TestAuditEnqueueDeduplicatesModels 守同一批里的重复模型只算一次：
// 每次检测都是真花钱，重复项必须去掉。
func TestAuditEnqueueDeduplicatesModels(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "去重渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"m1", "m1", "  m1  ", "m2", ""},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("重复与空模型应被去掉，期望 2 行，得到 %d", len(results))
	}
}

// TestAuditEnqueueRejectsEmptyModels 空模型列表必须报错，不能建出一个空任务。
func TestAuditEnqueueRejectsEmptyModels(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "空渠道")

	if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"  ", ""},
	}); err == nil {
		t.Fatalf("空模型列表应报错")
	}
	if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: 0,
		Models:    []string{"m"},
	}); err == nil {
		t.Fatalf("缺渠道应报错")
	}
}

// TestAuditJobProgressReachesDone 守进度收敛：
// 所有结果行完成后任务必须变 done，不能永远停在 running。
func TestAuditJobProgressReachesDone(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "进度渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 逐个标完成，模拟 worker 推进。
	for i := 0; i < 3; i++ {
		var res model.ModelAuditResult
		if err := db.GetDB().Where("job_id = ? AND status = ?", jobID, model.AuditResultPending).
			Order("id ASC").First(&res).Error; err != nil {
			t.Fatalf("取第 %d 个待办: %v", i, err)
		}
		db.GetDB().Model(&model.ModelAuditResult{}).Where("id = ?", res.ID).
			Update("status", model.AuditResultDone)
		ModelAuditJobProgress(ctx, jobID)

		job, _, _ := ModelAuditJobDetail(ctx, jobID)
		if job.Finished != i+1 {
			t.Fatalf("第 %d 步 finished 应为 %d，得到 %d", i, i+1, job.Finished)
		}
	}

	job, _, _ := ModelAuditJobDetail(ctx, jobID)
	if job.Status != model.AuditJobDone {
		t.Fatalf("全部完成后任务应为 done，得到 %s", job.Status)
	}
	if job.EndedAt == nil {
		t.Fatalf("done 任务应有结束时间")
	}
}

// TestAuditJobCancelSkipsPending 守取消语义：
// 未开始的结果标 skipped，已完成的结论不动。
func TestAuditJobCancelSkipsPending(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "取消渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"x", "y", "z"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 先把第一个标成 done，模拟它已经跑完。
	var first model.ModelAuditResult
	db.GetDB().Where("job_id = ?", jobID).Order("id ASC").First(&first)
	db.GetDB().Model(&model.ModelAuditResult{}).Where("id = ?", first.ID).
		Updates(map[string]any{"status": model.AuditResultDone, "verdict": "none"})

	if err := ModelAuditJobCancel(ctx, jobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	job, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if job.Status != model.AuditJobCanceled {
		t.Fatalf("任务应为 canceled，得到 %s", job.Status)
	}

	done, skipped := 0, 0
	for _, r := range results {
		switch r.Status {
		case model.AuditResultDone:
			done++
			if r.Verdict != "none" {
				t.Fatalf("已完成的结论不应被取消改动，得到 verdict=%q", r.Verdict)
			}
		case model.AuditResultSkipped:
			skipped++
		}
	}
	if done != 1 || skipped != 2 {
		t.Fatalf("应保留 1 个已完成、跳过 2 个，得到 done=%d skipped=%d", done, skipped)
	}
}

// TestAuditJobListOrdersNewestFirst 列表必须最新在前，否则用户找不到刚提交的任务。
func TestAuditJobListOrdersNewestFirst(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "列表渠道")

	for i := 0; i < 3; i++ {
		// 每次都换一个模型名：入队会挡掉「同渠道同模型且还在队列里」的重复提交，
		// 这里要测的是列表排序，不是去重，所以不该撞那道闸门。
		if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
			ChannelID: ch.ID,
			Models:    []string{fmt.Sprintf("m-%d", i)},
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	jobs, err := ModelAuditJobList(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("应有 3 个任务，得到 %d", len(jobs))
	}
	for i := 1; i < len(jobs); i++ {
		if jobs[i-1].ID < jobs[i].ID {
			t.Fatalf("任务应按 id 降序：%d 在 %d 之前", jobs[i-1].ID, jobs[i].ID)
		}
	}
}

// TestAuditRetryBackoffGrowsAndCaps 守退避曲线的两个性质：
//  1. 单调递增（否则重试等于连撞同一堵墙）
//  2. 有上限（否则用户以为任务卡死，而不是在等重试）
func TestAuditRetryBackoffGrowsAndCaps(t *testing.T) {
	prev := AuditRetryBackoff(1)
	if prev <= 0 {
		t.Fatalf("第一次退避必须为正，实际 %v", prev)
	}
	for attempt := 2; attempt <= 6; attempt++ {
		cur := AuditRetryBackoff(attempt)
		if cur < prev {
			t.Fatalf("退避必须单调不减: attempt=%d %v < %v", attempt, cur, prev)
		}
		prev = cur
	}
	if got := AuditRetryBackoff(20); got > 10*time.Minute {
		t.Fatalf("退避必须有上限，实际 %v", got)
	}
}

// TestAuditRetryFieldsPersist 守"重试状态落在行上"。
//
// 为什么这条重要：重试如果只活在内存里，进程一重启就全丢——用户点了检测、
// 跑失败该重试的，重启后永远停在失败。所以 attempt / next_retry_at 必须是
// 查得回来的列，不能是 worker 的局部变量。
func TestAuditRetryFieldsPersist(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "重试渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"retry-model"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("job_id = ?", jobID).First(&res).Error; err != nil {
		t.Fatalf("load result: %v", err)
	}
	if res.Attempt != 0 {
		t.Fatalf("新入队的 attempt 应为 0，实际 %d", res.Attempt)
	}

	// 模拟一次失败：回到 pending 并设置退避时间。
	next := time.Now().Add(AuditRetryBackoff(1))
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("id = ?", res.ID).
		Updates(map[string]any{
			"status":        model.AuditResultPending,
			"attempt":       1,
			"next_retry_at": next,
		}).Error; err != nil {
		t.Fatalf("update: %v", err)
	}

	var reread model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&reread).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reread.Attempt != 1 {
		t.Fatalf("attempt 应持久化为 1，实际 %d", reread.Attempt)
	}
	if reread.NextRetryAt == nil {
		t.Fatal("next_retry_at 应持久化，重启后靠它继续退避")
	}
}

// TestAuditMaxAttemptsIsFinite 守"重试有上限"。
//
// 检测真打上游、真计费。若上限被改成 0 或负数，重试逻辑会退化成无限重试，
// 一个坏渠道能被反复烧钱。这条测试是那个数字的护栏。
func TestAuditMaxAttemptsIsFinite(t *testing.T) {
	if AuditMaxAttempts < 2 {
		t.Fatalf("重试上限至少 2（1 次首试 + 1 次重试），实际 %d", AuditMaxAttempts)
	}
	if AuditMaxAttempts > 10 {
		t.Fatalf("重试上限不应超过 10（烧钱风险），实际 %d", AuditMaxAttempts)
	}
}

// TestAuditEnqueueRefusesDuplicateInFlight 守"重复入队=重复花钱"这道闸门。
//
// 这条以前是假的：注释写着"已在队列里不重复入队"，实现却只做了同一批内部的
// 去重，根本没查库。结果重复点击、多标签页各发一次请求，就会真的建出两个任务，
// 同一批模型被测两遍、付两次钱。
func TestAuditEnqueueRefusesDuplicateInFlight(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "去重渠道")

	first, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-4o"},
	})
	if err != nil {
		t.Fatalf("首次入队应成功: %v", err)
	}
	if first <= 0 {
		t.Fatalf("首次入队应返回任务 id")
	}

	// 同一个模型再来一次：必须被挡住，且明确告诉调用方"已经在队列里"。
	second, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-4o"},
	})
	if !errors.Is(err, ErrAuditAlreadyQueued) {
		t.Fatalf("重复入队应返回 ErrAuditAlreadyQueued，得到 err=%v jobID=%d", err, second)
	}

	// 库里必须只有一个任务、一行结果 —— 这才是"没多花钱"的硬证据。
	jobs, err := ModelAuditJobList(ctx, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("应只有 1 个任务，得到 %d", len(jobs))
	}
	var n int64
	if err := db.GetDB().Model(&model.ModelAuditResult{}).Where("channel_id = ?", ch.ID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只有 1 行结果，得到 %d", n)
	}
}

// TestAuditEnqueueAllowsRetestAfterFinish 守另一半：跑完的模型允许再检。
//
// 去重只该挡"还在排队或正在跑"的，把已完成的也挡住就等于禁止复检——
// 用户复查一个可疑模型是正当需求，拦下来是过度设计。
func TestAuditEnqueueAllowsRetestAfterFinish(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "复检渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-4o"},
	})
	if err != nil {
		t.Fatalf("首次入队: %v", err)
	}
	// 把这行结果标成已完成，模拟上一轮跑完了。
	if err := db.GetDB().Model(&model.ModelAuditResult{}).
		Where("job_id = ?", jobID).
		Update("status", model.AuditResultDone).Error; err != nil {
		t.Fatalf("mark done: %v", err)
	}

	retest, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"gpt-4o"},
	})
	if err != nil {
		t.Fatalf("已完成的模型应允许复检，得到 err=%v", err)
	}
	if retest <= 0 {
		t.Fatalf("复检应返回新的任务 id")
	}
}

// TestAuditEnqueueKeepsOnlyFreshModels 守混合场景：
// 一批里有的在队列里、有的不在，只把不在的排进去，已在的不重复。
func TestAuditEnqueueKeepsOnlyFreshModels(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "混合渠道")

	if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"busy-model"},
	}); err != nil {
		t.Fatalf("首次入队: %v", err)
	}

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"busy-model", "fresh-model"},
	})
	if err != nil {
		t.Fatalf("混合入队应成功: %v", err)
	}
	job, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if job.Total != 1 || len(results) != 1 {
		t.Fatalf("只应排入 1 个新模型，得到 total=%d results=%d", job.Total, len(results))
	}
	if results[0].Model != "fresh-model" {
		t.Fatalf("排进去的应该是 fresh-model，得到 %s", results[0].Model)
	}
}

// ---- 租约与过期回收 ----
//
// 这一组守的是「点了检测却永远不出结果」最隐蔽的那条路径：worker 只领取 pending，
// 而进程一旦在跑的过程中重启/崩溃，那行就永远停在 running —— 没有任何东西会去捡它，
// 任务进度按"未完成"统计，页面永远显示"进行中"。

// markRunningWithLease 模拟 worker 抢占成功：status=running 且带一个租约到期时间。
func markRunningWithLease(t *testing.T, ctx context.Context, resultID int64, until time.Time, attempt int) {
	t.Helper()
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("id = ?", resultID).
		Updates(map[string]any{
			"status":        model.AuditResultRunning,
			"next_retry_at": until,
			"attempt":       attempt,
		}).Error; err != nil {
		t.Fatalf("标记 running: %v", err)
	}
}

func loadAuditResult(t *testing.T, ctx context.Context, id int64) model.ModelAuditResult {
	t.Helper()
	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", id).First(&res).Error; err != nil {
		t.Fatalf("读结果行: %v", err)
	}
	return res
}

// TestAuditReclaimExpiredTerminatesWithoutRerun 守主路径：租约过期 → 直接终止，**绝不重跑**。
//
// 为什么不重跑：租约过期只说明心跳停了 120 秒，不说明上游没收到请求。探针可能已经
// 发出去、已经计费，只是我们没等到回包。自动重跑等于替用户再买一次单，而这个项目
// 的立场是"宁可这次没测成，也不悄悄花第二次钱"。
func TestAuditReclaimExpiredTerminatesWithoutRerun(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "回收渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"orphan"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	resID := results[0].ID

	// 租约已经过期 —— 等价于"跑它的那个进程已经不在了"。
	markRunningWithLease(t, ctx, resID, time.Now().Add(-time.Minute), 0)

	if n := AuditReclaimExpired(ctx, 50); n != 1 {
		t.Fatalf("应回收 1 行，实际 %d", n)
	}

	got := loadAuditResult(t, ctx, resID)
	if got.Status != model.AuditResultFailed {
		t.Fatalf("租约过期应直接终止为 failed（不重跑），得到 %s", got.Status)
	}
	if got.Attempt != 1 {
		t.Fatalf("应如实记一次尝试，得到 attempt=%d", got.Attempt)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("终态行不该再带租约，next_retry_at 应清空，得到 %v", got.NextRetryAt)
	}
	if !strings.Contains(got.ErrorMsg, "中断") {
		t.Fatalf("error_msg 应说清是中断，得到 %q", got.ErrorMsg)
	}
	// 用户要能看懂"为什么没自动重跑"，否则会以为程序忘了。
	if !strings.Contains(got.ErrorMsg, "不自动重跑") {
		t.Fatalf("error_msg 应说明不自动重跑的原因，得到 %q", got.ErrorMsg)
	}
	if got.FinishedAt == nil {
		t.Fatal("终态行必须有结束时间")
	}
}

// TestAuditReclaimLeavesLiveLeaseAlone 守反向边界：租约没过期的 running 行绝不能被回收。
//
// 这条是"重复花钱"的护栏：误回收一条正在跑的检测，同一次检测就会被做两遍、计两次费。
func TestAuditReclaimLeavesLiveLeaseAlone(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "活跃渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"busy"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, results, _ := ModelAuditJobDetail(ctx, jobID)
	resID := results[0].ID

	markRunningWithLease(t, ctx, resID, time.Now().Add(time.Minute), 0)

	if n := AuditReclaimExpired(ctx, 50); n != 0 {
		t.Fatalf("未过期的租约不该被回收，实际回收 %d 行", n)
	}
	if got := loadAuditResult(t, ctx, resID); got.Status != model.AuditResultRunning {
		t.Fatalf("仍在跑的行应保持 running，得到 %s", got.Status)
	}
}

// TestAuditReclaimNeverRequeuesWhateverTheAttempt 守"永不重跑"这条底线：
// 无论这一行之前跑过几次，租约过期都只能走向终态，不能回到 pending。
//
// 这条是花钱的护栏。回 pending 就意味着 worker 会再打一次上游——同一次检测被计两次费。
// 以前这里按"还有预算就重排"，astra 复核时指出：过期不等于没发请求，重排就是替用户
// 二次买单，于是改成一律终止。
func TestAuditReclaimNeverRequeuesWhateverTheAttempt(t *testing.T) {
	for _, attempts := range []int{0, 1, AuditMaxAttempts - 1, AuditMaxAttempts + 5} {
		t.Run(fmt.Sprintf("attempt_%d", attempts), func(t *testing.T) {
			ctx := setupAuditQueueTest(t)
			ch := ensureAuditChannel(t, fmt.Sprintf("不重跑渠道-%d", attempts))

			jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
				ChannelID: ch.ID,
				Models:    []string{"crashy"},
			})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			_, results, _ := ModelAuditJobDetail(ctx, jobID)
			resID := results[0].ID

			markRunningWithLease(t, ctx, resID, time.Now().Add(-time.Minute), attempts)

			if n := AuditReclaimExpired(ctx, 50); n != 1 {
				t.Fatalf("应处理 1 行，实际 %d", n)
			}

			got := loadAuditResult(t, ctx, resID)
			if got.Status == model.AuditResultPending {
				t.Fatalf("attempt=%d 被重排回 pending —— 这会二次计费", attempts)
			}
			if got.Status != model.AuditResultFailed {
				t.Fatalf("attempt=%d 应落 failed，得到 %s", attempts, got.Status)
			}
			if got.Attempt != attempts+1 {
				t.Fatalf("attempt 应+1 记账，期望 %d 得到 %d", attempts+1, got.Attempt)
			}
		})
	}
}

// TestAuditReclaimSkipsCanceledJob 守取消优先：任务已取消时，中断的行标 skipped 而不是重跑。
//
// 用户点取消就是不希望再打上游；把它捡回来重跑既违反意图，也白花钱。
func TestAuditReclaimSkipsCanceledJob(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "取消回收渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"canceled-model"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, results, _ := ModelAuditJobDetail(ctx, jobID)
	resID := results[0].ID

	// 先把行标成"正在跑"，再取消任务：取消只处理 pending，跑着的这行会留在 running。
	markRunningWithLease(t, ctx, resID, time.Now().Add(-time.Minute), 0)
	if err := ModelAuditJobCancel(ctx, jobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if n := AuditReclaimExpired(ctx, 50); n != 1 {
		t.Fatalf("应处理 1 行，实际 %d", n)
	}
	if got := loadAuditResult(t, ctx, resID); got.Status != model.AuditResultSkipped {
		t.Fatalf("已取消任务的行应标 skipped，得到 %s", got.Status)
	}
}

// TestAuditRenewLeaseOnlyTouchesRunning 守续租的边界：
// 只有仍在 running 的行才续得上；已被回收成 pending 的不能再被续成"我在跑"。
func TestAuditRenewLeaseOnlyTouchesRunning(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "续租渠道")

	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"lease-model"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, results, _ := ModelAuditJobDetail(ctx, jobID)
	resID := results[0].ID

	// pending 行续不上：它不属于任何执行者。
	if AuditRenewLease(ctx, resID, AuditLeaseUntil(time.Now())) {
		t.Fatal("pending 行不该续租成功")
	}

	markRunningWithLease(t, ctx, resID, time.Now().Add(-time.Minute), 0)
	until := AuditLeaseUntil(time.Now())
	if !AuditRenewLease(ctx, resID, until) {
		t.Fatal("running 行应能续租")
	}
	got := loadAuditResult(t, ctx, resID)
	if got.NextRetryAt == nil || got.NextRetryAt.Before(time.Now()) {
		t.Fatalf("续租后到期时间应在未来，得到 %v", got.NextRetryAt)
	}
}

// TestAuditLeaseWindowIsSane 守租约与心跳的关系：
// 心跳必须明显短于租约（否则一次丢心跳就被误判为死），租约也不该长到让用户干等。
func TestAuditLeaseWindowIsSane(t *testing.T) {
	if AuditRunLease <= AuditRunHeartbeat {
		t.Fatalf("租约(%v)必须大于心跳(%v)", AuditRunLease, AuditRunHeartbeat)
	}
	if AuditRunLease < 2*AuditRunHeartbeat {
		t.Fatalf("租约至少要容得下连续两次心跳(%v vs %v)", AuditRunLease, AuditRunHeartbeat)
	}
	if AuditRunLease > 10*time.Minute {
		t.Fatalf("租约过长会让重启后的卡死行干等太久，实际 %v", AuditRunLease)
	}
	if AuditRunHeartbeat <= 0 {
		t.Fatalf("心跳必须为正，实际 %v", AuditRunHeartbeat)
	}
}

// TestAuditEnqueueLeavesNoGhostJobWhenResultInsertFails 守入队的原子性。
//
// 生产上真出现过"空壳任务"：任务先落库、结果行逐条插，第二步炸了（结果表缺列），
// 于是库里留下一个只有任务、没有结果行的壳——它永远显示"排队中"，队列里却没有可跑
// 的活。用户看到的就是"点了检测没反应"，而真正的报错只在 toast 里闪一下就没了。
//
// 这里用 SQLite 触发器把"写结果行"这件事定向打炸（任务那一步仍然成功），
// 要求：入队必须报错，并且不能留下任何任务行。
func TestAuditEnqueueLeavesNoGhostJobWhenResultInsertFails(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "原子性渠道")

	if err := db.GetDB().Exec(
		`CREATE TRIGGER fail_audit_result BEFORE INSERT ON model_audit_results
		 BEGIN SELECT RAISE(ABORT, 'injected result-insert failure'); END;`,
	).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{ChannelID: ch.ID, Models: []string{"m-atomic"}})
	if err == nil {
		t.Fatal("结果行明明写不进去，入队却成功了 —— 会留下空壳任务")
	}

	var jobs int64
	if err := db.GetDB().Model(&model.ModelAuditJob{}).
		Where("channel_id = ?", ch.ID).Count(&jobs).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("写结果行失败后仍留下 %d 个任务行，事务没有回滚", jobs)
	}

	var results int64
	if err := db.GetDB().Model(&model.ModelAuditResult{}).
		Where("channel_id = ?", ch.ID).Count(&results).Error; err != nil {
		t.Fatalf("count results: %v", err)
	}
	if results != 0 {
		t.Fatalf("写结果行失败后仍留下 %d 行结果，事务没有回滚", results)
	}
}
