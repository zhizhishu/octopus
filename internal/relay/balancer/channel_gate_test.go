package balancer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 渠道级硬限流闸门（channel_gate.go）的单元测试：tryAcquire 原子性、release 幂等、
// RPM 滑动窗口（名额是消耗品）、只读探测（HasRoom）不占位、整轮排队探测
//（WaitAnyOpen：按渠道排队人数上限、25ms 一拍、任一空位立刻成功、取消立刻停、
// 每个候选只等自己的 QueueTimeoutMs）、以及「不占位」渠道（0/0）行为不变。
//
// 用例之间必须 ResetChannelGates：闸门状态是进程全局。

// channelGateWaiterCount 读一个渠道当前的排队人数（持锁读，与生产路径同一口径）。
func channelGateWaiterCount(channelID int) int {
	gate := channelGateFor(channelID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.waiters
}

// waitForChannelGateWaiters 等这个渠道的排队人数变成 want（并发用例靠它拿到确定时序）。
func waitForChannelGateWaiters(t *testing.T, channelID, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if channelGateWaiterCount(channelID) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("channel %d waiters = %d, want %d (timed out waiting for the queue state)", channelID, channelGateWaiterCount(channelID), want)
}

// TestChannelGateQueueTimeoutDefaultIsThreeSeconds 钉死文档口径：渠道没配排队时长时，
// 每个请求最多排 3 秒。
func TestChannelGateQueueTimeoutDefaultIsThreeSeconds(t *testing.T) {
	if ChannelGateQueueTimeoutDefault != 3*time.Second {
		t.Fatalf("ChannelGateQueueTimeoutDefault = %v, want 3s (排队默认最多 3 秒的文档口径)", ChannelGateQueueTimeoutDefault)
	}
}

// TestChannelGateQueueBudgetFollowsCandidates：整轮排队预算取候选自己的配置——
// QueueTimeoutMs=0 用默认，最大值胜出；没有能排队的候选（QueueSize=0）就是 0。
func TestChannelGateQueueBudgetFollowsCandidates(t *testing.T) {
	if got := ChannelGateQueueBudget(nil); got != 0 {
		t.Fatalf("no candidates must yield a zero budget, got %v", got)
	}
	if got := ChannelGateQueueBudget([]GateCandidate{{ChannelID: 7801, MaxConcurrent: 2, RPMLimit: 0}}); got != 0 {
		t.Fatalf("QueueSize=0 (不排队) must yield a zero budget, got %v", got)
	}
	if got := ChannelGateQueueBudget([]GateCandidate{{ChannelID: 7802, MaxConcurrent: 2, QueueSize: 4}}); got != 3*time.Second {
		t.Fatalf("QueueTimeoutMs=0 must fall back to the 3s default, got %v", got)
	}
	got := ChannelGateQueueBudget([]GateCandidate{
		{ChannelID: 7803, MaxConcurrent: 2, QueueSize: 4, QueueTimeoutMs: 400},
		{ChannelID: 7804, MaxConcurrent: 2, QueueSize: 4, QueueTimeoutMs: 0},
		{ChannelID: 7805, MaxConcurrent: 2, QueueSize: 0, QueueTimeoutMs: 9000},
	})
	if got != 3*time.Second {
		t.Fatalf("budget must be the widest *queue-enabled* candidate (default 3s), got %v", got)
	}
}

// TestChannelGateHardCapsConcurrentAdmits：并发位是硬上限——maxConcurrent=2 时，任意
// 多的并发 admit 同时在途的不得超过 2（占满立刻拒绝，不排队）；释放后全部归还。
func TestChannelGateHardCapsConcurrentAdmits(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const (
		channelID = 7700
		gateCap   = 2
	)

	var current, maxObserved int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, reason := AdmitChannelGate(context.Background(), channelID, gateCap, 0, 0, nil)
			if reason != "" {
				return // 占不到位：调用方立刻跳下一个候选（本测试只验峰值不越限）。
			}
			n := atomic.AddInt64(&current, 1)
			for {
				m := atomic.LoadInt64(&maxObserved)
				if n <= m || atomic.CompareAndSwapInt64(&maxObserved, m, n) {
					break
				}
			}
			time.Sleep(4 * time.Millisecond) // 撑住一小段在途窗口，制造重叠
			atomic.AddInt64(&current, -1)
			release()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&current); got != 0 {
		t.Fatalf("all attempts must finish with every slot released, inFlight = %d", got)
	}
	if got := atomic.LoadInt64(&maxObserved); got > gateCap {
		t.Fatalf("observed concurrency %d exceeds hard gateCap %d", got, gateCap)
	}
	if !ChannelGateHasRoom(channelID, gateCap, 0) {
		t.Fatalf("after full release the channel must have room again")
	}
}

