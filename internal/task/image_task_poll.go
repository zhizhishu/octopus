package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// 图片异步任务执行器：把 image_tasks 表里还没出结果的行推到终态。
//
// 与 video_poll.go 的关系：那边是"提交过的任务在上游慢慢跑，我们隔一会儿问一次状态"，
// 这边是"提交时什么都没发给上游，真正的调用由这里发出"。所以本文件做的事更接近
// relay.ImagesHandler 的那一次转发，只是没有下游 writer 可写——结果只能落到行上。
//
// 它不碰同步图片路径的任何行为：同步版一次调用照旧自己转发、自己写下游。
const (
	// imageTaskPollInterval 是两轮的间隔。
	//
	// 取 5 秒的依据：契约让客户端 Retry-After: 3 秒来问一次，如果执行器 30 秒才
	// 动一次（照抄视频的间隔），客户端会连续看到十几次 processing 才开始真正生成。
	// 5 秒让"提交 → 开始生成"的延迟落在客户端看来只是一两个轮询周期。
	imageTaskPollInterval = 5 * time.Second

	// imageTaskPollBatchLimit 是单轮最多推进多少条。积压时一轮不跑到天亮。
	imageTaskPollBatchLimit = 20

	// imageTaskPollExpiredLimit 是单轮最多收口多少条过期任务。
	imageTaskPollExpiredLimit = 50

	// imageTaskPersistTimeout 是"把已经拿到的上游结果落库"的独立时限。
	//
	// 【为什么不能复用本轮的 ctx】整轮预算到点时，ctx 已经取消。一次**已经成功**的
	// 上游调用如果因为 `context deadline exceeded` 写不进库，这一单就白花了上游的钱
	// （下一轮会再打一次），客户端也永远拿不到图。终态落库是纯本地 DB 操作，
	// 用一个脱离整轮预算的短时限把它做完，比"尊重大局预算"重要得多。
	imageTaskPersistTimeout = 10 * time.Second
)

// imageTaskRunRoundBudget 是一轮的整轮预算。超了就不再发起新的上游调用，
// 已经发出去的由各自的单条超时收口 —— 一轮拖不死下一轮。
//
// 改成 var 是为了让测试把它压到毫秒级（生产路径从不写它）。
var imageTaskRunRoundBudget = 2 * time.Minute

// imageTaskRunTaskTimeout 是**单条**上游调用的独立时限。
//
// 它与整轮预算是嵌套关系（子 ctx 不会越过父 ctx）。单条必须有独立时限的理由和
// videoTaskPollTaskTimeout 一样：一条"连得上、握手完、但对端一直不返回"的上游会
// 吃光整轮预算，后面所有任务都被 `ctx.Err() != nil` 跳过，而下一轮 ImageTaskListExecutable
// 仍按 id 升序从最老的那批开始 —— 同一个慢上游能持续饿死别人的任务。
//
// 取 180 秒：图片生成正常 20–60 秒，3 分钟已经宽了三倍，足够兜住"慢但能出图"的上游；
// 同时远小于 imageTaskTTL（24h），异常任务不会因此每次都被重跑。
var imageTaskRunTaskTimeout = 180 * time.Second

// imageTaskRunConcurrency 是同时在飞的上游请求上限。
//
// 【必须限】图片上游按 key 限速，开太多只会集体 429 —— 用户看到的是"全都失败"
// 而不是"慢一点"。4 是个既能吃满单 key 配额、又不会把上游打爆的值。
//
// 改成 var 是为了让测试能把它压到 1（验证并发上限本身）。
var imageTaskRunConcurrency = 4

var (
	imageTaskPollOnce   sync.Once
	imageTaskPollStarts atomic.Int32
)

// ImageTaskPollStart 启动图片异步任务执行器（进程内单例）。
//
// 【绝不许改成 init() 自注册】理由与 VideoTaskPollStart 逐字相同：测试二进制绝不该
// 有无人管生命周期的后台写库者（internal/task/init.go 顶部那段注释，CI 实翻过）。
// 本执行器每一轮都读库、写库、还会真花用户的钱（预扣/退款）——靠 init() 自己起来
// 等于让每个 go test 用例都可能撞上一个正在遍历临时库的写者。
//
// 所以只在生产启动路径上由 internal/task/init.go 显式调用。
func ImageTaskPollStart() {
	imageTaskPollOnce.Do(func() {
		imageTaskPollStarts.Add(1)
		safe.SafeGo("image-task-poll", func() {
			ticker := time.NewTicker(imageTaskPollInterval)
			defer ticker.Stop()
			for range ticker.C {
				runImageTaskPollOnce()
			}
		})
	})
}

