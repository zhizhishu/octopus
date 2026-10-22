package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// 视频任务轮询器：把 video_tasks 表里还没进终态的行推着往前走。
//
// 它只做一件事——查上游、写状态。不扣费、不做结算、不改中继的返回形状：
// 本项目的视频链路从头到尾没有任何 Quota 写入（见 relay/videos.go，只有 metrics.Save），
// 在没扣过费的地方做「结算/退款」等于凭空造账。
const (
	// videoTaskPollInterval 是两轮的间隔。视频成片是分钟级的，30 秒的粒度既够快
	// 又不至于把上游的查询配额烧在一个刚提交的任务上。
	videoTaskPollInterval = 30 * time.Second
	// videoTaskPollRoundTimeout 是整轮的总时限。没有它，一条卡在对端 TLS 握手上的
	// 任务就能让这一轮永远不结束，后面所有任务一起饿死。
	videoTaskPollRoundTimeout = 2 * time.Minute
	// videoTaskPollBatchLimit 是单轮最多推进多少条。积压时一轮不跑到天亮。
	videoTaskPollBatchLimit = 100
	// videoTaskPollStaleLimit 是单轮最多收口多少条超时任务。
	videoTaskPollStaleLimit = 200
	// videoTaskPollTimeout 是「多久还没终态就判超时」。取 2h：
	//  1) 视频生成在这些上游是分钟级，2 小时是异常而不是慢；
	//  2) 内存里的归属记录（relay.defaultVideoTaskTTL）也是 2h，两者同一量级，
	//     不会出现「库里还活着、内存里早就丢了」这种自相矛盾的状态；
	//  3) 判超时只是给我们自己这一行记账，客户端拿到的永远是上游原始响应，
	//     所以这里必须偏保守——宁可晚收一条，不要早判一条真在跑的任务。
	videoTaskPollTimeout = 2 * time.Hour
	// videoTaskPollSnippetLimit 限制留痕片段长度（落 fail_reason，text 列）。
	videoTaskPollSnippetLimit = 512
)

var (
	videoTaskPollOnce   sync.Once
	videoTaskPollStarts atomic.Int32
)

// VideoTaskPollStart 启动视频任务轮询器（进程内单例）。
//
// 【绝不许改成 init() 自注册】这一条不是风格问题：
// internal/task/init.go 顶部那段注释写得很清楚——「测试二进制绝不该有无人管生命周期的
// 后台写库者（会撞各用例的临时 DB，CI 实翻过）」。轮询器每一轮都读库、写库，一旦由
// init() 在包加载时自己起来，每个 go test 用例都会和它抢同一个临时库文件，
// 而且用例关库之后它还会继续跑。
//
// 所以和 op.RelayLogFlusherStart() 一样：只在生产启动路径上由 internal/task/init.go
// 显式调用。sync.Once 保证重复调用不会起第二个循环（Init 里有几处 early-return，
// 将来若有人把它挪到那些分支之后也不会意外停掉或多起一份）。
func VideoTaskPollStart() {
	videoTaskPollOnce.Do(func() {
		videoTaskPollStarts.Add(1)
		safe.SafeGo("video-task-poll", func() {
			ticker := time.NewTicker(videoTaskPollInterval)
			defer ticker.Stop()
			for range ticker.C {
				runVideoTaskPollOnce()
			}
		})
	})
}

