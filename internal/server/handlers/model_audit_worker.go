package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// 检测队列的**执行侧**：worker 循环 + 单模型执行。
//
// 为什么执行放在 handlers 层而不是 op 层：op 包被 client 包引用，而检测要用的
// modeltest 会反过来引用 client，把执行体放进 op 会形成 import cycle。
// 所以 op 只负责"排队与落库"这些纯数据操作，真正的探针发送留在这里。

const (
	// auditQueuePollInterval worker 空转轮询间隔。没有任务时只读一条带索引的
	// pending 行，代价可忽略，不需要引入信号量复杂度。
	auditQueuePollInterval = 3 * time.Second
)

var auditQueueStarted bool

// StartModelAuditQueueWorker 在服务启动时拉起检测队列 worker。可重复调用。
func StartModelAuditQueueWorker() {
	if auditQueueStarted {
		return
	}
	auditQueueStarted = true
	safe.SafeGo("model-audit-queue", func() {
		auditQueueLoop(context.Background())
	})
}

func auditQueueLoop(ctx context.Context) {
	log.Debugf("model audit queue started")
	ticker := time.NewTicker(auditQueuePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Debugf("model audit queue stopped")
			return
		case <-ticker.C:
			auditQueueTick(ctx)
		}
	}
}

// auditQueueTick 取一个待处理结果跑掉。一次只跑一个模型，跑完立刻看下一个。
//
// 串行是刻意的：检测会真的打上游并计费，并发跑一批会让同一渠道瞬间承受成倍
// 请求，既容易撞上游限流把结论污染成"失败"，也让费用不可预测。
func auditQueueTick(ctx context.Context) {
	var res model.ModelAuditResult
	// 只挑「到点该跑」的：pending 且（没有下次重试时间 或 时间已过）。
	//
	// 加 next_retry_at 这一条，是为了让失败重试退避真正生效：否则 worker 每 3 秒
	// 就会把刚失败的那条重新捡起来，退避形同虚设，还会把上游打得更狠。
	now := time.Now()
	err := db.GetDB().WithContext(ctx).
		Where("status = ?", model.AuditResultPending).
		Where("next_retry_at IS NULL OR next_retry_at <= ?", now).
		Order("id ASC").First(&res).Error
	if err != nil {
		return // 没有待办，或读取失败（下轮再试）
	}
	runOneAuditResult(ctx, &res)
}

