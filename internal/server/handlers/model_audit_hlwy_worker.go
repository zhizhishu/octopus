package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// hlwy 基准对照的**执行侧**。
//
// 它和普通检测共用同一个队列、同一把单飞闸门、同一个 worker 循环，但执行体完全是
// 另一套：普通检测跑「一组探针、产出一份风险报告」，hlwy 对照跑「同一个题目 N 次、
// 数出每个数字出现多少次、再和基准比」。前者关心"模型有没有乱来"，后者关心
// "这个渠道背后的分布像不像某个型号"，两边的记账、终止条件、可比性都没有交集，
// 所以这里不试图抽象成一个通用 runner，而是并列一份。
//
// 三条不可让步的原则（都在下面代码里体现）：
//  1. 先记账再发包：预留一格 → 落库 → 才发请求。宁可留下"可能花过但没证据"的
//     槽位，也不能让已经付过钱的调用从账上消失；
//  2. 不自动重跑：任何中断都收敛成终态并保留已采到的数字。重跑 = 再花一遍钱，
//     必须由人决定；
//  3. 证据不自洽就不给分：宁可交一份"不可评分 + 为什么"的报告，也不交一份
//     "条件不一致但排名第一"的表格。

const (
	// hlwyFinishTimeout 收尾写入的独立超时。
	//
	// 收尾用的 ctx 会脱离上层取消（用户点取消、worker 关闭都要照样把账记完），
	// 但脱离取消就必须自带超时，否则一次卡住的写会让整个 worker 停摆。
	hlwyFinishTimeout = 10 * time.Second

	// hlwyConsecutiveFailures 连续多少格拿不到可用响应就停止本轮。
	//
	// 上游挂了的时候继续跑完 200 格只是把 200 次失败买回来：没有数字，没有证据，
	// 只有账单。停在这里，报告里如实写"连续失败"与已采到的部分。
	hlwyConsecutiveFailures = 3
)

// hlwyFatalStatuses 是「再试也不会好」的上游状态码：请求本身被拒收。
//
// 401/403 是鉴权、404 是模型没了、400/422 是请求不被接受、429 是配额/限流。
// 这些情况下继续把剩下的一百多格发出去，得到的只会是同样一句话重复一百遍。
var hlwyFatalStatuses = map[int]struct{}{
	400: {}, 401: {}, 403: {}, 404: {}, 422: {}, 429: {},
}

// hlwyWithConditionsObserver 是本 worker 与核心采样器之间的**唯一**接缝。
//
// 条件观测必须由真正发出请求的那一层做（只有它看得到最终出站请求体），因此这里
// 直接接到 modeltest 的实现上。核心没接上时观测一律「未知」，报告如实写「可对照
// 程度未知」，绝不去猜 temperature/thinking——猜出来的「一致」比诚实的「未知」
// 危险得多。测试可以把它换成自己的实现。
var hlwyWithConditionsObserver = modeltest.WithProbeConditionsObserver

// hlwyNewSender 建一个采样 sender。测试用它注入假 sender（不打上游）。
var hlwyNewSender = func(channel model.Channel, upstreamModel, endpoint string) (modelverify.Sender, error) {
	return modeltest.NewProbeSender(channel, upstreamModel, endpoint)
}

// hlwyObserveConditions 给 ctx 装上条件观察器；核心未落地时原样返回。
func hlwyObserveConditions(ctx context.Context, fn func(numberdist.Conditions)) context.Context {
	if hlwyWithConditionsObserver == nil {
		return ctx
	}
	return hlwyWithConditionsObserver(ctx, fn)
}

// hlwyDispatchJob 读任务行，判断这条结果是不是 hlwy 对照。
//
// 必须在旧路径抢锁**之前**分流：旧路径会把结果行直接改成 running 并写租约，
// 那之后再想走 hlwy 的记账就晚了（attempt/租约/report 已经被旧路径占住）。
//
// 读任务行失败就返回错误，调用方这一轮**谁都不跑**：退回旧路径等于把一条 hlwy 的
// 结算交给旧执行体，而它不知道 planned/counts 那套账，跑出来的报告永远自相矛盾。
func hlwyDispatchJob(ctx context.Context, res *model.ModelAuditResult) (model.ModelAuditJob, bool, error) {
	if res.JobID <= 0 {
		return model.ModelAuditJob{}, false, nil
	}
	var job model.ModelAuditJob
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.JobID).First(&job).Error; err != nil {
		return model.ModelAuditJob{}, false, fmt.Errorf("检测结果 #%d 的任务 #%d 读不到: %w", res.ID, res.JobID, err)
	}
	if strings.TrimSpace(job.Kind) != model.AuditKindHlwy {
		// 不是 hlwy：把已经读到的任务行原样交回去（旧路径要用 Trigger），
		// 但**不要**再多查一次。
		return job, false, nil
	}
	return job, true, nil
}