// runVideoTaskPollOnce 跑一轮：扫超时 → 捞未完成 → 按渠道分组 → 逐个查上游 → CAS 写回。
//
// 顺序是刻意的：先收口超时的，再取未完成列表，这样本轮刚被判失败的任务不会又被
// 当成「未完成」去查一次上游。
func runVideoTaskPollOnce() {
	// 库没起来（例如某个只跑纯函数用例的测试二进制）就直接不干活，
	// 而不是在 nil 连接上 panic。
	if db.GetDB() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), videoTaskPollRoundTimeout)
	defer cancel()

	sweepTimedOutVideoTasks(ctx)

	tasks, err := op.VideoTaskListUnfinished(ctx, videoTaskPollBatchLimit)
	if err != nil {
		log.Warnf("video task poll: list unfinished tasks failed: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	groups := groupVideoTasksByChannel(tasks)
	for index, group := range groups {
		if ctx.Err() != nil {
			log.Warnf("video task poll: round timeout reached, %d channel group(s) deferred to the next round", len(groups)-index)
			return
		}
		pollVideoTaskGroup(ctx, group)
	}
}

// videoTaskChannelGroup 是一个渠道下待推进的任务。同渠道只取一次渠道对象：
// op.ChannelGet 走的是内存缓存，但后面的 key 解析、渠道存活判断都是按渠道做的，
// 按渠道分组能让「渠道被删」这类结论一次得出一整组任务的命运。
type videoTaskChannelGroup struct {
	channelID int
	tasks     []model.VideoTask
}

func groupVideoTasksByChannel(tasks []model.VideoTask) []videoTaskChannelGroup {
	order := make([]int, 0, 4)
	byChannel := make(map[int]*videoTaskChannelGroup, 4)
	for _, task := range tasks {
		group, ok := byChannel[task.ChannelID]
		if !ok {
			group = &videoTaskChannelGroup{channelID: task.ChannelID}
			byChannel[task.ChannelID] = group
			order = append(order, task.ChannelID)
		}
		group.tasks = append(group.tasks, task)
	}
	groups := make([]videoTaskChannelGroup, 0, len(order))
	for _, channelID := range order {
		groups = append(groups, *byChannel[channelID])
	}
	return groups
}

func pollVideoTaskGroup(ctx context.Context, group videoTaskChannelGroup) {
	channel, err := op.ChannelGet(group.channelID, ctx)
	if err != nil {
		// 渠道已删除/不在缓存：这一组任务永远查不到上游了。一次判失败并写明原因，
		// 留在「未完成」集合里只会每个周期重试一次、永不收敛。
		reason := fmt.Sprintf("video channel %d is unavailable: %v", group.channelID, err)
		for _, task := range group.tasks {
			markVideoTaskFailed(ctx, task, reason)
		}
		return
	}
	for _, task := range group.tasks {
		if ctx.Err() != nil {
			return
		}
		pollOneVideoTask(ctx, channel, task)
	}
}

func pollOneVideoTask(ctx context.Context, channel *model.Channel, task model.VideoTask) {
	channelKey, found, enabled := videoTaskChannelKey(channel, task.ChannelKeyID)
	if !found {
		markVideoTaskFailed(ctx, task, fmt.Sprintf("video channel key %d no longer exists on channel %d", task.ChannelKeyID, channel.ID))
		return
	}
	if !enabled {
		markVideoTaskFailed(ctx, task, fmt.Sprintf("video channel key %d on channel %d is disabled", task.ChannelKeyID, channel.ID))
		return
	}

	statusCode, body, err := relay.VideoTaskPollStatus(ctx, channel, channelKey, task.UpstreamTaskID)
	if err != nil {
		if statusCode == http.StatusNotFound {
			// 上游自己不认这个任务了：再问一万次也是 404，判失败收口。
			markVideoTaskFailed(ctx, task, "upstream no longer knows this video task (404)")
			return
		}
		// 其余错误（网络、5xx、鉴权暂时失效、渠道被临时停用）都当可恢复：
		// 这一轮跳过这一条，下一轮再试；真的一直不行由超时扫描统一收口，
		// 不会无限重试。
		log.Warnf("video task poll: task %s upstream query failed (status=%d): %v", task.UpstreamTaskID, statusCode, err)
		return
	}

	result, perr := parseVideoTaskPollBody(body)
	if perr != nil {
		// 一条读不懂的响应不许拖垮整批，也不许静默：记下来，下一轮再试。
		log.Warnf("video task poll: task %s returned an unparseable body: %v", task.UpstreamTaskID, perr)
		return
	}
	applyVideoTaskPollResult(ctx, task, result)
}

// videoTaskPollResult 是本层从上游响应里抽出来的、准备写回行上的字段。
type videoTaskPollResult struct {
	status     model.VideoTaskStatus
	progress   string
	resultURL  string
	failReason string
	// snippet 只在状态未知时填，用来把原始响应片段留在行上（见 applyVideoTaskPollResult）。
	snippet string
}

func parseVideoTaskPollBody(body []byte) (videoTaskPollResult, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return videoTaskPollResult{}, err
	}
	rawStatus, _ := raw["status"].(string)
	status := mapVideoTaskUpstreamStatus(rawStatus)

	result := videoTaskPollResult{
		status:     status,
		progress:   videoTaskProgress(raw),
		resultURL:  videoTaskResultURL(raw),
		failReason: videoTaskFailureReason(raw),
	}
	if status == model.VideoTaskUnknown {
		result.snippet = truncateVideoTaskSnippet(rawStatus, body)
	}
	return result, nil
}

