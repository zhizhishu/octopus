package balancer

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// Spread (轮询) IS the load-balancing mode: within one ChannelPriority the per-item
// Priority (UI drag order) is NOT a hard boundary, so candidates rotate turn by turn
// across ALL servable channels. Crucially, a merely-slower channel is NOT demoted —
// "slow" is not "broken"; latency no longer reorders the rotation. Both a fast and a
// slow servable peer must each get turns (轮询的语义=渠道都用上). Only genuinely
// failing/unusable channels sink (see TestSpreadDemotesRecentlyFailingChannel). This
// guards against the "轮询不轮询 / 优先级一样不切渠道" collapse where the marginally
// faster channel captured every turn.
func TestSpreadRotatesAcrossLatencyAndDragOrder(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		// Same ChannelPriority; channel 1's drag order (Priority 1) is "first" and it
		// is much slower (5000ms vs 300ms) — it must still share the rotation.
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 5000}},
		{ChannelID: 2, ModelName: "m", Priority: 2, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 300}},
	}
	seen := map[int]int{}
	for i := 0; i < 6; i++ {
		seen[(&Spread{}).Candidates(items)[0].ChannelID]++
	}
	if seen[1] == 0 || seen[2] == 0 {
		t.Fatalf("both servable channels must get turns regardless of latency/drag order, got %v", seen)
	}
}

// Equally healthy, equally fast channels that share a ChannelPriority but have
// DIFFERENT drag-order Priority must still rotate. Before the fix a unique drag
// order pinned channel 1 forever; now the ChannelPriority-bucketed round-robin
// rotates them turn by turn.
func TestSpreadRotatesEquallyFastAcrossDragOrder(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1}},
		{ChannelID: 2, ModelName: "m", Priority: 2, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1}},
	}
	first := (&Spread{}).Candidates(items)[0].ChannelID
	second := (&Spread{}).Candidates(items)[0].ChannelID
	if first == second {
		t.Fatalf("equally fast channels across drag order should rotate, got %d then %d", first, second)
	}
}

// ChannelPriority stays a hard boundary under Spread: a higher-priority (smaller
// ChannelPriority) channel leads even when it is distinctly slower and dragged
// last — spread only balances WITHIN a ChannelPriority bucket, never across.
func TestSpreadChannelPriorityHardBoundaryEvenWhenSlow(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 2, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 5000}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, ChannelPriority: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 100}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 1 {
			t.Fatalf("channel priority must stay a hard boundary even when slower, got %d", got)
		}
	}
}