// TestChannelGateRPMIsConsumableNotReturned：RPM 名额是速率不是并发位——释放并发位
// 不归还 RPM 名额，只能等窗口自己滑过去。
func TestChannelGateRPMIsConsumableNotReturned(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 7701
	var releases []func()
	for i := 0; i < 3; i++ {
		release, reason := AdmitChannelGate(context.Background(), channelID, 0, 3, 0, nil)
		if reason != "" {
			t.Fatalf("admit %d should succeed under rpm=3, got %q", i, reason)
		}
		releases = append(releases, release)
	}
	// 全部释放（并发位归零）后，同一 60s 窗口内的第 4 个 admit 仍被拒：名额不随 release 归还。
	for _, release := range releases {
		release()
	}
	if _, reason := AdmitChannelGate(context.Background(), channelID, 0, 3, 0, nil); reason != ChannelGateReasonRPM {
		t.Fatalf("4th admit after releases must be rejected as rpm limit, got %q", reason)
	}
}

// TestChannelGateRPMWindowSlides：60 桶滑动窗口——恰好在 59 秒宽限之外（now-60 < cutoff）
// 的记录不再计数。真实口径用现状时间戳；这里直接用注入的历史秒验证「滑出去 = 不计入」，
// 免去等满 60 秒。
func TestChannelGateRPMWindowSlides(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	gate := channelGateFor(7702)
	past := time.Now().Add(-60 * time.Second)
	gate.mu.Lock()
	gate.recordRequestLocked(past)
	count := gate.recentRequestCountLocked(time.Now())
	gate.mu.Unlock()
	if count != 0 {
		t.Fatalf("a request recorded 60s ago must have slid out of the window, got %d", count)
	}
}

// TestChannelGateHasRoomIsReadOnly：HasRoom 只读探测——不占位、不等待、不碰任何计数。
// 探测多少遍都不能把下一个真实 admit 的名额吃掉。
func TestChannelGateHasRoomIsReadOnly(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 7703
	release, reason := AdmitChannelGate(context.Background(), channelID, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("first admit should succeed, got %q", reason)
	}
	if ChannelGateHasRoom(channelID, 1, 0) {
		t.Fatalf("concurrency is full (1/1), HasRoom must report no room")
	}
	release()
	if !ChannelGateHasRoom(channelID, 1, 0) {
		t.Fatalf("after release HasRoom must be true")
	}
	// 连续探测不占位：探测之后紧接的真实 admit 仍成功。
	if !ChannelGateHasRoom(channelID, 1, 0) {
		t.Fatalf("HasRoom must not consume a slot")
	}
	release2, reason2 := AdmitChannelGate(context.Background(), channelID, 1, 0, 0, nil)
	if reason2 != "" {
		t.Fatalf("real admit after read-only probes must succeed, got %q", reason2)
	}
	release2()
}

// TestChannelGateUnlimitedNeverCreatesState：MaxConcurrent=0 且 RPMLimit=0 的渠道行为与
// 没有闸门完全一致——admit 直接放行（幂等空释放）、HasRoom 恒真。
func TestChannelGateUnlimitedNeverCreatesState(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 7704
	for i := 0; i < 8; i++ {
		release, reason := AdmitChannelGate(context.Background(), channelID, 0, 0, 0, nil)
		if reason != "" {
			t.Fatalf("unlimited channel admit %d must not be rejected, got %q", i, reason)
		}
		release()
		if !ChannelGateHasRoom(channelID, 0, 0) {
			t.Fatalf("unlimited channel must always report room")
		}
	}
}

// TestChannelGateAdmitIsAtomicAcrossDimensions：并发位与 RPM 名额必须在同一次持锁临界区
// 里判定并同时占上——要么两个维度一起占，要么一个都不占（不存在「并发位占了、RPM 没
// 占」的半占状态，那正是这次要修的绕过上限的形态）。
func TestChannelGateAdmitIsAtomicAcrossDimensions(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 7705
	// 并发=1、RPM=1：占住唯一并发位后，下一次 admit 因为 RPM 满被拒（两个维度全满）。
	release, reason := AdmitChannelGate(context.Background(), channelID, 1, 1, 0, nil)
	if reason != "" {
		t.Fatalf("first admit should succeed, got %q", reason)
	}
	if ChannelGateHasRoom(channelID, 1, 1) {
		t.Fatalf("both dimensions are full (1/1 + 1/1 rpm), HasRoom must be false")
	}
	if _, reason := AdmitChannelGate(context.Background(), channelID, 1, 1, 0, nil); reason == "" {
		t.Fatalf("second admit must be rejected (rpm dimension full)")
	}
	release()
	// 归还后并发位空了，但 RPM 名额是速率、60 秒内不退：同一渠道仍被 RPM 拒，
	// 且只看并发维度时已有空位。
	if _, reason3 := AdmitChannelGate(context.Background(), channelID, 1, 1, 0, nil); reason3 != "channel_rpm_limit" {
		t.Fatalf("after release the rpm quota is still spent, want channel_rpm_limit, got %q", reason3)
	}
	if !ChannelGateHasRoom(channelID, 1, 0) {
		t.Fatalf("after release the concurrency slot must be free again")
	}
}

