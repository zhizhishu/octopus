package balancer

// feat/rpm-tpm 验收测试（方案 10.4/10.6 + 滑窗语义）。

import (
	"context"
	"testing"
	"time"
)

// 方案 10.4：连续失败也算 attempt——RPM=8 时滑窗内第 9 次真实上行被挡。
// 失败语义=封口后释放（字节已出门但上游没成），名额不退。
func TestChannelGateRPMFailuresConsumeSlots(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 8801
	// 8 次「失败」尝试（每次都真实上了行）：全部占名额。
	for i := 0; i < 8; i++ {
		admission, reason := AdmitChannelGate(context.Background(), channelID, 0, 8, 0, 0, 0, nil)
		if reason != "" {
			t.Fatalf("attempt %d must pass the gate, got %q", i+1, reason)
		}
		admission.Seal() // 字节出门
		admission.Release()
	}
	if got := ChannelGateCurrentRPM(channelID); got != 8 {
		t.Fatalf("8 failed attempts must all count, current rpm = %d", got)
	}
	// 第 9 次：被挡，且这次**没有**真实上行。
	if _, reason := AdmitChannelGate(context.Background(), channelID, 0, 8, 0, 0, 0, nil); reason != ChannelGateReasonRPM {
		t.Fatalf("9th attempt in window must be rejected, got %q", reason)
	}
	if got := ChannelGateCurrentRPM(channelID); got != 8 {
		t.Fatalf("rejected attempt must not count, current rpm = %d", got)
	}
}

// 未封口释放=发送前失败：名额精确退还（方案 7.2「发送前就失败的可释放」）。
func TestChannelGateUnsentFailureRefundsSlot(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 8802
	admission, _ := AdmitChannelGate(context.Background(), channelID, 0, 1, 0, 100, 0, nil)
	admission.Release() // 没 Seal：没上行，退款
	if got := ChannelGateCurrentRPM(channelID); got != 0 {
		t.Fatalf("unsent failure must refund, rpm = %d", got)
	}
	if got := ChannelGateCurrentTPM(channelID); got != 0 {
		t.Fatalf("unsent failure must refund tpm estimate, tpm = %d", got)
	}
	if _, reason := AdmitChannelGate(context.Background(), channelID, 0, 1, 0, 100, 0, nil); reason != "" {
		t.Fatalf("refunded slot must be usable again, got %q", reason)
	}
}

// 方案 10.6：失败没 usage 的请求按估算输入计数（准入预占不退）；有 usage 的成功响应追加观测输出。
func TestChannelGateTPMCountsEstimatedInputAndObservedOutput(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 8803
	// 失败（没 usage）：估算输入 1000 已在准入计入。
	admission, _ := AdmitChannelGate(context.Background(), channelID, 0, 0, 2000, 1000, 0, nil)
	admission.Seal()
	admission.Release()
	if got := ChannelGateCurrentTPM(channelID); got != 1000 {
		t.Fatalf("failed attempt must count estimated input, tpm = %d", got)
	}
	// 有 usage 的成功响应：追加观测输出 500 → 1500。
	RecordChannelTPMOutput(channelID, 500)
	if got := ChannelGateCurrentTPM(channelID); got != 1500 {
		t.Fatalf("observed output must add, tpm = %d", got)
	}
	// tpm=2000：再来一次 est=600 的尝试，窗口将到 2100——准入时总量 1500 未到顶，放行；
	// 但下一次再到 2100 后就被挡。
	admission2, reason := AdmitChannelGate(context.Background(), channelID, 0, 0, 2000, 600, 0, nil)
	if reason != "" {
		t.Fatalf("window at 1500/2000 must admit, got %q", reason)
	}
	admission2.Seal()
	admission2.Release()
	if _, reason := AdmitChannelGate(context.Background(), channelID, 0, 0, 2000, 600, 0, nil); reason != ChannelGateReasonTPM {
		t.Fatalf("window at 2100/2000 must reject with tpm reason, got %q", reason)
	}
}

// 滑动窗口：60 秒前的记录被 prune（直接测 prune 内核，不等真实一分钟）。
func TestChannelGateSlidingWindowPrunesOldEntries(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 8804
	gate := channelGateFor(channelID)
	now := time.Now()
	gate.mu.Lock()
	// 手工摆账：61 秒前 5 次、30 秒前 2 次。
	for i := 0; i < 5; i++ {
		gate.seq++
		gate.rpmWindow = append(gate.rpmWindow, rpmMark{seq: gate.seq, at: now.Add(-61 * time.Second).UnixNano()})
	}
	for i := 0; i < 2; i++ {
		gate.seq++
		gate.rpmWindow = append(gate.rpmWindow, rpmMark{seq: gate.seq, at: now.Add(-30 * time.Second).UnixNano()})
	}
	gate.pruneWindowsLocked(now)
	got := len(gate.rpmWindow)
	gate.mu.Unlock()
	if got != 2 {
		t.Fatalf("entries older than 60s must slide out, remaining = %d, want 2", got)
	}
}

// 并发位归还不受 RPM/TPM 退款影响：未封口释放退的是名额，并发位照还不误。
func TestChannelGateReleaseAlwaysReturnsConcurrency(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()

	const channelID = 8805
	admission, _ := AdmitChannelGate(context.Background(), channelID, 1, 0, 0, 0, 0, nil)
	admission.Release() // 未封口
	admission2, reason := AdmitChannelGate(context.Background(), channelID, 1, 0, 0, 0, 0, nil)
	if reason != "" {
		t.Fatalf("concurrency slot must be returned regardless of refund, got %q", reason)
	}
	admission2.Release()
}
