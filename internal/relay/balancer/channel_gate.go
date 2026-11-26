package balancer

import (
	"context"
	"sync"
	"time"
)

// 渠道级硬限流闸门（channel hard gate）。
//
// 为什么需要它：Channel.MaxConcurrent / Channel.RPMLimit 过去一直只是「软降档」——
// 到顶只在选路里排序靠后（见 spreadTier），该发的一次都没少。生产事故就是这条：
// 渠道上配了 RPM 上限，上游那边看到 8 RPM。软降档挡不住同一请求的重试 / 竞速 /
// 救援换家重发，于是上限形同虚设。这里改成硬准入：每一次真正发往上游的尝试，在
// BeginRuntimeAttempt 之前先**原子地**占住渠道并发位；RPM 则在发送前读一次当前分钟的
// 成功计数，到顶就不发。
//
// 作用域是**渠道**，不是 (渠道, key, 模型)：上限本来就是渠道级配置，同一渠道上
// 不同模型、不同 key 的尝试必须合计计数，否则换个模型名就能绕过上限。
//
// RPM 口径对齐 sub2api（_references/sub2api-learning/backend/internal/repository/
// rpm_cache.go 的 rpm:{id}:{unix/60} 固定整分钟桶 + handler 在 Forward 成功之后才计数）：
//   - **整分钟桶**：桶号 = now.Unix()/60，分钟一翻旧计数作废；不是 60 秒滑动窗口。
//   - **只数成功**：失败/超时/取消/被跳过都不消耗名额，只有上游转发成功才 +1
//     （见 RecordChannelRPMSuccess，relay 的 attempt 成功块调用）。
//   - **发送前判定**：准入只读「当前分钟成功数 >= 上限则拒」，不预占名额。
//     因此并发中的多个在途请求可能同时过准入，成功计数落地前有短暂超出——这是
//     sub2api 同款口径（非原子预占），属预期行为，不是缺陷。
//
// 并发位（inFlight）仍是硬预占：占不到就不发，释放才归还。排队语义
// （waiters / QueueSize / ChannelGateWaitAnyOpen）与并发位判定不变。
const (
	// ChannelGateQueueTimeoutDefault 是渠道没配排队时长（QueueTimeoutMs=0）时每个请求的
	// 默认等待上限。
	ChannelGateQueueTimeoutDefault = 3 * time.Second

	// ChannelGateReasonConcurrency / ChannelGateReasonRPM 是闸门拒绝的维度标签。
	// 它们会进 Iterator.Skip 的审计消息（AttemptSkipped），让日志能回答「这次到底
	// 是并发位满了还是 RPM 名额满了」。
	ChannelGateReasonConcurrency = "channel_concurrency_limit"
	ChannelGateReasonRPM         = "channel_rpm_limit"
)

// channelGatePollInterval 是排队期间的轮询间隔：够短，空出来的位立刻有人拿走；
// 够长，不把 CPU 烧在忙等上。
const channelGatePollInterval = 25 * time.Millisecond

// channelGate 是单个渠道的闸门状态。
//
// 并发位与 RPM 计数共用一把锁：判定要在**同一次临界区**里做完（先看并发位、再看当前
// 分钟成功数），否则两个维度各自的检查会互相插队、日志也说不清是谁挡的。锁的粒度是
// 单渠道，热路径上就是一次加解锁，不碰任何 I/O。
type channelGate struct {
	mu sync.Mutex

	// inFlight 是当前占住并发位的在途尝试数（release 时归还）。
	inFlight int64

	// waiters 是此刻排在这条渠道队里的请求数（跨 key、跨模型合计）。由
	// ChannelGateWaitAnyOpen 占/还：排队人数达到渠道的 QueueSize 后新来的请求立即被拒。
	waiters int

	// rpmMinute / rpmCount 是**单个整分钟桶**的成功计数，口径对齐 sub2api 的
	// rpm:{id}:{unix/60}（那边 TTL 120s 覆盖当前分钟）：rpmMinute 记这个桶代表哪一分钟
	// （now.Unix()/60），rpmCount 记这一分钟内**成功**的上游尝试数。分钟一翻，桶号
	// 对不上就当 0，不做 60 秒滑动汇总。
	//
	// 只数成功、发送前判定：失败的尝试不占名额；准入时读的是「当前分钟已成功几个」，
	// 不预占。所以并发在途的多个请求可能一起过准入、成功计数落地前短暂超出上限，
	// 这是 sub2api 同款口径，属预期。
	//
	// 注意：telemetry.go 里另有一套每秒桶（runtimeEntry.reqWindow/reqBucketSec，供
	// spreadTier 软降档），与本闸门的硬准入是两套计数，互不影响。
	rpmMinute int64
	rpmCount  int32
}

