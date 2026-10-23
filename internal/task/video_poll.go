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
	// videoTaskPollBatchLimit 是单轮最多推进多少条。积压时一轮不跑到天亮。
	videoTaskPollBatchLimit = 100
	// videoTaskPollStaleLimit 是单轮最多收口多少条超时任务。
	videoTaskPollStaleLimit = 200
	// videoTaskPollSweepBudget 是超时扫描在整轮预算里最多能占多少。
	//
	// 为什么必须切一块给扫描、而不是让它和主轮询抢同一个整轮 ctx：
	// 超时扫描要「判死前先问一次上游」，每条都可能撞上一个"连得上但对端不返回"
	// 的挂起连接（单条 20s 封顶）。6 条这样的任务就能吃光 120s 整轮预算，然后
	// op.VideoTaskListUnfinished 拿到一个已经过期的 ctx，直接
	// `context deadline exceeded` 返回 —— 本轮一条任务都没推进。
	//
	// 下一轮 Order("id") 依旧从那批老任务开始，因此这个状态是**自锁**的：
	// 所有用户的新任务永久停在 submitted，前端无限转圈，日志里只有一行
	// "list unfinished tasks failed" 的 Warn，看起来像偶发抖动。
	//
	// 切出 1/3（40s）给扫描：挂起任务最多只能占掉这 40s，剩下 80s 保证主轮询
	// 一定能推进真实任务。扫描被打断不算故障 —— 没收口的超时任务下一轮接着收。

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

// videoTaskPollTaskTimeout 是**单条**上游查询的独立时限。它与整轮的
// videoTaskPollRoundTimeout 是嵌套关系（子 ctx 不会越过父 ctx），不是用来放宽整轮的。
//
// 为什么单条必须有独立时限：整条链路只有一个整轮 ctx，一条「连得上、握手完、但对端
// 一直不返回」的上游会吃光整轮预算，后面所有任务都被 `if ctx.Err() != nil { return }`
// 跳过；而下一轮 VideoTaskListUnfinished 仍按 id 升序从头取，同一条慢任务又挡在最前面——
// 一个上游的故障因此能持续拖住其它渠道的健康任务。单条封顶后，慢的那条自己让路，
// 本轮继续推进后面的任务。
//
// 取 20s 的依据：
//  1. 这是一条只取几 KB JSON 的 GET，TLS 握手之后健康上游的 TTFB 是百毫秒级；
//     20s 已经宽了两个数量级，不会把「慢但能用」的上游误判成挂起。
//  2. 整轮预算 120s。一条挂起任务最多吃掉 1/6 轮，剩 100s 仍能推进后面的任务；
//     取 30s 只剩 90s，同一轮里接连撞上两条挂起任务时容错余量明显更小。
//  3. 每轮都可能重新撞上同一批挂起任务，20s 让「最坏一轮」的浪费有界且可预期。
//
// 之所以是 var 而不是 const：测试要把「上游吊着不返回」的场景压到毫秒级，否则单条
// 用例要真等 20 秒。生产路径不写它（唯一写点是测试里的 override）。
var videoTaskPollRoundTimeout = 2 * time.Minute

// videoTaskPollSweepBudget 见上方 const 块注释；改成 var 是为了让测试压到毫秒级。
var videoTaskPollSweepBudget = 40 * time.Second
var videoTaskPollTaskTimeout = 20 * time.Second

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

