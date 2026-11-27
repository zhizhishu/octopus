package op

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// 检测记录的手动删除：删得掉已经停下的，绝不碰还在花钱的。

func setJobStatus(t *testing.T, jobID int64, jobStatus, resultStatus string) {
	t.Helper()
	if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", jobID).Update("status", jobStatus).Error; err != nil {
		t.Fatalf("set job status: %v", err)
	}
	if err := db.GetDB().Model(&model.ModelAuditResult{}).Where("job_id = ?", jobID).Update("status", resultStatus).Error; err != nil {
		t.Fatalf("set result status: %v", err)
	}
}

func countJobRows(t *testing.T, jobID int64) (jobs, results int64) {
	t.Helper()
	db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", jobID).Count(&jobs)
	db.GetDB().Model(&model.ModelAuditResult{}).Where("job_id = ?", jobID).Count(&results)
	return jobs, results
}

func enqueueTestJob(t *testing.T, chID int, models ...string) int64 {
	t.Helper()
	id, err := ModelAuditEnqueue(t.Context(), ModelAuditEnqueueInput{ChannelID: chID, Models: models})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

func TestAuditJobDeleteRemovesJobAndResults(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "删除渠道")
	done := enqueueTestJob(t, ch.ID, "m-a", "m-b")
	setJobStatus(t, done, model.AuditJobDone, model.AuditResultDone)
	keep := enqueueTestJob(t, ch.ID, "m-c")
	setJobStatus(t, keep, model.AuditJobDone, model.AuditResultDone)

	if err := ModelAuditJobDelete(ctx, done); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if j, r := countJobRows(t, done); j != 0 || r != 0 {
		t.Fatalf("deleted job left rows behind: job=%d results=%d", j, r)
	}
	if j, r := countJobRows(t, keep); j != 1 || r != 1 {
		t.Fatalf("an unrelated job was touched: job=%d results=%d", j, r)
	}
	if err := ModelAuditJobDelete(ctx, done); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("second delete should say not found, got %v", err)
	}
}

func TestAuditJobDeleteRefusesBusyJobs(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "忙渠道")
	queued := enqueueTestJob(t, ch.ID, "q")
	if err := ModelAuditJobDelete(ctx, queued); !errors.Is(err, ErrAuditJobBusy) {
		t.Fatalf("queued job must not be deleted, got %v", err)
	}
	// 已取消但还有一行在收尾：取消只拦新请求，正在跑的那次还在花钱。
	tail := enqueueTestJob(t, ch.ID, "tail")
	setJobStatus(t, tail, model.AuditJobCanceled, model.AuditResultRunning)
	if err := ModelAuditJobDelete(ctx, tail); !errors.Is(err, ErrAuditJobBusy) {
		t.Fatalf("canceled job with a running row must not be deleted, got %v", err)
	}
	if j, r := countJobRows(t, tail); j != 1 || r != 1 {
		t.Fatalf("busy job lost rows: job=%d results=%d", j, r)
	}
}

func TestAuditJobsBatchDeleteSkipsBusy(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "批量渠道")
	a := enqueueTestJob(t, ch.ID, "a")
	setJobStatus(t, a, model.AuditJobDone, model.AuditResultFailed)
	b := enqueueTestJob(t, ch.ID, "b")
	setJobStatus(t, b, model.AuditJobCanceled, model.AuditResultSkipped)
	running := enqueueTestJob(t, ch.ID, "r")
	setJobStatus(t, running, model.AuditJobRunning, model.AuditResultRunning)

	out, err := ModelAuditJobsDelete(ctx, []int64{a, b, running, a, -1})
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if out.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", out.Deleted)
	}
	if len(out.SkippedBusy) != 1 || out.SkippedBusy[0] != running {
		t.Fatalf("skipped = %v, want [%d]", out.SkippedBusy, running)
	}
	if j, _ := countJobRows(t, running); j != 1 {
		t.Fatalf("the running job was deleted")
	}
	if _, err := ModelAuditJobsDelete(ctx, nil); err == nil {
		t.Fatalf("empty id list must be an error, not a silent no-op")
	}
}

func TestAuditJobsClearFinishedKeepsActive(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "清空渠道")
	done := enqueueTestJob(t, ch.ID, "d")
	setJobStatus(t, done, model.AuditJobDone, model.AuditResultDone)
	canceled := enqueueTestJob(t, ch.ID, "c")
	setJobStatus(t, canceled, model.AuditJobCanceled, model.AuditResultSkipped)
	tail := enqueueTestJob(t, ch.ID, "t")
	setJobStatus(t, tail, model.AuditJobCanceled, model.AuditResultRunning)
	running := enqueueTestJob(t, ch.ID, "r")
	setJobStatus(t, running, model.AuditJobRunning, model.AuditResultRunning)
	queued := enqueueTestJob(t, ch.ID, "q")

	out, err := ModelAuditJobsClearFinished(ctx)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if out.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (done + canceled)", out.Deleted)
	}
	for _, id := range []int64{tail, running, queued} {
		if j, r := countJobRows(t, id); j != 1 || r != 1 {
			t.Fatalf("job %d should survive clear: job=%d results=%d", id, j, r)
		}
	}
	for _, id := range []int64{done, canceled} {
		if j, r := countJobRows(t, id); j != 0 || r != 0 {
			t.Fatalf("job %d should be gone: job=%d results=%d", id, j, r)
		}
	}
}

