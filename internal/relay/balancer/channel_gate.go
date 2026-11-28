package balancer

import (
	"context"
	"sync"
	"time"
)

// 渠道级硬限流闸门（channel hard gate）。
//
// RPM 口径（feat/rpm-tpm，用户 2026-11-28 拍板）：**upstream attempt RPM**——
// 任何真实发往上游的尝试，不论成败/超时，都在发送前原子预占名额；发送前就失败
// 的可释放（refund），字节已出门不释放。救援重试、竞速 contender、手动重试、
// probe、渠道测试全走同一闸、同一计数点。窗口是**滑动 60 秒**（不再是整分钟桶），
// 实现借自 claude-code-hub-learning src/lib/rate-limit/service.ts:1461-1514 的
// 滑动窗口记账（zremrangebyscore 清 60 秒外 + zcard 计数 + zadd 记当次），落地为
// 单机内存 ring；他们是 Redis 故障 fail-open，我们内存实现无此故障域，语义 fail-closed。
//
// TPM 口径（同批新增，第一档）：渠道级 tokens-per-minute 硬闸门，按
// 「本地估算输入 tokens + 已观测输出 tokens」在同一滑动 60 秒窗口里计数。
// 输入在准入时按入站请求体字节数估算预占（len/4，仓内既有估算口径，见
// modelverify/fingerprint/detector.go:151 与 feature.go:122 的 len/4 近似；
// tier-1 接受估算误差，精确的 tiktoken 输入计数是第二档增强）；失败没 usage 的
// 请求其估算输入已在准入时计入，不再退还（字节已出门）；输出 tokens 在响应带回
// usage 后追加（RecordChannelTPMOutput）。
//
// 并发位（inFlight）仍是硬预占：占不到就不发，释放才归还。排队语义不变。
const (
	// ChannelGateQueueTimeoutDefault 是渠道没配排队时长（QueueTimeoutMs=0）时每个请求的
	// 默认等待上限。
	ChannelGateQueueTimeoutDefault = 3 * time.Second

	// ChannelGateReasonConcurrency / ChannelGateReasonRPM 是闸门拒绝的维度标签。
	// 它们会进 Iterator.Skip 的审计消息（AttemptSkipped），让日志能回答「这次到底
	// 是并发位满了还是 RPM 名额满了」。
	ChannelGateReasonConcurrency = "channel_concurrency_limit"
	ChannelGateReasonRPM         = "channel_rpm_limit"
	ChannelGateReasonTPM         = "channel_tpm_limit"
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
	// seq 是闸门内单调递增的标记序号：预占的 RPM/TPM 名额凭 seq 在未封口时精确退还。
	seq int64

	// rpmWindow 是滑动 60 秒的上游尝试时间戳 ring（service.ts zadd/zcard 的内存版）：
	// 准入即预占追加；未封口释放时按 seq 摘除（发送前失败可释放）；封口后永驻到自然滑出。
	rpmWindow []rpmMark

	// tpmWindow 是滑动 60 秒的 token 计量 ring：准入时追加估算输入标记（可退），
	// 响应回来后按真实 usage 追加输出标记（RecordChannelTPMOutput，不可退）。
	tpmWindow []tpmMark
}

// rpmMark / tpmMark 是滑动窗口里的一条计量记录。seq 用于精确摘除（未封口退款）。
type rpmMark struct {
	seq int64
	at  int64 // unix nano
}