// TestChannelGateWaitAnyOpenBudgetExhausts：整轮排队探测的预算用尽必须返回 false，
// 总时长≈预算（候选逐个各排一份正是这次要修错的旧语义）。
func TestChannelGateWaitAnyOpenBudgetExhausts(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const (
		ch1 = 7706
		ch2 = 7707
	)
	// 两个候选都占满（并发=1）。
	release1, reason1 := AdmitChannelGate(context.Background(), ch1, 1, 0, 0, nil)
	if reason1 != "" {
		t.Fatalf("occupy ch1: %q", reason1)
	}
	defer release1()
	release2, reason2 := AdmitChannelGate(context.Background(), ch2, 1, 0, 0, nil)
	if reason2 != "" {
		t.Fatalf("occupy ch2: %q", reason2)
	}
	defer release2()

	candidates := []GateCandidate{
		{ChannelID: ch1, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 150},
		{ChannelID: ch2, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 150},
	}
	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil) {
		t.Fatalf("no slot frees within the budget, must return false")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("budget-wait elapsed %v, want ≈ budget(150ms) — never per-candidate re-queues", elapsed)
	}
	// 超时退出同样要把排队人数还干净（否则这条渠道永远少一个位置）。
	if got := channelGateWaiterCount(ch1); got != 0 {
		t.Fatalf("ch1 waiters after timeout = %d, want 0", got)
	}
	if got := channelGateWaiterCount(ch2); got != 0 {
		t.Fatalf("ch2 waiters after timeout = %d, want 0", got)
	}
}

// TestChannelGateWaitAnyOpenReturnsOnFirstRoom：任一候选有空位就立刻成功，绝不把整段
// 预算烧完；只读探测不停留在任何渠道上（空闲渠道仍可被真实 admit）。
func TestChannelGateWaitAnyOpenReturnsOnFirstRoom(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const (
		chBusy = 7708
		chFree = 7709
	)
	releaseBusy, reason := AdmitChannelGate(context.Background(), chBusy, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy busy channel: %q", reason)
	}
	defer releaseBusy()

	candidates := []GateCandidate{
		{ChannelID: chBusy, MaxConcurrent: 1, QueueSize: 1},
		{ChannelID: chFree, MaxConcurrent: 1, QueueSize: 1},
	}
	start := time.Now()
	if !ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil) {
		t.Fatalf("the free candidate must be observed immediately")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("first-room must return without waiting, elapsed %v", elapsed)
	}
	// 成功返回也要把排队人数还干净（占用只发生在等待期间）。
	if got := channelGateWaiterCount(chBusy); got != 0 {
		t.Fatalf("busy channel waiters after success = %d, want 0", got)
	}
	if got := channelGateWaiterCount(chFree); got != 0 {
		t.Fatalf("free channel waiters after success = %d, want 0", got)
	}
	// 只读探测不停留：空闲渠道仍可被真实 admit。
	releaseFree, reasonFree := AdmitChannelGate(context.Background(), chFree, 1, 0, 0, nil)
	if reasonFree != "" {
		t.Fatalf("real admit on the free channel must succeed, got %q", reasonFree)
	}
	releaseFree()
}

// TestChannelGateWaitAnyOpenStopsOnClientGone：clientGone（含 ctx 取消）必须立刻停——
// 绝不把一个已经走掉的调用端继续排在队里。
func TestChannelGateWaitAnyOpenStopsOnClientGone(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7710
	release, _ := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	defer release()

	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), []GateCandidate{{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 5000}}, 5*time.Second, func() bool { return true }) {
		t.Fatalf("client gone must return false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("clientGone must stop the wait immediately, elapsed %v", elapsed)
	}
	if got := channelGateWaiterCount(ch); got != 0 {
		t.Fatalf("client-gone exit must release the queue slot, waiters = %d", got)
	}
}

