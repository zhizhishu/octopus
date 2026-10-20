package op

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
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
		if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
			ChannelID: ch.ID,
			Models:    []string{"m"},
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