// hlwyRunState 是本轮采样的收尾意图。
//
// 它不是"结果"，只是"我们打算怎么结束"：真正的终态由 op.HlwyFinish 与账目、
// 任务状态对账后决定（取消赢过完成、账没落定就不能算完成）。
type hlwyRunState struct {
	execution  string
	stopReason string
	errMsg     string
}

// runHlwyResult 执行一条 hlwy 对照结果。绝不 panic 外泄。
func runHlwyResult(ctx context.Context, res *model.ModelAuditResult, job model.ModelAuditJob) {
	// detached 给所有"必须记完账"的写入用：脱离上层取消（否则用户一点取消，
	// 已经花掉的那一格就永远记不上了），但自带超时。
	detached := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), hlwyFinishTimeout)
	}
	failWithoutRun := func(msg string) {
		wctx, cancel := detached()
		defer cancel()
		if err := op.HlwyFailWithoutRun(wctx, res.ID, msg, time.Now()); err != nil {
			log.Warnf("hlwy result %d fail-without-run write failed: %v", res.ID, err)
		}
	}

	opts, err := model.ParseHlwyOptions(job.OptionsJSON)
	if err != nil {
		// 只落类别词：库里存的可能是任意历史 JSON，它的内容不该变成用户看到的
		// 故障原因，也不该出现在结果行上。
		log.Warnf("hlwy result %d options unusable: %v", res.ID, err)
		failWithoutRun("对照参数不可用")
		return
	}

	channel, err := op.ChannelGet(res.ChannelID, ctx)
	if err != nil {
		// 渠道没了是确定性失败：重试没有意义，也不该重试。
		failWithoutRun("渠道已不存在")
		return
	}
	upstreamModel := res.Model
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	endpoint := op.AuditEndpointForChannel(*channel)

	sender, err := hlwyNewSender(*channel, upstreamModel, endpoint)
	if err != nil {
		// 密钥全部在冷却里是**暂时状态**（等它恢复就能跑），与配置缺失分开处理：
		// 前者放回队列让位等待，后者直接终态。两边的落库文案都是固定类别词，
		// 不透传错误原文（它可能带 URL 或上游报文）。
		if errors.Is(err, modeltest.ErrProbeKeysCoolingDown) {
			hlwyYieldOrGiveUp(ctx, res, "渠道密钥全部在冷却中，稍后自动重试")
			return
		}
		log.Warnf("hlwy result %d sender unavailable: %v", res.ID, err)
		failWithoutRun("无法建立采样通道（请检查渠道配置与密钥）")
		return
	}

	// 与手动检测、旧队列共用同一把单飞闸门：同一个 (渠道, 上游模型) 同时只允许
	// 一路在打上游。hlwy 对照一次要塞 N 个请求进去，两路并发会把彼此的数字
	// 都污染（也让限流更容易触发）。
	release, ok := acquireModelAuditSlot(channel.ID, upstreamModel)
	if !ok {
		hlwyYieldOrGiveUp(ctx, res, "同一渠道与模型的检测正在进行中，稍后自动重试")
		return
	}
	clearModelAuditYield(res.ID)
	defer release()

	claim, err := op.HlwyClaimResult(ctx, op.HlwyClaimInput{
		ResultID:      res.ID,
		UpstreamModel: upstreamModel,
		Conditions:    op.HlwyUnknownConditions(probeProtocolName(channel.Type)),
	}, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, op.ErrHlwyJobCanceled), errors.Is(err, op.ErrHlwyNotPending):
			// 已取消 / 已被别的执行者领走：安静退出，不写任何东西。
		default:
			// 领取失败（库抖动之类）不改状态：行还是 pending，下一轮自己会再捡到。
			log.Warnf("hlwy result %d claim failed: %v", res.ID, err)
		}
		return
	}

	hlwyRunLoop(ctx, res, claim, opts, sender)
}