// TestChannelGateWaitAnyOpenStopsOnContextCancel：排队期间 ctx 被取消必须立刻停
// （cancel 会同时被 sleep 一拍与 clientGone 探测读出）。
func TestChannelGateWaitAnyOpenStopsOnContextCancel(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7711
	release, _ := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	if ChannelGateWaitAnyOpen(ctx, []GateCandidate{{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 5000}}, 5*time.Second, nil) {
		t.Fatalf("canceled context must return false")
	}
	if got := channelGateWaiterCount(ch); got != 0 {
		t.Fatalf("canceled wait must release the queue slot, waiters = %d", got)
	}
}

// —— 排队可调（Channel.QueueSize / QueueTimeoutMs）的基本口径 ——
//
// 排队是按渠道的等待名额：整轮候选全被拒时只排一次，人数达到 QueueSize 立刻拒，
// QueueSize=0 不排，排队结束/超时/取消都要把人数还回去。

// TestChannelGateQueueSizeZeroNeverQueues：QueueSize=0（不排队）时一个请求也不许排：
// 立刻返回 false，排队人数始终 0。
func TestChannelGateQueueSizeZeroNeverQueues(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7712
	release, reason := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel: %q", reason)
	}
	defer release()

	candidates := []GateCandidate{{ChannelID: ch, MaxConcurrent: 1, QueueSize: 0, QueueTimeoutMs: 5000}}
	if budget := ChannelGateQueueBudget(candidates); budget != 0 {
		t.Fatalf("QueueSize=0 must contribute no budget, got %v", budget)
	}
	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil) {
		t.Fatalf("QueueSize=0 must never queue")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("QueueSize=0 must not wait at all, elapsed %v", elapsed)
	}
	if got := channelGateWaiterCount(ch); got != 0 {
		t.Fatalf("QueueSize=0 must not reserve a queue slot, waiters = %d", got)
	}
}

// TestChannelGateQueueFullRejectsImmediately：QueueSize=1 且已经有一个请求在排队时，
// 第二个请求必须立刻被拒（队满不排），而不是也挤进队列里各等一份超时。
func TestChannelGateQueueFullRejectsImmediately(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7713
	release, reason := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel: %q", reason)
	}
	defer release()

	candidates := []GateCandidate{{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 1200}}
	firstDone := make(chan bool, 1)
	go func() {
		firstDone <- ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil)
	}()
	waitForChannelGateWaiters(t, ch, 1)

	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil) {
		t.Fatalf("a full queue must be rejected while the slot is still busy")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("a full queue must be rejected immediately (not queued behind the first waiter), elapsed %v", elapsed)
	}
	if got := channelGateWaiterCount(ch); got != 1 {
		t.Fatalf("the rejected request must not add a queue slot, waiters = %d", got)
	}

	// 第一个排队者把自己的配置等满后放弃，并把排队位还干净。
	select {
	case got := <-firstDone:
		if got {
			t.Fatalf("the first waiter must time out because the slot never frees")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the first waiter must return after its own QueueTimeoutMs")
	}
	waitForChannelGateWaiters(t, ch, 0)
}

// TestChannelGateQueueDuplicateCandidatesTakeOneSlot：同一渠道在一轮候选里出现多次时
// 只占一个排队位，否则 QueueSize 会被自己的重复候选提前吃满。
func TestChannelGateQueueDuplicateCandidatesTakeOneSlot(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7714
	release, reason := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel: %q", reason)
	}
	defer release()

	duplicated := []GateCandidate{
		{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 600},
		{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 600},
		{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 600},
	}
	done := make(chan bool, 1)
	go func() {
		done <- ChannelGateWaitAnyOpen(context.Background(), duplicated, ChannelGateQueueBudget(duplicated), nil)
	}()
	waitForChannelGateWaiters(t, ch, 1)
	// 排队人数只能是 1：三个重复候选不能变成 3。
	if got := channelGateWaiterCount(ch); got != 1 {
		t.Fatalf("duplicate candidates must share one queue slot, waiters = %d", got)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the duplicated-candidate wait must end with its own timeout")
	}
	waitForChannelGateWaiters(t, ch, 0)
}