// mapVideoTaskUpstreamStatus 把上游的 status 翻译成这张表的七个值。
//
// 表的状态值本来就与上游（火山 Ark / 豆包视频生成那一路）逐字对齐，所以已知值原样落库，
// 不认识的**一律 unknown**，不猜：猜成 failure 会让一个真在跑的任务被提前判定失败，
// 猜成 success 会把空结果当成品。unknown 不是终态（见 model.IsVideoTaskTerminal），
// 下一轮继续问，让上游自己收敛。
func mapVideoTaskUpstreamStatus(raw string) model.VideoTaskStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(model.VideoTaskNotStart):
		return model.VideoTaskNotStart
	case string(model.VideoTaskSubmitted):
		return model.VideoTaskSubmitted
	case string(model.VideoTaskQueued):
		return model.VideoTaskQueued
	case string(model.VideoTaskInProgress), "processing", "running", "pending":
		return model.VideoTaskInProgress
	case string(model.VideoTaskSuccess), "succeeded", "completed", "complete", "finished", "done":
		return model.VideoTaskSuccess
	case string(model.VideoTaskFailure), "failed", "error", "expired", "cancelled", "canceled":
		return model.VideoTaskFailure
	default:
		return model.VideoTaskUnknown
	}
}

func videoTaskProgress(raw map[string]any) string {
	value, ok := raw["progress"]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%g", typed)
	default:
		return ""
	}
}