func TestModelAuditResultPurgeSkipsBusyRows(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "定期清理渠道")

	clean := enqueueTestJob(t, ch.ID, "old-clean")
	setJobStatus(t, clean, model.AuditJobDone, model.AuditResultDone)
	busyTail := enqueueTestJob(t, ch.ID, "old-tail")
	setJobStatus(t, busyTail, model.AuditJobCanceled, model.AuditResultRunning)
	recent := enqueueTestJob(t, ch.ID, "recent-clean")
	setJobStatus(t, recent, model.AuditJobDone, model.AuditResultDone)

	oldCreatedAt := time.Now().Add(-91 * 24 * time.Hour)
	for _, id := range []int64{clean, busyTail} {
		if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", id).Update("created_at", oldCreatedAt).Error; err != nil {
			t.Fatalf("age job %d: %v", id, err)
		}
	}

	if err := ModelAuditResultPurge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if jobs, results := countJobRows(t, clean); jobs != 0 || results != 0 {
		t.Fatalf("expired finished job was not purged: jobs=%d results=%d", jobs, results)
	}
	if jobs, results := countJobRows(t, busyTail); jobs != 1 || results != 1 {
		t.Fatalf("purge touched a canceled job still settling a running row: jobs=%d results=%d", jobs, results)
	}
	if jobs, results := countJobRows(t, recent); jobs != 1 || results != 1 {
		t.Fatalf("purge touched a recent job: jobs=%d results=%d", jobs, results)
	}
}

// fix-1127 返工第 9 条：最老 200 条全是 busy（结果行卡 running）时，清理不能空转——
// busy 在查询时就排除，较新的可清理任务照样进得了候选。205 条 busy 占满窗口，
// 第 206 条干净的到期任务必须被清掉（修复前：它永远排不进 Limit 200）。
func TestModelAuditResultPurgeBusyRowsDoNotEatTheWindow(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "清理窗口渠道")

	oldCreatedAt := time.Now().Add(-91 * 24 * time.Hour)
	for i := 0; i < 205; i++ {
		id := enqueueTestJob(t, ch.ID, "busy-model-"+strconv.Itoa(i))
		setJobStatus(t, id, model.AuditJobCanceled, model.AuditResultRunning)
		if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", id).Update("created_at", oldCreatedAt).Error; err != nil {
			t.Fatalf("age busy job: %v", err)
		}
	}
	clean := enqueueTestJob(t, ch.ID, "clean-model")
	setJobStatus(t, clean, model.AuditJobDone, model.AuditResultDone)
	if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", clean).Update("created_at", oldCreatedAt).Error; err != nil {
		t.Fatalf("age clean job: %v", err)
	}

	if err := ModelAuditResultPurge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if jobs, _ := countJobRows(t, clean); jobs != 0 {
		t.Fatalf("clean job beyond a busy-only window was not purged (busy rows ate the limit): jobs=%d", jobs)
	}
}

// fix-1127 返工 r2 第 6 条：结果行里混入 job_id 为 NULL 的散行（无任务旧数据）时，
// NOT IN 三值逻辑不能让清理整条空转——可删的到期任务照样被清。
func TestModelAuditResultPurgeSurvivesNullJobIDRows(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "NULL job_id 渠道")

	clean := enqueueTestJob(t, ch.ID, "null-clean")
	setJobStatus(t, clean, model.AuditJobDone, model.AuditResultDone)
	oldCreatedAt := time.Now().Add(-91 * 24 * time.Hour)
	if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", clean).Update("created_at", oldCreatedAt).Error; err != nil {
		t.Fatalf("age job: %v", err)
	}
	// 造一行 job_id 为 NULL 且仍 running 的散行（旧数据/无主行）：没有
	// job_id IS NOT NULL 过滤时，它会让 busy 子查询含 NULL，NOT IN 恒不为 TRUE。
	stray := enqueueTestJob(t, ch.ID, "null-stray")
	var strayResultID int64
	if err := db.GetDB().Model(&model.ModelAuditResult{}).Where("job_id = ?", stray).Select("id").Scan(&strayResultID).Error; err != nil {
		t.Fatalf("find stray result: %v", err)
	}
	if err := db.GetDB().Exec("UPDATE model_audit_results SET job_id = NULL, status = ? WHERE id = ?", model.AuditResultRunning, strayResultID).Error; err != nil {
		t.Fatalf("null stray job_id: %v", err)
	}

	if err := ModelAuditResultPurge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if jobs, _ := countJobRows(t, clean); jobs != 0 {
		t.Fatalf("clean job not purged when a NULL job_id row exists (NOT IN three-valued logic): jobs=%d", jobs)
	}
}