type tpmMark struct {
	seq    int64
	at     int64 // unix nano
	tokens int64
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

// channelGateWindowNanos 是 RPM/TPM 共用的滑动窗口长度：60 秒（service.ts 的 60000ms）。
const channelGateWindowNanos = int64(60 * time.Second)

// pruneWindowsLocked 把两条窗口里滑出 60 秒的记录从头摘掉。记录按时间顺序追加，
// 从头 prune 即可（service.ts zremrangebyscore(key, "-inf", oneMinuteAgo) 的内存版）。
// 调用者必须持有 g.mu。
func (g *channelGate) pruneWindowsLocked(now time.Time) {
	cutoff := now.UnixNano() - channelGateWindowNanos
	trim := 0
	for trim < len(g.rpmWindow) && g.rpmWindow[trim].at <= cutoff {
		trim++
	}
	if trim > 0 {
		g.rpmWindow = append([]rpmMark(nil), g.rpmWindow[trim:]...)
	}
	trim = 0
	for trim < len(g.tpmWindow) && g.tpmWindow[trim].at <= cutoff {
		trim++
	}
	if trim > 0 {
		g.tpmWindow = append([]tpmMark(nil), g.tpmWindow[trim:]...)
	}
}

// tpmSumLocked 是窗口内 token 总量（估算输入+观测输出混合口径，见文件头注释）。
// 调用者必须持有 g.mu，且 pruneWindowsLocked 已先跑过。
func (g *channelGate) tpmSumLocked() int64 {
	var sum int64
	for _, mark := range g.tpmWindow {
		sum += mark.tokens
	}
	return sum
}

// GateAdmission 是一次准入预占的句柄：并发位 + RPM 名额 + TPM 估算输入名额。
//
// 两条规则（方案 7.2，用户拍板）：
//   - **字节已出门不释放**：调用方在真实发往上游前必须 Seal()；封口后 Release 只还
//     并发位，RPM/TPM 名额永驻窗口直到自然滑出。
//   - **发送前就失败可释放**：没走到 Seal 就 Release 的，RPM/TPM 预占按 seq 精确退还。
type GateAdmission struct {
	gate     *channelGate
	rpmSeq   int64
	tpmSeq   int64
	sealed   bool
	released bool
}

// Seal 标记「字节已出门」：此后 Release 不再退还 RPM/TPM 名额。幂等。
func (a *GateAdmission) Seal() {
	if a == nil || a.gate == nil {
		return
	}
	a.gate.mu.Lock()
	a.sealed = true
	a.gate.mu.Unlock()
}

// Release 归还并发位；未封口时一并退还 RPM/TPM 预占。幂等，nil 安全。
func (a *GateAdmission) Release() {
	if a == nil || a.gate == nil {
		return
	}
	a.gate.mu.Lock()
	defer a.gate.mu.Unlock()
	if a.released {
		return
	}
	a.released = true
	if a.gate.inFlight > 0 {
		a.gate.inFlight--
	}
	if a.sealed {
		return
	}
	for i, mark := range a.gate.rpmWindow {
		if mark.seq == a.rpmSeq {
			a.gate.rpmWindow = append(a.gate.rpmWindow[:i], a.gate.rpmWindow[i+1:]...)
			break
		}
	}
	for i, mark := range a.gate.tpmWindow {
		if mark.seq == a.tpmSeq {
			a.gate.tpmWindow = append(a.gate.tpmWindow[:i], a.gate.tpmWindow[i+1:]...)
			break
		}
	}
}

// tryAcquire 在**同一次持锁**里判定三个维度并原子预占：并发位满了、滑动窗口内
// 尝试数到 RPM 上限、或窗口内 token 量到 TPM 上限就拒，什么都不占。成功则并发位+1、
// RPM 窗口追加当次尝试、TPM 窗口追加 estInputTokens 估算输入标记。
//
// 计数与限额解耦：只要 channelID 有效，RPM/TPM 窗口**永远记账**（渠道页实时值、
// 审计口径都靠它），上限只在配了 >0 时用于拦截。
func (g *channelGate) tryAcquire(maxConcurrent, rpmLimit, tpmLimit int, estInputTokens int64, now time.Time) (*GateAdmission, string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.pruneWindowsLocked(now)
	if maxConcurrent > 0 && g.inFlight >= int64(maxConcurrent) {
		return nil, ChannelGateReasonConcurrency
	}
	if rpmLimit > 0 && int64(len(g.rpmWindow)) >= int64(rpmLimit) {
		return nil, ChannelGateReasonRPM
	}
	if tpmLimit > 0 && g.tpmSumLocked() >= int64(tpmLimit) {
		return nil, ChannelGateReasonTPM
	}

	g.inFlight++
	g.seq++
	rpmSeq := g.seq
	g.rpmWindow = append(g.rpmWindow, rpmMark{seq: rpmSeq, at: now.UnixNano()})
	var tpmSeq int64
	if estInputTokens > 0 {
		g.seq++
		tpmSeq = g.seq
		g.tpmWindow = append(g.tpmWindow, tpmMark{seq: tpmSeq, at: now.UnixNano(), tokens: estInputTokens})
	}
	return &GateAdmission{gate: g, rpmSeq: rpmSeq, tpmSeq: tpmSeq}, ""
}

// AdmitChannelGate 尝试为 channelID 原子预占「并发位 + RPM 滑动窗口名额 + TPM 估算输入名额」。
//
// maxConcurrent/rpmLimit/tpmLimit <=0 分别表示该维度不限。estInputTokens 是本次尝试的
// 本地估算输入 tokens（调用方按入站请求体估算，len/4 口径）；只在 TPM 记账用。
// 成功返回非 nil 的 *GateAdmission——调用方必须在真实发往上游前 Seal()（字节出门，
// 名额不退），并 defer Release()（收尾还并发位；未封口时一并退 RPM/TPM 预占）。
// 失败返回 nil + reason（并发满 / RPM 满 / TPM 满）。
//
// 排队等待语义不变（maxWait>0 时轮询到 deadline，每拍重查 ctx/clientGone）。
func AdmitChannelGate(ctx context.Context, channelID int, maxConcurrent, rpmLimit, tpmLimit int, estInputTokens int64, maxWait time.Duration, clientGone func() bool) (*GateAdmission, string) {
	if channelID <= 0 {
		return &GateAdmission{}, ""
	}

	gate := channelGateFor(channelID)
	var deadline time.Time
	if maxWait > 0 {
		deadline = time.Now().Add(maxWait)
	}

	var lastReason string
	for {
		admission, why := gate.tryAcquire(maxConcurrent, rpmLimit, tpmLimit, estInputTokens, time.Now())
		if why == "" {
			return admission, ""
		}
		lastReason = why

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
	TPMLimit       int
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
func (g *channelGate) hasRoomLocked(maxConcurrent, rpmLimit, tpmLimit int, now time.Time) bool {
	g.pruneWindowsLocked(now)
	if maxConcurrent > 0 && g.inFlight >= int64(maxConcurrent) {
		return false
	}
	if rpmLimit > 0 && int64(len(g.rpmWindow)) >= int64(rpmLimit) {
		return false
	}
	if tpmLimit > 0 && g.tpmSumLocked() >= int64(tpmLimit) {
		return false
	}
	return true
}

// RecordChannelTPMOutput 把一次响应里**已观测的输出 tokens** 追加进渠道 TPM 滑动窗口。
// 输出 tokens 只在响应带回 usage 后才知道（方案 7.4 第一档「已观测输出」），
// 此刻字节早已出门，不存在退还语义，所以不进预占/退款体系，直接记账。
// 成功/失败只要有 usage 都该记：失败没 usage 的则只有准入时的估算输入在账上（用户拍板口径）。
func RecordChannelTPMOutput(channelID int, outputTokens int64) {
	if channelID <= 0 || outputTokens <= 0 {
		return
	}
	gate := channelGateFor(channelID)
	gate.mu.Lock()
	gate.pruneWindowsLocked(time.Now())
	gate.seq++
	gate.tpmWindow = append(gate.tpmWindow, tpmMark{seq: gate.seq, at: time.Now().UnixNano(), tokens: outputTokens})
	gate.mu.Unlock()
}

// ChannelGateCurrentRPM 只读返回该渠道滑动 60 秒窗口内的上游尝试数（含失败/救援/探测，
// feat/rpm-tpm 新口径），供渠道列表/编辑页显示实时值。不占位、不等待、不创建闸门状态。
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
	gate.pruneWindowsLocked(time.Now())
	return len(gate.rpmWindow)
}

// ChannelGateCurrentTPM 只读返回滑动 60 秒窗口内的 token 计量（估算输入+观测输出），
// 供渠道页与 TPM 上限对照显示。
func ChannelGateCurrentTPM(channelID int) int64 {
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
	gate.pruneWindowsLocked(time.Now())
	return gate.tpmSumLocked()
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
func ChannelGateHasRoom(channelID, maxConcurrent, rpmLimit, tpmLimit int) bool {
	if channelID <= 0 || (maxConcurrent <= 0 && rpmLimit <= 0 && tpmLimit <= 0) {
		// 闸门不接管的维度（未配置/渠道 ID 无效）：永远视为有空位。
		return true
	}
	gate := channelGateFor(channelID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.hasRoomLocked(maxConcurrent, rpmLimit, tpmLimit, time.Now())
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
			if ChannelGateHasRoom(slot.candidate.ChannelID, slot.candidate.MaxConcurrent, slot.candidate.RPMLimit, slot.candidate.TPMLimit) {
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
