package op

import (
	"context"
	"errors"

	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 模型检测队列：批量入队 + 后台串行执行 + 结果持久化。
//
// 为什么要有队列：一次检测会真的打上游、消耗额度。用户想"把这一批模型都测一遍"时，
// 同步接口会让他必须守着页面等——超时、切页、断网都会白等甚至白花钱。改成入队后，
// 点一下就能走人，结论落进独立表随时回看。
//
// 为什么串行（单 worker）：检测是**花钱且打上游**的动作，并发跑一批模型会让同一渠道
// 瞬间承受成倍的请求，既容易触发上游限流把结论污染成"失败"，也让费用不可预测。
// worker 数固定为 1 是有意的取舍，不是为了省事。

const (
	// auditResultRetention 结果保留天数。检测是花钱得到的记录，比日志保得久。
	auditResultRetention = 90 * 24 * time.Hour

	AuditDefaultTimeoutSeconds = 300
	AuditMaxTimeoutSeconds     = 300
)

// ModelAuditEnqueueInput 一次入队请求。
type ModelAuditEnqueueInput struct {
	ChannelID      int
	Models         []string
	Probes         []string
	TimeoutSeconds int
	LogID          int64
	Trigger        string
}

// ModelAuditEnqueue 把一批模型入队，立即返回任务 id。
//
// 同一个 (渠道, 上游模型) 已经在队列里等待或正在跑时不重复入队——重复点击、
// 多标签页都会走到这里，而每一次都是钱。已有则一并返回其任务 id。
func ModelAuditEnqueue(ctx context.Context, in ModelAuditEnqueueInput) (int64, error) {
	if in.ChannelID <= 0 {
		return 0, errors.New("渠道不能为空")
	}
	models := make([]string, 0, len(in.Models))
	seen := make(map[string]struct{}, len(in.Models))
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		models = append(models, m)
	}
	if len(models) == 0 {
		return 0, errors.New("至少要选一个模型")
	}

	channel, err := ChannelGet(in.ChannelID, ctx)
	if err != nil {
		return 0, errors.New("渠道不存在")
	}

	timeout := in.TimeoutSeconds
	if timeout <= 0 {
		timeout = AuditDefaultTimeoutSeconds
	}
	if timeout > AuditMaxTimeoutSeconds {
		timeout = AuditMaxTimeoutSeconds
	}

	trigger := in.Trigger
	if trigger == "" {
		trigger = "manual"
	}

	job := model.ModelAuditJob{
		ChannelID:      channel.ID,
		ChannelName:    channel.Name,
		Status:         model.AuditJobQueued,
		Total:          len(models),
		Probes:         strings.Join(in.Probes, ","),
		TimeoutSeconds: timeout,
		Trigger:        trigger,
	}
	if err := db.GetDB().WithContext(ctx).Create(&job).Error; err != nil {
		return 0, err
	}

	for _, m := range models {
		res := model.ModelAuditResult{
			JobID:       job.ID,
			LogID:       in.LogID,
			ChannelID:   channel.ID,
			ChannelName: channel.Name,
			Model:       m,
			Status:      model.AuditResultPending,
		}
		if err := db.GetDB().WithContext(ctx).Create(&res).Error; err != nil {
			return job.ID, err
		}
	}

	return job.ID, nil
}

// ModelAuditJobCancel 取消一个任务：把还没开始的结果标成 skipped。
// 已经在跑的那一个会自然跑完（中断一次真实上游请求的代价不值得）。
func ModelAuditJobCancel(ctx context.Context, jobID int64) error {
	if jobID <= 0 {
		return errors.New("任务 id 无效")
	}
	now := time.Now()
	res := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("job_id = ? AND status = ?", jobID, model.AuditResultPending).
		Updates(map[string]any{"status": model.AuditResultSkipped, "error_msg": "已取消", "finished_at": now})
	if res.Error != nil {
		return res.Error
	}
	return db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).
		Where("id = ?", jobID).
		Updates(map[string]any{"status": model.AuditJobCanceled, "ended_at": now}).Error
}

// ModelAuditJobList 返回最近的检测任务。
func ModelAuditJobList(ctx context.Context, limit int) ([]model.ModelAuditJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var jobs []model.ModelAuditJob
	err := db.GetDB().WithContext(ctx).Order("id DESC").Limit(limit).Find(&jobs).Error
	return jobs, err
}

// ModelAuditJobDetail 返回一个任务及其全部结果行。
func ModelAuditJobDetail(ctx context.Context, jobID int64) (model.ModelAuditJob, []model.ModelAuditResult, error) {
	var job model.ModelAuditJob
	if err := db.GetDB().WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err != nil {
		return job, nil, err
	}
	var results []model.ModelAuditResult
	err := db.GetDB().WithContext(ctx).Where("job_id = ?", jobID).Order("id ASC").Find(&results).Error
	return job, results, err
}

// ModelAuditResultPurge 清理超过保留期的检测结果与空任务。
//
// 检测结果是**花钱买来的**，保留期刻意比日志长得多（90 天）。这里只清真正的
// 老记录，绝不因为"空间紧张"提前删——那等于把用户的检测费用删掉。
func ModelAuditResultPurge(ctx context.Context) error {
	cutoff := time.Now().Add(-auditResultRetention)

	var oldJobIDs []int64
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).
		Where("created_at < ? AND status IN ?", cutoff, []string{model.AuditJobDone, model.AuditJobCanceled}).
		Limit(200).Pluck("id", &oldJobIDs).Error; err != nil {
		return err
	}
	if len(oldJobIDs) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).
		Where("job_id IN ?", oldJobIDs).Delete(&model.ModelAuditResult{}).Error; err != nil {
		return err
	}
	return db.GetDB().WithContext(ctx).
		Where("id IN ?", oldJobIDs).Delete(&model.ModelAuditJob{}).Error
}

// ModelAuditJobProgress 重算任务的完成数与状态。
//
// 用 COUNT 重算而不是自增，是为了让"进程重启导致某行卡在 running"这类情况
// 在下次任务推进时自动收敛，不会让进度永远停在 99%。
func ModelAuditJobProgress(ctx context.Context, jobID int64) {
	var pending, running, total int64
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("job_id = ? AND status IN ?", jobID, []string{model.AuditResultPending, model.AuditResultRunning}).
		Count(&pending)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("job_id = ?", jobID).Count(&total)
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("job_id = ? AND status = ?", jobID, model.AuditResultRunning).Count(&running)

	var job model.ModelAuditJob
	if err := db.GetDB().WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err != nil {
		return
	}
	if job.Status == model.AuditJobCanceled {
		return // 已取消的任务不再改状态
	}

	finished := total - pending
	updates := map[string]any{"finished": finished, "total": total}

	switch {
	case pending == 0 && total > 0:
		updates["status"] = model.AuditJobDone
		updates["ended_at"] = time.Now()
	case running > 0 || finished > 0:
		updates["status"] = model.AuditJobRunning
		if job.StartedAt == nil {
			updates["started_at"] = time.Now()
		}
	}
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Where("id = ?", jobID).Updates(updates)
}