// runImageTaskPollOnce 跑一轮：先收口过期任务，再执行还没出结果的任务。
//
// 顺序是刻意的：先判死过期的，再取待执行列表。反过来的话，本轮刚被标失败的行
// 已经不在 processing 里了，但一条**恰好在本轮过期**的任务会被先执行一次
// （白花一次上游调用），或者更糟：执行成功后又过期 —— 而客户端早已按 404 处理。
//
// 与 video_poll.go 的一个区别：那里要为「判死前先问一次上游」切出独立的 sweep 预算，
// 因为那一步每条都要发一次可能挂死的上游请求。这里的过期清扫**不发任何上游请求**，
// 只做 CAS + 退款（都是本地 DB 操作），所以它不可能吃掉整轮预算；一轮的预算上限
// 由 imageTaskRunRoundBudget + 单条超时 + 并发上限 + 批次上限 共同兜住。
func runImageTaskPollOnce() {
	// 库没起来（例如只跑纯函数用例的测试二进制）就直接不干活，而不是在 nil 连接上 panic。
	if db.GetDB() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), imageTaskRunRoundBudget)
	defer cancel()

	sweepExpiredImageTasks(ctx)

	tasks, err := op.ImageTaskListExecutable(ctx, time.Now().Unix(), imageTaskPollBatchLimit)
	if err != nil {
		log.Warnf("image task poll: list executable tasks failed: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	runImageTaskBatch(ctx, tasks)
}

// runImageTaskBatch 以 imageTaskRunConcurrency 为上限并发执行一批任务，并等这一批
// **全部**结束才返回。
//
// 为什么不做"fire and forget"：那会让同一个 taskID 在下一轮被 ImageTaskListExecutable
// 再捞一次（本表没有 updated_at 可以把它推到队尾），同一条任务于是被反复执行、
// 反复扣一次上游配额。等这一批结束 = 每一轮都是自洽的一小段，最多慢，不会重复花。
func runImageTaskBatch(ctx context.Context, tasks []model.ImageTask) {
	concurrency := imageTaskRunConcurrency
	if concurrency <= 0 {
		// 0 会让下面的 channel 永远写不进去（整轮死锁）。这是配置错误，
		// 退回默认值而不是把执行器卡死。
		concurrency = 4
	}
	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, task := range tasks {
		select {
		case <-ctx.Done():
			log.Warnf("image task poll: round budget reached, %d task(s) deferred to the next round", len(tasks))
			wg.Wait()
			return
		case slots <- struct{}{}:
		}
		wg.Add(1)
		task := task
		safe.SafeGo("image-task-run", func() {
			defer wg.Done()
			defer func() { <-slots }()
			executeOneImageTask(ctx, task)
		})
	}
	wg.Wait()
}

// executeOneImageTask 把一条任务推到终态。
//
// 三条出口都必须做两件事（契约硬要求）：推进状态、释放该任务的在途预留。
// 退款只在失败/过期时发生，且由 op.ImageTaskRefund 保证幂等。
func executeOneImageTask(ctx context.Context, task model.ImageTask) {
	// 单条独立时限（见 imageTaskRunTaskTimeout）。
	taskCtx, cancel := context.WithTimeout(ctx, imageTaskRunTaskTimeout)
	defer cancel()

	channel, err := op.ChannelGet(task.ChannelID, taskCtx)
	if err != nil {
		failImageTask(task, fmt.Sprintf("image channel %d is unavailable: %v", task.ChannelID, err))
		return
	}
	channelKey, found, enabled := imageTaskChannelKey(channel, task.ChannelKeyID)
	if !found {
		failImageTask(task, fmt.Sprintf("image channel key %d no longer exists on channel %d", task.ChannelKeyID, channel.ID))
		return
	}
	if !enabled {
		failImageTask(task, fmt.Sprintf("image channel key %d on channel %d is disabled", task.ChannelKeyID, channel.ID))
		return
	}

	statusCode, body, failReason, runErr := relay.ImageTaskRunUpstream(
		taskCtx, channel, channelKey, task.Endpoint, task.Model, []byte(task.RequestBody),
	)
	if runErr != nil {
		// 上游没给结论（网络/超时/读失败）。这类"问不出结果"不重试：
		// 任务已经离开客户端视野很久，重试只会在同一个坏渠道上反复花钱；
		// 判失败并退款，让用户重新发一次。
		failImageTask(task, imageTaskRedactFailure(fmt.Sprintf("image generation request failed: %v", runErr)))
		return
	}
	if failReason != "" {
		failImageTask(task, imageTaskRedactFailure(failReason))
		return
	}

	resultJSON, imageURL := relay.ImageTaskResultPayload(body)
	if resultJSON == "" && imageURL == "" {
		// 上游 2xx 但回来的东西里既没有图也没有可用的结果形状：判失败而不是
		// 记一个空的 completed —— 后者会让客户端拿到"成功但没有图"。
		failImageTask(task, fmt.Sprintf("upstream returned status %d without an image result", statusCode))
		return
	}

	now := time.Now().Unix()
	updates := map[string]any{
		"completed_at":  now,
		"result_json":   resultJSON,
		"error_message": "",
	}
	// 见 imageTaskPersistTimeout：结果必须落库，不跟着整轮预算一起取消。
	persistCtx, persistCancel := context.WithTimeout(context.Background(), imageTaskPersistTimeout)
	defer persistCancel()
	ok, err := op.ImageTaskStatusCAS(persistCtx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusCompleted, updates)
	if err != nil {
		log.Warnf("image task poll: mark task %s completed failed: %v", task.TaskID, err)
		// 状态没写进去 → 不释放预留、不退款：下一轮会重新执行它，
		// 那时释放才是对的。这里释放会让"在跑"的任务失去额度保护。
		return
	}
	if !ok {
		// 别的写者（过期清扫）赢了，或者这一行已经不是 processing。
		// 释放是幂等的，交出去不会有副作用。
		op.ImageTaskReservationRelease(task.TaskID)
		log.Warnf("image task poll: task %s was already advanced by another writer", task.TaskID)
		return
	}
	// 成功：只归还在途预留，**不退款**（预扣的就是这一单的钱）。
	op.ImageTaskReservationRelease(task.TaskID)
	log.Infof("image task poll: task %s completed (channel=%d, model=%s)", task.TaskID, task.ChannelID, task.Model)
}

// failImageTask 是失败路径的唯一入口：CAS 到 failed + 退款（幂等）+ 归还预留（幂等）。
//
// 「退款」和「释放预留」是两笔独立的账，必须分开做：
//   - 钱（Cost）是提交时真扣走的，失败要原路退回；
//   - 在途预留（UserCanRelay 那一份）是"这笔余额正在被一条任务占着"，任何终态都要还。
//
// 顺序：先 CAS（只有一个写者能赢），赢了再退款/释放。CAS 没赢说明别的写者已经
// 把它推进终态了，它会在自己那条路径上退款——我们这里直接返回，避免重复退。
func failImageTask(task model.ImageTask, reason string) {
	reason = imageTaskRedactFailure(reason)
	// 终态 + 退款用脱离整轮预算的短时限（见 imageTaskPersistTimeout）：一次
	// "判死并退款"如果被预算取消掉，用户的钱就留在库里等下一次机会了。
	persistCtx, persistCancel := context.WithTimeout(context.Background(), imageTaskPersistTimeout)
	defer persistCancel()
	ok, err := op.ImageTaskStatusCAS(persistCtx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusFailed, map[string]any{
		"completed_at":  time.Now().Unix(),
		"error_message": reason,
	})
	if err != nil {
		log.Warnf("image task poll: mark task %s failed: %v", task.TaskID, err)
		return
	}
	if !ok {
		log.Debugf("image task poll: task %s was already advanced by another writer", task.TaskID)
		return
	}
	if _, rerr := op.ImageTaskRefund(persistCtx, task); rerr != nil {
		log.Errorf("image task poll: refund task %s failed: %v", task.TaskID, rerr)
	}
	op.ImageTaskReservationRelease(task.TaskID)
	log.Warnf("image task poll: task %s marked failure: %s", task.TaskID, reason)
}

// sweepExpiredImageTasks 把"到 expires_at 还没出结果"的任务标失败并退款。
//
// 为什么必须有这一步（而不是靠查询侧的 404 兜住）：客户端查到 404 就当任务没了，
// 但**钱还扣在库里**。不在这里收口，那笔预扣就永远退不回去了。
func sweepExpiredImageTasks(ctx context.Context) {
	now := time.Now().Unix()
	expired, err := op.ImageTaskListExpired(ctx, now, imageTaskPollExpiredLimit)
	if err != nil {
		log.Warnf("image task poll: list expired tasks failed: %v", err)
		return
	}
	for _, task := range expired {
		if ctx.Err() != nil {
			return
		}
		failImageTask(task, "task expired before completion")
	}
}

// imageTaskChannelKey 按 id 找回执行这条任务要用的 key。
//
// 刻意**不**用 channel.GetAvailableChannelKeys：它会把"冷却中/刚失败"的 key 过滤掉。
// 对选路是对的，对执行已经提交的任务是错的——冷却中的 key 在它眼里等同于
// "这把 key 没了"，会把一条本来能成功的任务直接判失败。
// 与 videoTaskChannelKey 同一条理由：这里只问"这把 key 还在不在、开没开"。
func imageTaskChannelKey(channel *model.Channel, keyID int) (channelKey string, found bool, enabled bool) {
	if channel == nil || keyID == 0 {
		return "", false, false
	}
	for _, key := range channel.Keys {
		if key.ID == keyID {
			return key.ChannelKey, true, key.Enabled
		}
	}
	return "", false, false
}

// imageTaskFailureLimit 限制落进 error_message 的原因长度（text 列，但前端要显示）。
const imageTaskFailureLimit = 512

// imageTaskRedactFailure 收口失败原因：截断 + 去掉可能的换行噪声。
//
// 【红线】这里只会收到上游错误摘要、状态码、网络错误串——**绝不**传入任务行上的
// RequestBody（含用户 prompt）。脱敏本身由 relay.upstreamBodySummary（xredact）做，
// 本函数只负责长度与形态，避免把一份超长上游错误页塞进前端。
func imageTaskRedactFailure(reason string) string {
	reason = strings.Join(strings.Fields(strings.TrimSpace(reason)), " ")
	if reason == "" {
		return "image generation failed"
	}
	if len(reason) > imageTaskFailureLimit {
		reason = reason[:imageTaskFailureLimit] + "…"
	}
	return reason
}