// runVideoTaskPollOnce 跑一轮：扫超时（判死前先问一次上游）→ 捞未完成 → 按渠道分组 →
// 逐个查上游 → CAS 写回。
//
// 顺序是刻意的：先收口超时的，再取未完成列表，这样本轮刚被判失败的任务不会又被
// 当成「未完成」去查一次上游。反方向的那一半——超时扫描里刚问过、上游说还在跑的任务——
// 由 sweepTimedOutVideoTasks 的返回值带回来，从本轮批次里剔掉：答案刚刚落库，
// 同一轮再问一遍只会把一条慢上游的耗时在一轮里算两次。
func runVideoTaskPollOnce() {
	// 库没起来（例如某个只跑纯函数用例的测试二进制）就直接不干活，
	// 而不是在 nil 连接上 panic。
	if db.GetDB() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), videoTaskPollRoundTimeout)
	defer cancel()

	// 超时扫描用**独立的子预算**，不能和主轮询共用一个整轮 ctx：
	// 扫描里每条都要问一次上游，挂起的连接会把整轮吃光，主轮询就再也拿不到
	// 活着的 ctx（详见 videoTaskPollSweepBudget 的注释）。扫描超时只是"这轮没收完"，
	// 下一轮继续收；主轮询的预算必须留住，否则新任务永远排不上。
	sweepCtx, sweepCancel := context.WithTimeout(ctx, videoTaskPollSweepBudget)
	polledBySweep := sweepTimedOutVideoTasks(sweepCtx)
	sweepCancel()

	tasks, err := op.VideoTaskListUnfinished(ctx, videoTaskPollBatchLimit)
	if err != nil {
		log.Warnf("video task poll: list unfinished tasks failed: %v", err)
		return
	}
	tasks = dropVideoTasksAlreadyPolledThisRound(tasks, polledBySweep)
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

	// 单条查询自带上限（见 videoTaskPollTaskTimeout）：超时只会让这一条本轮跳过，
	// 绝不中止整轮——上游可能只是慢，任务留在未完成集合里下一轮再问。
	statusCode, body, err := queryVideoTaskUpstream(ctx, channel, channelKey, task.UpstreamTaskID)
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

// queryVideoTaskUpstream 是所有上游查询的唯一入口：先给单条查询套上
// videoTaskPollTaskTimeout 的独立时限，再交给 relay。
//
// 集中在一处，是为了让「单条上限」只有一份定义：分散到两个调用点迟早会漏掉一个，
// 而漏掉的那一个正是「一条慢查询拖死整轮」的复发点。
func queryVideoTaskUpstream(ctx context.Context, channel *model.Channel, channelKey, upstreamTaskID string) (int, []byte, error) {
	queryCtx, cancel := context.WithTimeout(ctx, videoTaskPollTaskTimeout)
	defer cancel()
	return relay.VideoTaskPollStatus(queryCtx, channel, channelKey, upstreamTaskID)
}