// runOneAuditResult 执行单个模型的检测并把结论落库。绝不 panic 外泄。
func runOneAuditResult(ctx context.Context, res *model.ModelAuditResult) {
	// 先抢占成 running，避免进程重启后同一行被反复拾取。
	claim := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
		Update("status", model.AuditResultRunning)
	if claim.Error != nil || claim.RowsAffected == 0 {
		return // 别人已经抢走
	}

	started := time.Now()
	attempt := res.Attempt + 1

	// 回写日志时带上真实的触发来源，而不是一律写 manual。
	// trigger 记在 job 上（result 没这个字段），查一次；查不到就退回 manual。
	auditTrigger := "manual"
	{
		var job model.ModelAuditJob
		if err := db.GetDB().WithContext(ctx).Select("trigger").
			Where("id = ?", res.JobID).First(&job).Error; err == nil &&
			strings.TrimSpace(job.Trigger) != "" {
			auditTrigger = job.Trigger
		}
	}

	finish := func(status, verdict string, score int, findingN int, reportJSON, errMsg string) {
		now := time.Now()
		db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
			Where("id = ?", res.ID).
			Updates(map[string]any{
				"status":        status,
				"verdict":       verdict,
				"score":         score,
				"finding_n":     findingN,
				"report":        reportJSON,
				"error_msg":     errMsg,
				"attempt":       attempt,
				"next_retry_at": nil,
				"duration_ms":   now.Sub(started).Milliseconds(),
				"finished_at":   now,
			})
		op.ModelAuditJobProgress(ctx, res.JobID)

		// 结论回写触发它的那行日志。
		//
		// 以前这里只更新 queue 表（model_audit_results），主表 relay_logs 的
		// model_audit_* 列完全不动。而日志页的徽标只读本行字段——两边各写各的，
		// 结果就是"检测跑完了、结论也存了，日志页却永远显示没检测过"。
		// 这不是显示 bug，是结论有两个落点而只落了一个。
		//
		// 只在拿到真实结论（done）时回写：failed/retry 属于"没测出结果"，
		// 拿它去覆盖日志会让人以为这行被验过了。RelayLogMarkAudited 自己也是
		// 先到先得（verdict 为空才写），重复检测不会互相覆盖。
		if res.LogID > 0 && status == model.AuditResultDone {
			if err := op.RelayLogMarkAudited(ctx, res.LogID, verdict, score, findingN, auditTrigger, reportJSON); err != nil {
				// 回写失败不该影响检测本身的结论，记一条警告即可。
				log.Warnf("model audit backfill for log %d failed: %v", res.LogID, err)
			}
		}
	}

	// failOrRetry 记录一次失败，并决定"再试一次"还是"认输"。
	//
	// 这是本文件最该解释清楚的地方：以前任何失败都直接标 failed 收工，于是
	//   · 上游抖一下 → 这条模型就被判成"检测失败"，用户以为模型有问题；
	//   · 任务跑完时失败项就静静躺在那儿，没有任何人重试；
	//   · 任务进度停在 100% 但有效结论少一半，看起来"跑完了"，其实没测到。
	// 现在：失败先退避重试，试满 AuditMaxAttempts 仍未成才标 failed。
	//
	// 注意 finish 里把 next_retry_at 置空 —— 成功/终态不该再被调度器捡起来。
	failOrRetry := func(errMsg string) {
		if attempt < op.AuditMaxAttempts {
			next := time.Now().Add(op.AuditRetryBackoff(attempt))
			db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
				Where("id = ?", res.ID).
				Updates(map[string]any{
					"status":        model.AuditResultPending, // 回到待跑，等退避到点
					"attempt":       attempt,
					"next_retry_at": next,
					"error_msg":     errMsg,
					"finished_at":   nil,
				})
			// 进度不动：这条还没结束，只是稍后再试。让用户看到"还在跑"比
			// 让它闪一下"失败"再变回"待跑"更不容易误解。
			return
		}
		finish(model.AuditResultFailed, "", 0, 0, "", errMsg)
	}

	channel, err := op.ChannelGet(res.ChannelID, ctx)
	if err != nil {
		// 渠道不存在是**确定性**失败，重试没有意义——直接终态。
		finish(model.AuditResultFailed, "", 0, 0, "", "渠道已不存在")
		return
	}

	upstreamModel := res.Model
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	endpoint := auditEndpointFor(*channel)

	sender, err := modeltest.NewProbeSender(*channel, upstreamModel, endpoint)
	if err != nil {
		// 建 sender 失败多半是渠道配置缺失（密钥空、地址空），也是确定性失败。
		finish(model.AuditResultFailed, "", 0, 0, "", err.Error())
		return
	}

	// 与手动同步入口共用同一把单飞闸门，防止两处同时打同一 (渠道, 上游模型)。
	release, ok := acquireModelAuditSlot(channel.ID, upstreamModel)
	if !ok {
		// 单飞被占：**不是失败，是排队**。放回 pending 稍后再试，
		// 否则同一模型在手动检测与队列之间会互相把对方挤成 skipped。
		db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
			Where("id = ?", res.ID).
			Updates(map[string]any{
				"status":        model.AuditResultPending,
				"next_retry_at": time.Now().Add(op.AuditRetryBackoff(1)),
				"error_msg":     "同一渠道与模型的检测正在进行中，稍后自动重试",
			})
		return
	}
	defer release()

	timeout := op.AuditDefaultTimeoutSeconds
	var job model.ModelAuditJob
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.JobID).First(&job).Error; err == nil {
		if job.TimeoutSeconds > 0 {
			timeout = job.TimeoutSeconds
		}
	}
	if timeout > op.AuditMaxTimeoutSeconds {
		timeout = op.AuditMaxTimeoutSeconds
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	ids := parseAuditProbesOrEmpty(job.Probes)
	results := behavior.Run(runCtx, ids, sender, upstreamModel)

	// 关键判断：**一个探针都没成功，就不算"检测完成"**。
	//
	// 以前这里无条件写 done。后果是：上游超时/鉴权错/限流时，所有探针都失败，
	// 报告照样生成、照样标 done、verdict 落成 unknown —— 于是"没测到"和"测了没
	// 发现问题"在库里长得一模一样，用户看到的是一条"已完成"，实际什么都没验。
	// 这正是"执行未完成却不重试"最隐蔽的一种：它不是崩了，是**假装跑完了**。
	//
	// 现在把这两种结果分开：全挂 → 走 failOrRetry（退避重试，试满才判 failed）；
	// 至少一个探针拿到真实响应 → 才算 done，unknown 才是真的 unknown。
	usable := 0
	var firstErr string
	for _, r := range results {
		if r.Err == nil && r.OK {
			usable++
			continue
		}
		if firstErr == "" && r.Err != nil {
			firstErr = r.Err.Error()
		}
	}
	if usable == 0 {
		if firstErr == "" {
			firstErr = "所有探针均未获得有效响应"
		}
		failOrRetry(firstErr)
		return
	}

	report := behavior.ToView(behavior.BuildReport(results, res.Model))

	reportJSON := ""
	if b, err := json.Marshal(report); err == nil {
		reportJSON = string(b)
	}

	finish(model.AuditResultDone, string(report.Verdict), report.Score, len(report.Findings), reportJSON, "")
}

// parseAuditProbesOrEmpty 把逗号分隔的探针串还原成 ID 列表；空/无效则用默认集合。
func parseAuditProbesOrEmpty(s string) []behavior.ProbeID {
	s = strings.TrimSpace(s)
	if s == "" {
		return behavior.DefaultProbes()
	}
	out := make([]behavior.ProbeID, 0, 8)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id := behavior.ProbeID(p)
		if behavior.IsKnownProbe(id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return behavior.DefaultProbes()
	}
	return out
}
