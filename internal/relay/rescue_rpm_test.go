package relay

import (
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

// TestRescueRetriesConsumeRPMSlots（验收「救援重试占 RPM 名额」）：
// 一次触发救援的请求，初始上行 + 每一次救援重试都必须各吃掉一个 RPM 名额——
// 上游总派发数与滑窗 RPM 读数一一相等。
func TestRescueRetriesConsumeRPMSlots(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 3*time.Second)
	withRequestCeiling(t, 6*time.Second)
	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	upstream := newEpisodeUpstream(t, -1, "never") // always 502 → 触发救援
	channelID := newNoBreakerChannel(t, upstream.url(), "rescue-rpm", 1, true)

	res := runEpisodeRequest(t, "rescue-rpm-probe", episodeRequestGuard)
	if res.code == 200 {
		t.Fatalf("always-failing upstream must not end 200: %q", res.body)
	}
	dispatches := upstream.count()
	if dispatches < 2 {
		t.Fatalf("expected the turn to actually rescue (>=2 dispatches), got %d", dispatches)
	}
	// 每一次真实上行（含每次救援重试）都在真发前原子预占 RPM——读数必须等于派发数。
	if got := balancer.ChannelGateCurrentRPM(channelID); got != dispatches {
		t.Fatalf("sliding-window rpm = %d, want %d (every rescue retry consumes a slot)", got, dispatches)
	}
}
