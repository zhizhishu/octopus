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

// ErrAuditAlreadyQueued 表示要检测的模型已经全在队列里（等待中或正在跑）。
//
// 单独定义一个错误而不是返回 nil：调用方需要区分「新建了任务」和「本来就排着」，
// 否则前端会显示一个并不存在的任务号。
var ErrAuditAlreadyQueued = errors.New("这些模型已经在检测队列里了")

const (
	// auditResultRetention 结果保留天数。检测是花钱得到的记录，比日志保得久。
	auditResultRetention       = 90 * 24 * time.Hour
	AuditDefaultTimeoutSeconds = 300
	AuditMaxTimeoutSeconds     = 300

	// AuditMaxAttempts 单条结果最多试几次（含第一次）。
	//
	// 定成 3 而不是"一直试到成功"：检测会真的打上游、真的计费，无限重试等于
	// 给一个坏渠道无限烧钱。3 次足够跨过上游抖动，又不至于失控。
	AuditMaxAttempts = 3

	// auditRetryBaseBackoff 第一次重试前的等待，之后每次翻倍（30s → 60s）。
	//
	// 退避的意义不是省事：上游抖动往往是短暂的，立刻重试只是把同一堵墙再撞一次，
	// 还会让"这个渠道真的不通"这个结论迟迟定不下来。
	auditRetryBaseBackoff = 30 * time.Second

	// AuditRunLease 一条 running 结果的**租约**时长。
	//
	// 为什么需要租约：worker 只领取 pending，一旦进程在跑的过程中重启/崩溃，
	// 那行就永远停在 running——任务进度按"未完成"统计，于是任务永远显示"进行中"，
	// 用户看到的就是"点了检测却不出结果"。没有任何东西会去捡它。
	//
	// 这里不新增列，复用 next_retry_at 存"这行下次可以被捡走的时间"：
	// pending 行上它是失败退避，running 行上它是租约到期时间，语义都是"到点才允许再动"。
	AuditRunLease = 120 * time.Second

	// AuditRunHeartbeat 跑的过程中续租的间隔。取租约的 1/4，
	// 容忍连续丢两次续租而不被误判为死掉。
	AuditRunHeartbeat = AuditRunLease / 4
)

