package handlers

import (
	"context"
	"encoding/json"
	"errors"
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

	// auditReclaimInterval 回收过期租约的扫描间隔。
	//
	// 单独一个节拍而不是每 3 秒扫一次：回收是为了救"上次跑它的进程已经不在了"的行，
	// 早 27 秒发现它没有任何价值，却会白白多打一次库。
	auditReclaimInterval = 30 * time.Second
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
	reclaim := time.NewTicker(auditReclaimInterval)
	defer reclaim.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Debugf("model audit queue stopped")
			return
		case <-ticker.C:
			auditQueueTick(ctx)
		case <-reclaim.C:
			// 捡回"上次跑它的进程已经不在了"的行。没有这一步，一次重启就会让
			// 那行永远停在 running，任务永远显示"进行中"，用户看到的是"不出结果"。
			if n := op.AuditReclaimExpired(ctx, 50); n > 0 {
				log.Warnf("model audit reclaimed %d expired running result(s)", n)
			}
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
	// 先抢占成 running，并**同时写下租约到期时间**。
	//
	// 以前这里只改 status，于是进程一重启那行就永远停在 running：没有续租、
	// 没有回收，任务进度永远差一格，页面永远显示"进行中"。现在 running 行
	// 带着"到点可以被回收"的时间戳，进程没了就由 AuditReclaimExpired 捡回来。
	claim := db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
		Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
		Updates(map[string]any{
			"status":        model.AuditResultRunning,
			"next_retry_at": op.AuditLeaseUntil(time.Now()),
		})
	if claim.Error != nil || claim.RowsAffected == 0 {
		return // 别人已经抢走
	}

	// 跑的过程中持续续租：检测一次最长可以跑几分钟，没有续租就会被自己人回收。
	leaseCtx, stopLease := context.WithCancel(ctx)
	defer stopLease()
	safe.SafeGo("model-audit-lease", func() {
		t := time.NewTicker(op.AuditRunHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-t.C:
				if !op.AuditRenewLease(leaseCtx, res.ID, op.AuditLeaseUntil(time.Now())) {
					// 这行已经不属于我们（被回收/被取消），别再续了，让它自然过期。
					return
				}
			}
		}
	})

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
	// reportJSON 参数是给终态用的：既然探针已经跑过、每条都有失败原因和耗时，
	// 那就把这份结构性证据一起留下来。只写一句"没跑成"等于把用户已经付过的
	// 调用费用和排错线索一起扔掉。
	//
	// 注意 finish 里把 next_retry_at 置空 —— 成功/终态不该再被调度器捡起来。
	failOrRetry := func(errMsg, reportJSON string) {
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
		finish(model.AuditResultFailed, "", 0, 0, reportJSON, errMsg)
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
	endpoint := op.AuditEndpointForChannel(*channel)

	sender, err := modeltest.NewProbeSender(*channel, upstreamModel, endpoint)
	if err != nil {
		// 密钥全在冷却里不是环境配错，是**暂时状态**：等它恢复就能跑。
		// 以前这类也直接终态 failed，结果同一个渠道在冷却期里连点几次都是
		// 「没跑成」，看着像渠道坏了，其实只是要等一会儿。
		if errors.Is(err, modeltest.ErrProbeKeysCoolingDown) {
			failOrRetry(err.Error(), "")
			return
		}
		// 其余多半是渠道配置缺失（密钥空、地址空），确定性失败，重试没有意义。
		finish(model.AuditResultFailed, "", 0, 0, "", err.Error())
		return
	}

	// 与手动同步入口共用同一把单飞闸门，防止两处同时打同一 (渠道, 上游模型)。
	release, ok := acquireModelAuditSlot(channel.ID, upstreamModel)
	if !ok {
		// 单飞被占：**不是失败，是排队**。放回 pending 稍后再试，
		// 否则同一模型在手动检测与队列之间会互相把对方挤成 skipped。
		//
		// 但排队必须有收敛上限：让位次数超过 modelAuditYieldCap 就直接终止。
		// 让位本身不发请求、不涨 attempt，可一旦持有者卡死，无限让位就等于无限空转——
		// 用户看到的是"一直排队、永远没结果"。到点说清楚，让他自己决定要不要重检。
		if yields := noteModelAuditYield(res.ID); yields > modelAuditYieldCap {
			clearModelAuditYield(res.ID)
			finish(model.AuditResultFailed, "", 0, 0, "",
				"同一渠道同一模型一直有另一次检测在跑，等了约 10 分钟没轮到；本次让位放弃，请稍后手动重检")
			return
		}
		db.GetDB().WithContext(ctx).Model(&model.ModelAuditResult{}).
			Where("id = ?", res.ID).
			Updates(map[string]any{
				"status":        model.AuditResultPending,
				"next_retry_at": time.Now().Add(op.AuditRetryBackoff(1)),
				"error_msg":     "同一渠道与模型的检测正在进行中，稍后自动重试",
			})
		return
	}
	clearModelAuditYield(res.ID)
	defer release()

	var job model.ModelAuditJob
	_ = db.GetDB().WithContext(ctx).Where("id = ?", res.JobID).First(&job).Error
	ids := parseAuditProbesOrEmpty(job.Probes)
	runCtx, cancel := context.WithTimeout(ctx, auditRunTimeout(job.TimeoutSeconds, ids))
	defer cancel()

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

	// 先把报告建出来，**无论成功失败**。
	//
	// 以前只在成功分支建报告，全挂时 report 写空串——于是"没跑成"只剩下一个
	// 错误字符串，用户看不到是哪一项、为什么、花了多久。可这些探针**已经真的
	// 打过上游、已经计过费**了，把它们的结果丢掉等于白花钱。
	// behavior.BuildReport 本身会把每个 Err 收进 report.Errors，Results 里也逐条
	// 保留 Error 字段，所以失败报告天然就有内容。
	report := behavior.ToView(behavior.BuildReport(results, res.Model))

	reportJSON := ""
	if b, err := json.Marshal(report); err == nil {
		reportJSON = string(b)
	}

	if usable == 0 {
		if firstErr == "" {
			firstErr = "所有探针均未获得有效响应"
		}
		failOrRetry(firstErr, reportJSON)
		return
	}

	finish(model.AuditResultDone, string(report.Verdict), report.Score, len(report.Findings), reportJSON, "")
}

// auditRunTimeout 一条结果的整单超时。
//
// 任务行里存的是入队时按 op.AuditTimeoutFor 放大过的值；这里再按同一口径封顶
// （默认上限 300 秒，含分词器等大请求量探针时按计划请求数放大，最多 30 分钟），
// 防止库里的脏值或旧任务把单 worker 堵死。没有存值时用缺省值并同样放大。
func auditRunTimeout(stored int, ids []behavior.ProbeID) time.Duration {
	probes := make([]string, len(ids))
	for i, id := range ids {
		probes[i] = string(id)
	}
	timeout := stored
	if timeout <= 0 {
		timeout = op.AuditTimeoutFor(probes, op.AuditDefaultTimeoutSeconds)
	}
	if limit := op.AuditTimeoutFor(probes, op.AuditMaxTimeoutSeconds); timeout > limit {
		timeout = limit
	}
	return time.Duration(timeout) * time.Second
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
