package balancer

import (
	"context"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// TestSpreadTierDemotesAtRPMCap pins the per-channel requests-per-minute cap: a
// channel at/over RPMLimit (recent request count) is demoted to the soft tier so
// bursts spread to peers with spare budget, while channels under their cap (or
// with no cap) keep their normal idle/busy tier.
func TestSpreadTierDemotesAtRPMCap(t *testing.T) {
	mk := func(rpmLimit int, recent int64) model.GroupItem {
		return model.GroupItem{RoutingStats: model.RoutingRuntimeStats{
			AvailableKeyCount: 1, HealthyKeyCount: 1,
			RPMLimit: rpmLimit, RecentRequestCount: recent,
		}}
	}

	if got := spreadTier(mk(10, 5)); got != 0 {
		t.Fatalf("under rpm cap (5/10) and idle should stay idle tier 0, got %d", got)
	}
	if got := spreadTier(mk(10, 10)); got != 2 {
		t.Fatalf("at rpm cap (10/10) should be demoted to tier 2, got %d", got)
	}
	if got := spreadTier(mk(10, 25)); got != 2 {
		t.Fatalf("over rpm cap (25/10) should be demoted to tier 2, got %d", got)
	}
	if got := spreadTier(mk(0, 999)); got != 0 {
		t.Fatalf("no rpm cap (0) must never demote on request count alone, got %d", got)
	}
}

// TestRuntimeRecentRequestCount verifies the sliding-window per-minute counter is
// fed by BeginRuntimeAttempt at the channel-level aggregate and surfaces through
// the routing snapshot, so spreadTier can compare it against RPMLimit.
func TestRuntimeRecentRequestCount(t *testing.T) {
	ResetRuntimeTelemetry()
	defer ResetRuntimeTelemetry()

	const channelID = 4321
	const modelName = "rpm-model"
	for i := 0; i < 3; i++ {
		done := BeginRuntimeAttempt(channelID, 7, modelName)
		done()
	}

	snap := SnapshotRoutingRuntime(channelID, modelName,
		[]model.ChannelKey{{ID: 7, Enabled: true, ChannelKey: "k"}}, false)
	if snap.RecentRequestCount != 3 {
		t.Fatalf("expected RecentRequestCount=3 after 3 attempts, got %d", snap.RecentRequestCount)
	}
}

// TestChannelGateRPMSuccessDoesNotFeedSoftTierCounters pins that the hard gate's
// minute-bucket success counter (channel_gate.go) and the telemetry trailing-60s
// counter (telemetry.go, feeding the spreadTier soft demote) are two independent
// counters: a forwarded success recorded for the hard gate must not silently
// inflate the soft-tier figure, and a BeginRuntimeAttempt must not spend the hard
// gate's RPM budget.
func TestChannelGateAttemptsDoNotFeedSoftTierCounters(t *testing.T) {
	ResetChannelGates()
	defer ResetChannelGates()
	ResetRuntimeTelemetry()
	defer ResetRuntimeTelemetry()

	const channelID = 4322
	const modelName = "rpm-model-split"

	// feat/rpm-tpm 新口径：计数在准入预占。两次真实准入（封口=字节出门）后，
	// 硬闸 RPM=2；这套计数绝不能喂给软降档的 telemetry 窗口。
	for range 2 {
		admission, reason := AdmitChannelGate(context.Background(), channelID, 0, 0, 0, 0, 0, nil)
		if reason != "" {
			t.Fatalf("admission must pass: %s", reason)
		}
		admission.Seal()
		admission.Release()
	}
	snap := SnapshotRoutingRuntime(channelID, modelName,
		[]model.ChannelKey{{ID: 8, Enabled: true, ChannelKey: "k"}}, false)
	if snap.RecentRequestCount != 0 {
		t.Fatalf("hard-gate attempts must not feed the telemetry window, got %d", snap.RecentRequestCount)
	}

	done := BeginRuntimeAttempt(channelID, 8, modelName)
	done()
	if got := ChannelGateCurrentRPM(channelID); got != 2 {
		t.Fatalf("an attempt start must not spend the hard gate's rpm budget, current rpm = %d", got)
	}
}