// hlwyYieldOrGiveUp 把这一行推后等待，让位次数超上限就放弃。
//
// 让位本身不发请求、不涨 attempt，所以它可以无限次发生；一旦持有者卡死，无限让位
// 就等于无限空转——用户看到的是"一直排队、永远没结果"。因此照旧用既有的让位计数
// 封顶（约 10 分钟），到点说清楚并放弃，让人自己决定要不要重检。
func hlwyYieldOrGiveUp(ctx context.Context, res *model.ModelAuditResult, reason string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hlwyFinishTimeout)
	defer cancel()
	if yields := noteModelAuditYield(res.ID); yields > modelAuditYieldCap {
		clearModelAuditYield(res.ID)
		msg := "同一渠道同一模型一直有另一次检测在跑，等了约 10 分钟没轮到；本次让位放弃，请稍后手动重检"
		if err := op.HlwyFailWithoutRun(wctx, res.ID, msg, time.Now()); err != nil {
			log.Warnf("hlwy result %d give-up write failed: %v", res.ID, err)
		}
		return
	}
	// 只推迟，不改状态：已经结束（或已被别人领走）的行不能被一次让位复活成待跑，
	// 那会在下一轮再花一遍钱。
	if err := op.HlwyDeferPending(wctx, res.ID, reason, op.AuditRetryBackoff(1), time.Now()); err != nil {
		if !errors.Is(err, op.ErrHlwyJobCanceled) {
			log.Warnf("hlwy result %d defer failed: %v", res.ID, err)
		}
	}
}