// globalChannelGates 只按渠道 ID 分片：key 是 channelID(int) -> *channelGate。
var globalChannelGates sync.Map

func channelGateFor(channelID int) *channelGate {
	gate := &channelGate{}
	actual, _ := globalChannelGates.LoadOrStore(channelID, gate)
	return actual.(*channelGate)
}

// ResetChannelGates 清空内存里的渠道闸门状态。生产代码不会调用它；测试用它避免
// 用例之间互相串味（同 ResetRuntimeTelemetry）。
func ResetChannelGates() {
	globalChannelGates = sync.Map{}
}

// channelGateMinuteOf 把一个时刻折算成整分钟桶号（unix/60），与 sub2api 的
// rpm:{accountID}:{minuteTimestamp} 同一口径。
func channelGateMinuteOf(now time.Time) int64 {
	return now.Unix() / 60
}

// recordRPMSuccessLocked 在当前分钟桶上 +1（分钟翻转时先归零）。
// 调用者必须持有 g.mu。只在**上游转发成功**之后调用（见 RecordChannelRPMSuccess）。
func (g *channelGate) recordRPMSuccessLocked(now time.Time) {
	minute := channelGateMinuteOf(now)
	if g.rpmMinute != minute {
		g.rpmMinute = minute
		g.rpmCount = 0
	}
	g.rpmCount++
}

// currentRPMCountLocked 读当前分钟桶里的成功计数；桶不是这一分钟（分钟已翻）就是 0。
// 调用者必须持有 g.mu。
func (g *channelGate) currentRPMCountLocked(now time.Time) int64 {
	if g.rpmMinute != channelGateMinuteOf(now) {
		return 0
	}
	return int64(g.rpmCount)
}

// tryAcquire 在**同一次持锁**里判定两个维度并占住并发位：并发位满了或当前分钟成功数
// 已到 RPM 上限就拒。失败返回 nil + 具体是哪个维度满了。成功返回的 release 幂等，
// 可安全多次调用。
//
// RPM 是「只读判定」：准入不消耗名额，名额只由成功转发累加（RecordChannelRPMSuccess）。
func (g *channelGate) tryAcquire(maxConcurrent, rpmLimit int, now time.Time) (func(), string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if maxConcurrent > 0 && g.inFlight >= int64(maxConcurrent) {
		return nil, ChannelGateReasonConcurrency
	}
	if rpmLimit > 0 && g.currentRPMCountLocked(now) >= int64(rpmLimit) {
		return nil, ChannelGateReasonRPM
	}

	// 并发位在这一次持锁里占上；RPM 不预占（成功后才计数）。
	g.inFlight++

	// sync.Once 保证 release 幂等：defer 链、早退分支、panic 收尾都可能重复调用，
	// 重复调用也绝不能把 inFlight 减穿。
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.inFlight > 0 {
				g.inFlight--
			}
			g.mu.Unlock()
		})
	}, ""
}

// AdmitChannelGate 尝试为 channelID 占一个「渠道级」并发位。
//
// maxConcurrent<=0 或 rpmLimit<=0 分别表示该渠道对应维度不限（两个都不限时直接放行，
// 不占位）。成功返回非 nil 的 release 函数（幂等，安全可多次调用）；失败返回 nil +
// reason（ChannelGateReasonConcurrency 并发满 / ChannelGateReasonRPM RPM 满）。
//
// 获取失败时，若 maxWait>0 且 ctx 未取消且 clientGone()=false，则在 maxWait 内轮询
// 等待（channelGatePollInterval 一拍）直到成功/超时；等到了也算成功占位。
// 每一拍都重新检查 ctx 取消 / 截止时间 / clientGone，调用端一走就不再白等。
//
// 注意排队上限（Channel.QueueSize / QueueTimeoutMs）不在这里：生产调用点一律传
// maxWait=0，排队统一在整轮全拒后只做一次（见 ChannelGateWaitAnyOpen）。
func AdmitChannelGate(ctx context.Context, channelID int, maxConcurrent, rpmLimit int, maxWait time.Duration, clientGone func() bool) (release func(), reason string) {
	if channelID <= 0 || (maxConcurrent <= 0 && rpmLimit <= 0) {
		// 不限（或渠道 ID 无效）：不占位。返回幂等空释放，调用方可以无条件 defer。
		return func() {}, ""
	}

	gate := channelGateFor(channelID)
	var deadline time.Time
	if maxWait > 0 {
		deadline = time.Now().Add(maxWait)
	}

	var lastReason string
	for {
		release, why := gate.tryAcquire(maxConcurrent, rpmLimit, time.Now())
		if why == "" {
			return release, ""
		}
		lastReason = why

		// 不排队的情形：没配等待预算 / 调用端已经走了 / 预算已经用完。
		// 把最后一次看到的维度带回去，主循环的审计才说得出「为什么没发」。
		if deadline.IsZero() {
			return nil, lastReason
		}
		if ctx != nil && ctx.Err() != nil {
			return nil, lastReason
		}
		if clientGone != nil && clientGone() {
			return nil, lastReason
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, lastReason
		}
		wait := channelGatePollInterval
		if remaining < wait {
			wait = remaining
		}
		if !channelGateSleep(ctx, wait) {
			return nil, lastReason
		}
	}
}