// sweepTimedOutVideoTasks 收口「早于 videoTaskPollTimeout 仍未进终态」的任务。
//
// 【判死之前必须先问一次上游】只按本地时钟判失败是错的：服务停机/重启/长时间断网期间，
// 上游很可能早就把片生成好了，本地却还停在 submitted/queued。此时直接写一条 timeout
// failure，会让 op.VideoTaskListUnfinished 把它排除在未完成集合之外——结果地址从此再也
// 补不回来，客户端直查上游也修不好这一行。用户付过费却看到永久失败，甚至可能再付一次重做。
// 所以每条超时任务都先做最后一次上游查询：
//
//   - 上游给出终态 → 按真实结果落库（success 会写 result_url + finish_time），不覆盖成 timeout；
//   - 上游说还在跑 / 给了不认识的状态 → 不判失败，留在未完成集合里继续推进；
//   - 问不出结论（渠道或 key 已不可用、上游 404 不认这个任务、查询本身失败/超时）
//     → 才按超时判失败，保留原语义（一条永远问不出结论的行不能永久占着未完成集合）。
//
// 返回值是本轮「已经问过上游、而它那边仍活着或已给出真实结果」的任务 id 集合：调用方
// 据此把这几条从本轮待推进批次里剔掉，避免同一轮对同一条上游问两遍。
func sweepTimedOutVideoTasks(ctx context.Context) map[int64]struct{} {
	polled := make(map[int64]struct{})
	deadline := time.Now().Add(-videoTaskPollTimeout).Unix()
	stale, err := op.VideoTaskListStale(ctx, deadline, videoTaskPollStaleLimit)
	if err != nil {
		log.Warnf("video task poll: list stale tasks failed: %v", err)
		return polled
	}
	for _, task := range stale {
		if ctx.Err() != nil {
			// 🔴 预算耗尽就收手，并把这些**没来得及问**的任务也登记进 polled：
			// 它们已经占据本轮的时间和队头窗口，若再流回主轮询，就会把剩下的
			// 预算二次花在同一批挂起连接上，健康任务永远轮不到。
			// 登记后本轮直接跳过它们；下一轮重新开始，它们是老任务会先被扫到。
			for _, rest := range stale {
				polled[rest.ID] = struct{}{}
			}
			return polled
		}
		if videoTaskTimeoutResolvedUpstream(ctx, task) {
			polled[task.ID] = struct{}{}
			continue
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
	return polled
}

// videoTaskTimeoutResolvedUpstream 是超时任务判失败之前的最后一次上游查询。
//
// 返回 true 表示「上游给出了这一行的真实状态，本地已经按它收敛，或上游还在跑」——
// 调用方绝不许再写 timeout failure；返回 false 表示问不出结论，调用方按超时判失败。
func videoTaskTimeoutResolvedUpstream(ctx context.Context, task model.VideoTask) bool {
	channel, err := op.ChannelGet(task.ChannelID, ctx)
	if err != nil {
		// 渠道已经不在缓存里：连问的地方都没有，只能按超时收口。
		log.Warnf("video task poll: timed-out task %s cannot resolve channel %d: %v", task.UpstreamTaskID, task.ChannelID, err)
		return false
	}
	channelKey, found, enabled := videoTaskChannelKey(channel, task.ChannelKeyID)
	if !found || !enabled {
		log.Warnf("video task poll: timed-out task %s has no usable key on channel %d (found=%t enabled=%t)", task.UpstreamTaskID, channel.ID, found, enabled)
		return false
	}

	statusCode, body, err := queryVideoTaskUpstream(ctx, channel, channelKey, task.UpstreamTaskID)
	if err != nil {
		// 查询本身失败（网络、超时、5xx、上游 404 不认这个任务）：问不出结论，按超时判失败。
		// 这一支刻意**不**改成「继续留着重试」——那就从「不查就判死」越到了另一个极端：
		// 一条永远问不动的行会永久钉在未完成集合里，每轮白烧一次上游查询。
		log.Warnf("video task poll: last-look query for timed-out task %s failed (status=%d): %v", task.UpstreamTaskID, statusCode, err)
		return false
	}
	result, perr := parseVideoTaskPollBody(body)
	if perr != nil {
		// 读不懂的响应同样问不出结论。
		log.Warnf("video task poll: last-look query for timed-out task %s returned an unparseable body: %v", task.UpstreamTaskID, perr)
		return false
	}

	// 上游有真实答案：按它落库（success 会带 result_url 和 finish_time），
	// 绝不能再写一条 timeout failure 把它盖掉。
	applyVideoTaskPollResult(ctx, task, result)
	if model.IsVideoTaskTerminal(result.status) {
		log.Warnf("video task poll: task %s exceeded the local %s timeout but the upstream already reached %q; kept the upstream result", task.UpstreamTaskID, videoTaskPollTimeout, result.status)
		return true
	}
	// 上游还在干活（或给了不认识的状态）：本地判超时是错的，留在未完成集合里继续推进。
	log.Warnf("video task poll: task %s exceeded the local %s timeout but the upstream still reports %q; keeping it unfinished", task.UpstreamTaskID, videoTaskPollTimeout, result.status)
	return true
}

// dropVideoTasksAlreadyPolledThisRound 把本轮超时收口里刚问过上游的任务从待推进批次里剔掉。
// 它们这一轮的上游答案刚刚落库，同一轮再问一遍既不会更快收敛，又会让一条慢上游的耗时
// 在一轮里被算两次（预算是 120s，不是 240s）。
func dropVideoTasksAlreadyPolledThisRound(tasks []model.VideoTask, polled map[int64]struct{}) []model.VideoTask {
	if len(polled) == 0 {
		return tasks
	}
	filtered := tasks[:0]
	for _, task := range tasks {
		if _, ok := polled[task.ID]; ok {
			continue
		}
		filtered = append(filtered, task)
	}
	return filtered
}