// hlwyRunLoop 是「预留 → 发包 → 落定」的循环。
//
// claim.Token 每落库一次就换成新的：下一次写都以它做 CAS。一旦换手失败（说明有
// 别的写法绕过任务行锁改了报告），下一条语句会以 0 行受影响把我们拦住，而不是
// 默默把别人的写入盖掉。
func hlwyRunLoop(ctx context.Context, res *model.ModelAuditResult, claim op.HlwyClaim,
	opts model.HlwyOptions, sender modelverify.Sender) {
	// 总时长由服务器按采样次数算定（min(300+10N, 1800)），不采信库里的值：
	// 旧任务或脏数据不得把单 worker 堵死。
	runCtx, cancelRun := context.WithTimeout(ctx,
		time.Duration(op.HlwyRunTimeout(opts.Samples))*time.Second)
	defer cancelRun()

	// 续租：节拍与旧路径一致（30 秒），但绑 attempt —— 租约一旦不属于本次运行
	// 就立刻停手，而且**不收尾**：收尾写同样要求持有租约，过期之后写它只会把
	// 回收路径刚写的终态盖回去。
	//
	// 心跳只拿一份副本（只读 resultID/jobID/attempt）：主循环每落定一格都会改写
	// claim.Token，把同一个 struct 交给两个协程读写就是数据竞争。
	leaseLost := make(chan struct{})
	leaseCtx, stopLease := context.WithCancel(runCtx)
	defer stopLease()
	leaseClaim := op.HlwyClaim{ResultID: claim.ResultID, JobID: claim.JobID, Attempt: claim.Attempt}
	leaseDone := make(chan struct{})
	safe.SafeGo("model-audit-hlwy-lease", func() {
		defer close(leaseDone)
		t := time.NewTicker(op.AuditRunHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-t.C:
				if !op.HlwyRenewOwnership(leaseCtx, leaseClaim, op.AuditLeaseUntil(time.Now())) {
					close(leaseLost)
					cancelRun()
					return
				}
			}
		}
	})

	state := hlwyRunState{execution: model.HlwyExecutionComplete}
	conds := claim.Conditions
	failures := 0

	for {
		if runErr := runCtx.Err(); runErr != nil {
			select {
			case <-leaseLost:
				return // 租约已不属于我们，交给回收路径收尾
			default:
			}
			// 整轮超时与「上层把 worker 停了」是两件事：前者是采样跑不完，
			// 后者是进程/服务正在退出。写成一个原因，日志里就再也分不清
			// 「模型慢」和「服务重启」了。
			if errors.Is(runErr, context.DeadlineExceeded) {
				state.execution = model.HlwyExecutionTimedOut
				if state.stopReason == "" {
					state.stopReason = "run_timeout"
				}
				if state.errMsg == "" {
					state.errMsg = "本轮采样超过总时长上限，已停止（已采到的样本保留）"
				}
			} else {
				state.execution = model.HlwyExecutionInterrupted
				if state.stopReason == "" {
					state.stopReason = "interrupted"
				}
				if state.errMsg == "" {
					state.errMsg = "执行被中断（服务停止）；已采到的样本保留，为避免重复计费不自动重跑"
				}
			}
			break
		}

		slot, err := op.HlwyReserveSlot(runCtx, claim, time.Now())
		if err != nil {
			switch {
			case errors.Is(err, op.ErrHlwySlotsExhausted):
				// 计划内的槽位全部用完：正常收尾。
			case errors.Is(err, op.ErrHlwyJobCanceled):
				state.execution = model.HlwyExecutionCanceled
				state.stopReason = "canceled"
			case errors.Is(err, op.ErrHlwyOwnershipLost), errors.Is(err, op.ErrHlwyNotPending):
				return
			default:
				log.Warnf("hlwy result %d reserve failed: %v", res.ID, err)
				state.execution = model.HlwyExecutionFailed
				state.stopReason = "reserve_failed"
				state.errMsg = "采样记账写入失败，已停止本轮采样"
			}
			break
		}
		claim.Token = slot.Token

		out, observed, hasObs, fatalStop := hlwySampleOnce(runCtx, sender)

		// 条件与题目只信观测到的硬证据：拿到哈希就比，拿到参数就记，只在**两边
		// 都已知**时判漂移。观测到的条件一律照实写进报告——题目/参数变了更不能
		// 假装还是旧值，那等于把不可比的证据洗成可比的。
		stopAfterSettle := fatalStop
		if hasObs {
			snapshot := op.HlwyMergeConditions(conds, observed)
			if op.HlwyPromptMismatch(model.HlwyPromptSHA256(), observed) {
				stopAfterSettle = op.HlwyStopPromptMismatch
				state.errMsg = "实际发出的题目与本轮预期不一致，已停止采样（这一轮的数字不再对照）"
			} else if drift := op.HlwyConditionsDrift(conds, observed); drift != "" {
				stopAfterSettle = op.HlwyStopConditionsChanged
				state.errMsg = "同一轮里采样条件发生了变化，已停止采样（这一轮的数字不再对照）"
			} else {
				conds = snapshot
			}
			// 无论是否因此停手，这一格观测到的东西照实落库。
			if model.ValidateHlwyConditions(snapshot) == nil {
				out.Conditions = &snapshot
			}
		}

		// 这一格的钱可能已经花出去了，因此落定用脱离取消的 ctx：用户刚点取消、
		// 或总时长刚好到，都不能把这一格的证据丢掉。
		// 停止原因跟着这一格一起落库（见 HlwySettleSlot）：即使进程在收尾前消失，
		// 「本轮不可比」也已经写进报告了。
		out.StopReason = stopAfterSettle
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), hlwyFinishTimeout)
		nextToken, serr := op.HlwySettleSlot(wctx, claim, slot, out, time.Now())
		wcancel()
		if serr != nil {
			if errors.Is(serr, op.ErrHlwyOwnershipLost) {
				return
			}
			// 记账写不进去就不能再继续发包：再发下去就是花完钱还不记账。
			log.Warnf("hlwy result %d settle slot %d failed: %v", res.ID, slot.Index, serr)
			state.execution = model.HlwyExecutionFailed
			if state.stopReason == "" {
				state.stopReason = "settle_failed"
			}
			if state.errMsg == "" {
				state.errMsg = "采样记账写入失败，已停止本轮采样"
			}
			break
		}
		claim.Token = nextToken

		if stopAfterSettle != "" {
			state.execution = model.HlwyExecutionFailed
			state.stopReason = stopAfterSettle
			if state.errMsg == "" {
				state.errMsg = "上游拒收（" + stopAfterSettle + "），已停止本轮采样"
			}
			break
		}

		if out.Result == op.HlwySlotFailed {
			failures++
			if failures >= hlwyConsecutiveFailures {
				state.execution = model.HlwyExecutionFailed
				state.stopReason = "consecutive_failures"
				state.errMsg = "连续多格没拿到可用响应，已停止本轮采样（已采到的样本保留）"
				break
			}
			continue
		}
		failures = 0
	}

	// 心跳先停、并且等它真的结束：续租与收尾都在同一把任务行锁上，并发拿锁只会
	// 互相等（SQLite 上就是白等到超时）；而且收尾之后再推一次租约毫无意义。
	stopLease()
	<-leaseDone

	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), hlwyFinishTimeout)
	defer wcancel()
	if err := op.HlwyFinish(wctx, claim, op.HlwyFinishInput{
		Execution:  state.execution,
		StopReason: state.stopReason,
		ErrorMsg:   state.errMsg,
	}, time.Now()); err != nil {
		log.Warnf("hlwy result %d finish failed: %v", res.ID, err)
	}
}