// GateCandidate 是整轮候选里被闸门拒掉的渠道（含上限与排队配置）：排队探测只回答
// 「有没有空位」，真正的占用发生在重跑候选时的 AdmitChannelGate。
//
// QueueSize<=0 表示这条候选不排队；QueueTimeoutMs=0 表示用 ChannelGateQueueTimeoutDefault。
type GateCandidate struct {
	ChannelID      int
	MaxConcurrent  int
	RPMLimit       int
	QueueSize      int
	QueueTimeoutMs int
}

// channelGateQueueTimeout 把一个候选的 QueueTimeoutMs 折算成等待时长：0 = 默认 3s。
func channelGateQueueTimeout(candidate GateCandidate) time.Duration {
	if candidate.QueueTimeoutMs > 0 {
		return time.Duration(candidate.QueueTimeoutMs) * time.Millisecond
	}
	return ChannelGateQueueTimeoutDefault
}

// ChannelGateQueueBudget 是整轮排队预算：取候选里最大的生效等待时长（QueueTimeoutMs，
// 0 用默认），所以整轮只等一次、不超过最宽松的那个渠道配置。没有可排队的候选
// （QueueSize<=0）时返回 0，调用方的排队就等于立刻放弃。
func ChannelGateQueueBudget(candidates []GateCandidate) time.Duration {
	var budget time.Duration
	for _, candidate := range candidates {
		if candidate.ChannelID <= 0 || candidate.QueueSize <= 0 {
			continue
		}
		if timeout := channelGateQueueTimeout(candidate); timeout > budget {
			budget = timeout
		}
	}
	return budget
}

// channelGateHasRoomLocked 是 ChannelGateHasRoom 的持锁内核。调用者必须持有 g.mu。
func (g *channelGate) hasRoomLocked(maxConcurrent, rpmLimit int, now time.Time) bool {
	if maxConcurrent > 0 && g.inFlight >= int64(maxConcurrent) {
		return false
	}
	if rpmLimit > 0 && g.currentRPMCountLocked(now) >= int64(rpmLimit) {
		return false
	}
	return true
}

// RecordChannelRPMSuccess 记一次**成功**的上游转发：把该渠道当前分钟桶的成功计数 +1
// （分钟翻转时从 0 重新开始）。
//
// 由 relay 的 attempt 成功块调用（internal/relay/relay.go 的 `if fwdErr == nil` 分支）；
// 失败/被跳过/取消都不调用，所以 RPM 上限的语义是「每分钟最多成功几个请求」，与
// sub2api 在 Forward 成功后才计数一致。
func RecordChannelRPMSuccess(channelID int) {
	if channelID <= 0 {
		return
	}
	gate := channelGateFor(channelID)
	gate.mu.Lock()
	gate.recordRPMSuccessLocked(time.Now())
	gate.mu.Unlock()
}

// ChannelGateCurrentRPM 只读返回该渠道「当前这分钟」的成功计数，供渠道列表/编辑页
// 显示实时 RPM（对齐 sub2api 账号页的 current_rpm）。
//
// 不占位、不等待、不创建闸门状态（渠道还没有任何闸门记录时就是 0）。
func ChannelGateCurrentRPM(channelID int) int {
	if channelID <= 0 {
		return 0
	}
	value, ok := globalChannelGates.Load(channelID)
	if !ok {
		return 0
	}
	gate, ok := value.(*channelGate)
	if !ok || gate == nil {
		return 0
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return int(gate.currentRPMCountLocked(time.Now()))
}

// reserveWaiter 占一个排队位：queueSize<=0（不排队）或队已满时返回 false。
// 判定与占位在同一把锁里，否则并发请求会一起把排队人数推过 QueueSize。
func (g *channelGate) reserveWaiter(queueSize int) bool {
	if queueSize <= 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.waiters >= queueSize {
		return false
	}
	g.waiters++
	return true
}

// releaseWaiter 归还一个排队位（等待结束、超时、取消都走这里）。幂等到不会减成负数。
func (g *channelGate) releaseWaiter() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.waiters > 0 {
		g.waiters--
	}
}