// AuditRetryBackoff 第 attempt 次失败后该等多久再试（attempt 从 1 开始）。
//
// 翻倍退避，不加重试上限之外的复杂度：检测失败的绝大多数原因是上游瞬时抖动，
// 指数拉开间隔比"多试几次"更有效，也更容易在日志里看出"它一直在失败"。
func AuditRetryBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return auditRetryBaseBackoff
	}
	d := auditRetryBaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	// 封顶 10 分钟：再长用户会以为任务卡死了，而不是在等重试。
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

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

	// 查库去重：同一个渠道下，已经「在排队」或「正在跑」的模型不再重复入队。
	//
	// 为什么必须在库里查、而不是只靠调用方自觉：
	//   1. 重复点击、刷新页面重发、多标签页并开，都会各自发一次入队请求；
	//   2. 每一次入队最终都会变成真实的上游计费请求；
	//   3. 前端那个 isPending 只挡得住同一个标签页里的连点，挡不住别的。
	// 所以这道闸门必须落在数据库这一层，且要在建任务之前查。
	//
	// 只挡 pending/running：已经跑完（done/failed/skipped）的模型允许再检，
	// 用户想复检是正常需求，不该被拦住。
	activeModels := make(map[string]struct{}, len(models))
	var inflight []string
	if err := db.GetDB().WithContext(ctx).
		Model(&model.ModelAuditResult{}).
		Where("channel_id = ? AND model IN ? AND status IN ?",
			channel.ID, models, []string{model.AuditResultPending, model.AuditResultRunning}).
		Distinct().
		Pluck("model", &inflight).Error; err != nil {
		return 0, err
	}
	for _, m := range inflight {
		activeModels[m] = struct{}{}
	}
	if len(activeModels) > 0 {
		kept := models[:0]
		for _, m := range models {
			if _, busy := activeModels[m]; !busy {
				kept = append(kept, m)
			}
		}
		models = kept
		if len(models) == 0 {
			// 全部都在队列里了：不新建任务，也不报错——用户想要的结果
			// （这些模型会被检测）已经成立，再建一个只会重复花钱。
			return 0, ErrAuditAlreadyQueued
		}
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

// AuditLeaseUntil 计算一条刚被领取的结果该持有的租约到期时间。
func AuditLeaseUntil(now time.Time) time.Time { return now.Add(AuditRunLease) }

// AuditRenewLease 给正在跑的那行续租。返回 false 表示这行已经不该由我们继续跑了
// （被回收、被取消、或已经不是 running），调用方应当尽快收手。
//
// 只在 status 仍是 running 时才续：绝不把已被回收成 pending 的行重新续成"我在跑"。
func AuditRenewLease(ctx context.Context, resultID int64, until time.Time) bool {
	if resultID <= 0 {
		return false
	}
	res := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("id = ? AND status = ?", resultID, model.AuditResultRunning).
		Update("next_retry_at", until)
	return res.Error == nil && res.RowsAffected > 0
}

// AuditReclaimExpired 把租约过期仍是 running 的结果捡回来。
//
// 触发场景只有一个：上次跑它的进程已经不在了（重启、崩溃、被 kill），不会再有人
// 给它续租或收尾。因此这里**只处理过期租约**，绝不无条件把所有 running 重置——
// 多实例或滚动重启时那样做会让正在跑的那一份被重复执行，等于重复花钱。
//
// 处理规则：
//   - 所属任务已取消 → 标 skipped（用户已经不要了，不该再打上游）；
//   - 还有重试预算 → 回 pending 并让 attempt+1（和失败重试共用同一份预算）；
//   - 预算用尽 → 终态 failed，说清楚是"执行中断"，而不是含糊的"没跑成"。
//
// 返回被处理的行数，供调用方决定是否刷新任务进度。
func AuditReclaimExpired(ctx context.Context, limit int) int {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	now := time.Now()
	var expired []model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).
		Where("status = ?", model.AuditResultRunning).
		Where("next_retry_at IS NOT NULL AND next_retry_at <= ?", now).
		Order("id ASC").Limit(limit).Find(&expired).Error; err != nil {
		return 0
	}
	if len(expired) == 0 {
		return 0
	}

	touchedJobs := make(map[int64]struct{}, len(expired))
	for _, res := range expired {
		// 再核一次状态：查出来到写回去之间它可能已经自己收尾了。
		// 用 CAS 保证不把刚跑完的行改回 pending（那会重复花钱）。
		claim := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ?", res.ID, model.AuditResultRunning)
		if res.JobID > 0 {
			touchedJobs[res.JobID] = struct{}{}
		}

		if jobCanceled(ctx, res.JobID) {
			claim.Updates(map[string]any{
				"status":      model.AuditResultSkipped,
				"error_msg":   "任务已取消，执行中断的行不再重跑",
				"finished_at": now,
			})
			continue
		}

		attempt := res.Attempt + 1
		if attempt >= AuditMaxAttempts {
			claim.Updates(map[string]any{
				"status":      model.AuditResultFailed,
				"error_msg":   "执行被中断（进程重启或卡住），已达重试上限",
				"attempt":     attempt,
				"finished_at": now,
			})
			continue
		}
		claim.Updates(map[string]any{
			"status":        model.AuditResultPending,
			"attempt":       attempt,
			"next_retry_at": nil, // 立刻可跑：中断不是上游的错，不必退避
			"error_msg":     "上次执行没结束就中断了（进程重启或卡住），已重新排队",
			"finished_at":   nil,
		})
	}

	for jobID := range touchedJobs {
		ModelAuditJobProgress(ctx, jobID)
	}
	return len(expired)
}

// jobCanceled 判断任务是否已被用户取消。查不到任务时按"未取消"处理：
// 宁可多跑一次能解释的重试，也不要因为一次读失败把用户的检测静默丢掉。
func jobCanceled(ctx context.Context, jobID int64) bool {
	if jobID <= 0 {
		return false
	}
	var job model.ModelAuditJob
	if err := db.GetDB().WithContext(ctx).Select("status").
		Where("id = ?", jobID).First(&job).Error; err != nil {
		return false
	}
	return job.Status == model.AuditJobCanceled
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
