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
// BeginRuntimeAttempt 之前先**原子地**占住「渠道并发位 + 渠道 RPM 滑动窗口名额」，
// 占不到就不发。
//
// 作用域是**渠道**，不是 (渠道, key, 模型)：上限本来就是渠道级配置，同一渠道上
// 不同模型、不同 key 的尝试必须合计计数，否则换个模型名就能绕过上限。
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
// 并发位与 RPM 窗口共用一把锁：两者必须在**同一次临界区**里判定并同时占上，否则会
// 出现「并发位占到了、RPM 名额没占到」这种半占状态，而那正是这次要修的绕过上限的
// 形态。锁的粒度是单渠道，热路径上就是一次加解锁，不碰任何 I/O。
type channelGate struct {
	mu sync.Mutex

	// inFlight 是当前占住并发位的在途尝试数（release 时归还）。
	inFlight int64

	// waiters 是此刻排在这条渠道队里的请求数（跨 key、跨模型合计）。由
	// ChannelGateWaitAnyOpen 占/还：排队人数达到渠道的 QueueSize 后新来的请求立即被拒。
	waiters int

	// reqWindow / reqBucketSec 是 60 个每秒桶的滑动窗口，口径与 telemetry.go 里
	// runtimeEntry 的 reqWindow/reqBucketSec 一致：reqBucketSec 记每个槽位当前代表
	// 哪一秒，过期槽位直接当空槽，不会把上一分钟的计数重复算进来。
	// RPM 名额是**消耗品**：占上就不归还（它是速率，不是并发位），只能等窗口自己
	// 滑过去。
	reqWindow    [60]int32
	reqBucketSec [60]int64
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

// recordRequestLocked 把一个上游尝试记进最近 60 秒的窗口。调用者必须持有 g.mu。
func (g *channelGate) recordRequestLocked(now time.Time) {
	sec := now.Unix()
	idx := sec % 60
	if g.reqBucketSec[idx] != sec {
		g.reqBucketSec[idx] = sec
		g.reqWindow[idx] = 0
	}
	g.reqWindow[idx]++
}

// recentRequestCountLocked 汇总最近 60 秒内已经占掉的 RPM 名额。
// 调用者必须持有 g.mu。
func (g *channelGate) recentRequestCountLocked(now time.Time) int64 {
	cutoff := now.Unix() - 59
	var total int64
	for i := 0; i < 60; i++ {
		if g.reqBucketSec[i] >= cutoff {
			total += int64(g.reqWindow[i])
		}
	}
	return total
}

// tryAcquire 原子地同时占住并发位与 RPM 名额：要么两个维度都占上，要么一个都不占。
// 失败返回 nil + 具体是哪个维度满了。成功返回的 release 幂等，可安全多次调用。
func (g *channelGate) tryAcquire(maxConcurrent, rpmLimit int, now time.Time) (func(), string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if maxConcurrent > 0 && g.inFlight >= int64(maxConcurrent) {
		return nil, ChannelGateReasonConcurrency
	}
	if rpmLimit > 0 && g.recentRequestCountLocked(now) >= int64(rpmLimit) {
		return nil, ChannelGateReasonRPM
	}

	// 两个维度都还有余量：在同一次持锁里一起占上，中间态不存在。
	g.inFlight++
	g.recordRequestLocked(now)

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
	if rpmLimit > 0 && g.recentRequestCountLocked(now) >= int64(rpmLimit) {
		return false
	}
	return true
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

// ChannelGateHasRoom 只读探测：该渠道此刻是否还占得到「并发位 + RPM 名额」。不占位、
// 不等待、不碰任何计数——调用方拿它决定「要不要为整轮被拒的候选再跑一轮」，真正的
// 占用发生在重跑候选的 AdmitChannelGate 里。
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