// 一次采样失败的三种算账类别。
const (
	// hlwyClassInvalidAnswer 上游回了话，但没给出可解析的答案（2xx 但内容空）。
	hlwyClassInvalidAnswer = "invalid_answer"
	// hlwyClassRejected 请求被拒收：再发剩下的槽位只会得到同一句话。
	hlwyClassRejected = "rejected"
	// hlwyClassUnreachable 网络/超时/5xx：这一格没拿到可用响应。
	hlwyClassUnreachable = "unreachable"
)

// hlwyClassifyStatus 把上游错误里的状态码翻译成「怎么算账」与「落库类别词」。
//
// 三种类别分开是因为后续动作完全不同：无效回答是证据（继续采）、4xx 是确定性拒收
// （立刻停）、其余才是「没跑成」（交给连续失败计数）。全部算成网络失败，会把一次
// 「模型回了空内容」当成三格网络错误而提前停掉整轮。
func hlwyClassifyStatus(status int) (string, string) {
	switch {
	case status >= 200 && status < 300:
		return hlwyClassInvalidAnswer, ""
	case status >= 400 && status < 500:
		if _, known := hlwyFatalStatuses[status]; known {
			return hlwyClassRejected, fmt.Sprintf("http_%d", status)
		}
		return hlwyClassRejected, op.HlwyStopRequestRejected
	default:
		return hlwyClassUnreachable, ""
	}
}

// hlwySampleOnce 发一格采样，并尽量诱出“这次到底发的是什么”的观测。
//
// 四个返回值：结果、观测到的条件快照、是否观测到、需要致命停止的原因（空=继续）。
// 上游报错一律**不向上传递原文**：它可能带着响应体、URL 或密钥。落库的只有
// 类别词（如 http_401）和我们自己拼的文案。
func hlwySampleOnce(ctx context.Context, sender modelverify.Sender) (op.HlwySlotOutcome, numberdist.Conditions, bool, string) {
	var (
		observed numberdist.Conditions
		hasObs   bool
	)
	sampleCtx, cancel := context.WithTimeout(ctx, op.HlwySampleTimeout)
	defer cancel()
	sampleCtx = hlwyObserveConditions(sampleCtx, func(c numberdist.Conditions) {
		observed = c
		hasObs = true
	})

	resp, err := sender.Ask(sampleCtx, modelverify.Ask{Prompt: numberdist.Prompt})
	if err != nil {
		var status *modelverify.UpstreamStatusError
		if errors.As(err, &status) {
			class, code := hlwyClassifyStatus(status.Status)
			if class == hlwyClassInvalidAnswer {
				// 上游确实回了话，只是没给出答案：这是无效回答，不是「没跑成」。
				return op.HlwySlotOutcome{Result: op.HlwySlotInvalid}, observed, hasObs, ""
			}
			if class == hlwyClassRejected {
				return op.HlwySlotOutcome{Result: op.HlwySlotFailed, StopReason: code}, observed, hasObs, code
			}
		}
		// 超时/连接被断/5xx 都算“这一格没拿到可用响应”，
		// 是否停手由连续失败计数决定。
		return op.HlwySlotOutcome{Result: op.HlwySlotFailed}, observed, hasObs, ""
	}

	parsed := numberdist.Parse(resp.Content)
	out := op.HlwySlotOutcome{
		Result:    op.HlwySlotInvalid,
		Truncated: hlwyTruncated(resp.FinishReason),
	}
	if parsed.Valid {
		out.Result = op.HlwySlotValid
		out.Number = parsed.Number
		out.StrictInteger = parsed.StrictInteger
	}
	return out, observed, hasObs, ""
}

// hlwyTruncated 判断上游是不是因为长度上限被截断。
//
// 截断不是错误而是证据：它说明这个渠道藏着思考内容或者被限了输出长度，
// 采到的数字也就没那么可信。记下来，不丢。
func hlwyTruncated(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "max_tokens":
		return true
	default:
		return false
	}
}