func videoTaskResultURL(raw map[string]any) string {
	for _, key := range []string{"video_url", "result_url", "url"} {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	// 有的上游把结果放在 output.{url,video_url} 里。
	if output, ok := raw["output"].(map[string]any); ok {
		for _, key := range []string{"video_url", "result_url", "url"} {
			if value, ok := output[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func videoTaskFailureReason(raw map[string]any) string {
	for _, key := range []string{"fail_reason", "failure_reason", "error_message"} {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"error", "failure"} {
		switch typed := raw[key].(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case map[string]any:
			if message, ok := typed["message"].(string); ok && strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
	}
	return ""
}

func truncateVideoTaskSnippet(rawStatus string, body []byte) string {
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		snippet = fmt.Sprintf("status=%q", rawStatus)
	}
	if len(snippet) > videoTaskPollSnippetLimit {
		snippet = snippet[:videoTaskPollSnippetLimit]
	}
	return snippet
}

// applyVideoTaskPollResult 把一次成功的上游查询结果按 CAS 写回。
//
// 状态没变、也没有别的字段要改时直接跳过：轮询每 30 秒一轮，每条在跑的任务都写一次
// 同样的值在 SQLite 上是纯白费的写锁争抢。
func applyVideoTaskPollResult(ctx context.Context, task model.VideoTask, result videoTaskPollResult) {
	updates := make(map[string]any, 4)

	if result.progress != "" && result.progress != task.Progress {
		updates["progress"] = result.progress
	}
	if result.resultURL != "" && result.resultURL != task.ResultURL {
		updates["result_url"] = result.resultURL
	}

	switch {
	case model.IsVideoTaskTerminal(result.status):
		if result.status == model.VideoTaskFailure {
			reason := result.failReason
			if reason == "" {
				reason = "upstream reported failure without a reason"
			}
			if reason != task.FailReason {
				updates["fail_reason"] = reason
			}
		} else if task.FailReason != "" {
			// 之前的失败/未知留痕必须清掉：终态成功了还挂着失败原因，
			// 谁读这一行都会被误导。
			updates["fail_reason"] = ""
		}
		if finishTime := time.Now().Unix(); finishTime != task.FinishTime {
			updates["finish_time"] = finishTime
		}
	case result.status == model.VideoTaskUnknown:
		// 不认识的状态不许静默丢：原始响应片段留在行上（文字列里只有 fail_reason 能用；
		// 塞进 result_url 等于把垃圾当结果地址发给客户端，所以不碰 result_url）。
		if result.snippet != "" && result.snippet != task.FailReason {
			updates["fail_reason"] = result.snippet
		}
	default:
		if task.FailReason != "" {
			updates["fail_reason"] = ""
		}
	}

	if result.status == task.Status && len(updates) == 0 {
		return
	}

	ok, err := op.VideoTaskStatusCAS(ctx, task.ID, task.Status, result.status, updates)
	if err != nil {
		log.Warnf("video task poll: write status for task %s failed: %v", task.UpstreamTaskID, err)
		return
	}
	if !ok {
		logVideoTaskCASMiss(ctx, task)
		return
	}
	log.Debugf("video task poll: task %s advanced %s -> %s", task.UpstreamTaskID, task.Status, result.status)
}

func markVideoTaskFailed(ctx context.Context, task model.VideoTask, reason string) {
	ok, err := op.VideoTaskStatusCAS(ctx, task.ID, task.Status, model.VideoTaskFailure, map[string]any{
		"fail_reason": reason,
		"finish_time": time.Now().Unix(),
	})
	if err != nil {
		log.Warnf("video task poll: mark task %s failed: %v", task.UpstreamTaskID, err)
		return
	}
	if !ok {
		logVideoTaskCASMiss(ctx, task)
		return
	}
	log.Warnf("video task poll: task %s marked failure: %s", task.UpstreamTaskID, reason)
}

// logVideoTaskCASMiss 区分 CAS 没写进去的两种原因：别的写者赢了我们（正常），
// 还是这一行在状态没变的情况下也写不进去（真出错，必须告警）。
// 不区分就等于把真错误静默成「并发正常」。
func logVideoTaskCASMiss(ctx context.Context, task model.VideoTask) {
	current, err := op.VideoTaskGetByUpstreamID(ctx, task.UpstreamTaskID)
	if err != nil {
		log.Warnf("video task poll: CAS missed for task %s and the re-read failed: %v", task.UpstreamTaskID, err)
		return
	}
	if current == nil {
		log.Warnf("video task poll: CAS missed for task %s and the row is gone", task.UpstreamTaskID)
		return
	}
	if current.Status != task.Status {
		log.Debugf("video task poll: task %s was advanced by another writer (%s -> %s)", task.UpstreamTaskID, task.Status, current.Status)
		return
	}
	log.Warnf("video task poll: CAS missed for task %s while the row status is still %s; the write did not land", task.UpstreamTaskID, task.Status)
}

// videoTaskChannelKey 按 id 找回轮询这条任务要用的 key。
//
// 刻意不复用 relay 里那个 videoChannelKeyByID：它基于 GetAvailableChannelKeys，
// 会把「冷却中/刚失败」的 key 过滤掉。对热路径那是对的，对后台轮询是错的——
// 冷却中的 key 在它眼里等同于「这把 key 没了」，会把一个健康任务直接标失败。
// 轮询需要的是「这把 key 还在不在、开没开」，所以直接看渠道原始 Keys。
func videoTaskChannelKey(channel *model.Channel, keyID int) (channelKey string, found bool, enabled bool) {
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

// sweepTimedOutVideoTasks 把「早于 videoTaskPollTimeout 仍未进终态」的任务判失败。
func sweepTimedOutVideoTasks(ctx context.Context) {
	deadline := time.Now().Add(-videoTaskPollTimeout).Unix()
	stale, err := op.VideoTaskListStale(ctx, deadline, videoTaskPollStaleLimit)
	if err != nil {
		log.Warnf("video task poll: list stale tasks failed: %v", err)
		return
	}
	for _, task := range stale {
		if ctx.Err() != nil {
			return
		}
		reason := fmt.Sprintf("video task timed out: no terminal status within %s after submit", videoTaskPollTimeout)
		ok, err := op.VideoTaskStatusCAS(ctx, task.ID, task.Status, model.VideoTaskFailure, map[string]any{
			"fail_reason": reason,
			"finish_time": time.Now().Unix(),
		})
		if err != nil {
			log.Warnf("video task poll: mark timed-out task %s failed: %v", task.UpstreamTaskID, err)
			continue
		}
		if !ok {
			logVideoTaskCASMiss(ctx, task)
			continue
		}
		log.Warnf("video task poll: task %s marked failure after timeout: %s", task.UpstreamTaskID, reason)
	}
}
