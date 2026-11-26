package balancer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 渠道级硬限流闸门（channel_gate.go）的单元测试：tryAcquire 原子性、release 幂等、
// RPM 滑动窗口（名额是消耗品，只能等窗口滑过去）、只读探测（HasRoom）不占位、
// 整轮排队探测（WaitAnyOpen：总预算 + 25ms 一拍 + 任一空位立刻成功 + clientGone/ctx
// 取消立刻停）、以及「不占位」渠道（0/0）行为不变。
//
// 用例之间必须 ResetChannelGates：闸门状态是进程全局（按 channelID 的 sync.Map），
// 残留会让后面的用例莫名其妙被拒。

// TestChannelGateWaitBudgetIsThreeSeconds 钉死文档口径：全满排队最多 3 秒。
func TestChannelGateWaitBudgetIsThreeSeconds(t *testing.T) {
	if ChannelGateWaitBudget != 3*time.Second {
		t.Fatalf("ChannelGateWaitBudget = %v, want 3s (全满排队最多 3 秒的文档口径)", ChannelGateWaitBudget)
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
	// 归还后两个维度都空，重新可占。
	release3, reason3 := AdmitChannelGate(context.Background(), channelID, 1, 1, 0, nil)
	if reason3 != "" {
		t.Fatalf("admit after full release must succeed again, got %q", reason3)
	}
	release3()
}

// TestChannelGateWaitAnyOpenBudgetExhausts：整轮排队探测的总预算用尽必须返回 false，
// 总时长≈预算（候选逐个 3s 排队正是这次要修错的旧语义）。
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
		{ChannelID: ch1, MaxConcurrent: 1},
		{ChannelID: ch2, MaxConcurrent: 1},
	}
	start := time.Now()
	if ChannelGateWaitAnyOpen(context.Background(), candidates, 150*time.Millisecond, nil) {
		t.Fatalf("no slot frees within the budget, must return false")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("budget-wait elapsed %v, want ≈ budget(150ms) — never per-candidate re-queues", elapsed)
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
		{ChannelID: chBusy, MaxConcurrent: 1},
		{ChannelID: chFree, MaxConcurrent: 1},
	}
	start := time.Now()
	if !ChannelGateWaitAnyOpen(context.Background(), candidates, 3*time.Second, nil) {
		t.Fatalf("the free candidate must be observed immediately")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("first-room must return without waiting, elapsed %v", elapsed)
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
	if ChannelGateWaitAnyOpen(context.Background(), []GateCandidate{{ChannelID: ch, MaxConcurrent: 1}}, 5*time.Second, func() bool { return true }) {
		t.Fatalf("client gone must return false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("clientGone must stop the wait immediately, elapsed %v", elapsed)
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
	if ChannelGateWaitAnyOpen(ctx, []GateCandidate{{ChannelID: ch, MaxConcurrent: 1}}, 5*time.Second, nil) {
		t.Fatalf("canceled context must return false")
	}
}