// TestChannelGateQueueWakesWhenSlotOpens：排到空位就立刻成功，并把排队位还掉。
func TestChannelGateQueueWakesWhenSlotOpens(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const ch = 7715
	release, reason := AdmitChannelGate(context.Background(), ch, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel: %q", reason)
	}
	defer release()
	time.AfterFunc(150*time.Millisecond, release)

	candidates := []GateCandidate{{ChannelID: ch, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 2000}}
	start := time.Now()
	if !ChannelGateWaitAnyOpen(context.Background(), candidates, ChannelGateQueueBudget(candidates), nil) {
		t.Fatalf("the waiter must succeed once the slot frees within its QueueTimeoutMs")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("the waiter must wake up right after the slot frees (150ms), elapsed %v", elapsed)
	}
	if got := channelGateWaiterCount(ch); got != 0 {
		t.Fatalf("a successful wait must release the queue slot, waiters = %d", got)
	}
}

// TestChannelGateQueueHonoursPerCandidateTimeout：每个候选只等自己的 QueueTimeoutMs，
// 短配置的候选先退出并归还排队位（不被长配置的邻居拖住），整轮预算仍是最大值。
func TestChannelGateQueueHonoursPerCandidateTimeout(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const (
		shortCh = 7716
		longCh  = 7717
	)
	releaseShort, reason := AdmitChannelGate(context.Background(), shortCh, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy short channel: %q", reason)
	}
	defer releaseShort()
	releaseLong, reason := AdmitChannelGate(context.Background(), longCh, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy long channel: %q", reason)
	}
	defer releaseLong()

	candidates := []GateCandidate{
		{ChannelID: shortCh, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 120},
		{ChannelID: longCh, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 500},
	}
	budget := ChannelGateQueueBudget(candidates)
	if budget != 500*time.Millisecond {
		t.Fatalf("round budget = %v, want the widest candidate (500ms)", budget)
	}

	done := make(chan bool, 1)
	go func() {
		done <- ChannelGateWaitAnyOpen(context.Background(), candidates, budget, nil)
	}()
	waitForChannelGateWaiters(t, shortCh, 1)
	waitForChannelGateWaiters(t, longCh, 1)

	// 过了短配置的 120ms（但还没到长配置的 500ms）：短渠道的排队位必须已经还回去，
	// 长渠道还在等——这正是「每个候选自己的等待不要超过它的配置」的可观测形态。
	time.Sleep(260 * time.Millisecond)
	if got := channelGateWaiterCount(shortCh); got != 0 {
		t.Fatalf("the 120ms candidate must have left the queue by now, waiters = %d", got)
	}
	if got := channelGateWaiterCount(longCh); got != 1 {
		t.Fatalf("the 500ms candidate must still be waiting, waiters = %d", got)
	}

	start := time.Now()
	select {
	case got := <-done:
		if got {
			t.Fatalf("no slot freed, the wait must end with false")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the wait must end when the widest candidate runs out of its own budget")
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("the wait must end at the widest budget (500ms total), extra elapsed %v", elapsed)
	}
	waitForChannelGateWaiters(t, longCh, 0)
}

// TestChannelGateQueueMixedCandidatesOnlyQueueTheConfiguredOne：一轮里既有不排队的
// 渠道（QueueSize=0）也有配了排队的渠道时，只有后者占排队位、参与等待。
func TestChannelGateQueueMixedCandidatesOnlyQueueTheConfiguredOne(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const (
		noQueueCh = 7718
		queueCh   = 7719
	)
	releaseNoQueue, reason := AdmitChannelGate(context.Background(), noQueueCh, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel without queue: %q", reason)
	}
	defer releaseNoQueue()
	releaseQueue, reason := AdmitChannelGate(context.Background(), queueCh, 1, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy channel with queue: %q", reason)
	}
	defer releaseQueue()

	candidates := []GateCandidate{
		{ChannelID: noQueueCh, MaxConcurrent: 1, QueueSize: 0, QueueTimeoutMs: 9000},
		{ChannelID: queueCh, MaxConcurrent: 1, QueueSize: 1, QueueTimeoutMs: 150},
	}
	budget := ChannelGateQueueBudget(candidates)
	if budget != 150*time.Millisecond {
		t.Fatalf("budget = %v, want only the queue-enabled candidate's 150ms", budget)
	}
	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), candidates, budget, nil) {
		t.Fatalf("nobody frees a slot, the wait must end with false")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 800*time.Millisecond {
		t.Fatalf("the round must wait the queue-enabled candidate's own 150ms, elapsed %v", elapsed)
	}
	if got := channelGateWaiterCount(noQueueCh); got != 0 {
		t.Fatalf("a QueueSize=0 candidate must never take a queue slot, waiters = %d", got)
	}
	if got := channelGateWaiterCount(queueCh); got != 0 {
		t.Fatalf("the queue-enabled candidate must release its slot on timeout, waiters = %d", got)
	}
}
