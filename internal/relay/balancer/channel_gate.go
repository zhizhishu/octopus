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
	// ChannelGateWaitBudget 是单次准入的排队等待预算：占不到位时最多等这么久（期间
	// 按 channelGatePollInterval 轮询），等到了就照常发送。3s 这个数是刻意选的——
	// 它远小于一次真实 429 的等待，调用方宁可先排 3s 队拿到真回复，也不愿立刻吃一个
	// 我们自己造出来的假限流错误。
	ChannelGateWaitBudget = 3 * time.Second

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

// GateCandidate 是整轮候选里被闸门拒掉的渠道（含其上限配置）：排队探测只回答「有没有
// 空位」，真正的占用发生在重跑候选时的 AdmitChannelGate。
type GateCandidate struct {
	ChannelID     int
	MaxConcurrent int
	RPMLimit      int
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

// ChannelGateWaitAnyOpen 在 candidates 任一渠道出现空位时返回 true（调用方据此重跑一轮
// 候选）。总预算 budget，每 channelGatePollInterval 一拍对整个候选集只读探测一遍；
// clientGone（含 ctx 取消）为真或预算用尽时立刻返回 false——绝不把一个已经走掉的
// 调用端继续排在队里。
func ChannelGateWaitAnyOpen(ctx context.Context, candidates []GateCandidate, budget time.Duration, clientGone func() bool) bool {
	if len(candidates) == 0 || budget <= 0 {
		return false
	}
	deadline := time.Now().Add(budget)
	for {
		for _, candidate := range candidates {
			if ChannelGateHasRoom(candidate.ChannelID, candidate.MaxConcurrent, candidate.RPMLimit) {
				return true
			}
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