// ChannelGateHasRoom 只读探测：该渠道此刻是否还占得到「并发位 + 当前分钟的 RPM 余额」。
// 不占位、不等待、不碰任何计数——调用方拿它决定「要不要为整轮被拒的候选再跑一轮」，
// 真正的占用发生在重跑候选的 AdmitChannelGate 里。
func ChannelGateHasRoom(channelID, maxConcurrent, rpmLimit int) bool {
	if channelID <= 0 || (maxConcurrent <= 0 && rpmLimit <= 0) {
		// 闸门不接管的维度（未配置/渠道 ID 无效）：永远视为有空位。
		return true
	}
	gate := channelGateFor(channelID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.hasRoomLocked(maxConcurrent, rpmLimit, time.Now())
}

// channelGateWaitSlot 是一轮排队里占住的一条候选渠道的排队位。deadline 是这条候选
// 自己的等待上限（QueueTimeoutMs，0 用默认，且不超过整轮预算）。
type channelGateWaitSlot struct {
	candidate GateCandidate
	gate      *channelGate
	deadline  time.Time
	released  bool
}

// release 归还排队位，幂等（超时先还、函数退出时 defer 再还一次也不会多减）。
func (s *channelGateWaitSlot) release() {
	if s == nil || s.released {
		return
	}
	s.released = true
	s.gate.releaseWaiter()
}

// ChannelGateWaitAnyOpen 在 candidates 任一渠道出现空位时返回 true（调用方据此重跑一轮
// 候选）。
//
// 同渠道候选去重后只占一个排队位；QueueSize<=0（不排队）或队列已满的候选立即退出；
// 每个候选最多等自己的 QueueTimeoutMs（0 用默认，且不超过 budget）；每
// channelGatePollInterval 探一拍，ctx 取消 / clientGone / 预算用尽时立刻返回 false。
// 无论从哪条分支退出，本次占的排队位都会归还。
func ChannelGateWaitAnyOpen(ctx context.Context, candidates []GateCandidate, budget time.Duration, clientGone func() bool) bool {
	if len(candidates) == 0 || budget <= 0 {
		return false
	}
	deadline := time.Now().Add(budget)
	slots := make([]*channelGateWaitSlot, 0, len(candidates))
	defer func() {
		for _, slot := range slots {
			slot.release()
		}
	}()

	queued := make(map[int]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.ChannelID <= 0 {
			continue
		}
		if _, dup := queued[candidate.ChannelID]; dup {
			// 同渠道只排一个位：重复候选不能把 QueueSize 吃两次。
			continue
		}
		queued[candidate.ChannelID] = struct{}{}
		gate := channelGateFor(candidate.ChannelID)
		if !gate.reserveWaiter(candidate.QueueSize) {
			// QueueSize<=0（这条渠道不排队）或者队已经满了：立即拒，不白等。
			continue
		}
		slotDeadline := time.Now().Add(channelGateQueueTimeout(candidate))
		if slotDeadline.After(deadline) {
			slotDeadline = deadline
		}
		slots = append(slots, &channelGateWaitSlot{candidate: candidate, gate: gate, deadline: slotDeadline})
	}

	for {
		now := time.Now()
		pending := 0
		for _, slot := range slots {
			if slot.released {
				continue
			}
			if !now.Before(slot.deadline) {
				// 这条候选已经等够自己的配置：先归还排队位，再把剩下的候选等完。
				slot.release()
				continue
			}
			pending++
			if ChannelGateHasRoom(slot.candidate.ChannelID, slot.candidate.MaxConcurrent, slot.candidate.RPMLimit) {
				return true
			}
		}
		if pending == 0 {
			// 没有能排队的候选（不排队 / 队满 / 都等够了自己的上限）。
			return false
		}
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		if clientGone != nil && clientGone() {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		wait := channelGatePollInterval
		if remaining < wait {
			wait = remaining
		}
		if !channelGateSleep(ctx, wait) {
			return false
		}
	}
}

// channelGateSleep 睡 d；ctx 被取消时提前返回 false（调用方据此停止排队）。
func channelGateSleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	if ctx == nil {
		<-timer.C
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
