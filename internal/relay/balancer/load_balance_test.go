package balancer

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// After the fix, Spread (轮询) IS the load-balancing mode: within one
// ChannelPriority the per-item Priority (UI drag order, always unique) is NOT a
// hard boundary, so a distinctly slower channel is demoted by spreadRank even
// though its drag order puts it first. Before the fix, the unique drag order
// short-circuited spreadTier/spreadRank and pinned the first-dragged channel —
// the "轮询不轮询、慢渠道不降档" bug this change fixes.
func TestSpreadDemotesSlowChannelDespiteDragOrder(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		// Same ChannelPriority; channel 1's drag order (Priority 1) is "first",
		// but it is distinctly slower (5000ms vs 300ms).
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 5000}},
		{ChannelID: 2, ModelName: "m", Priority: 2, Weight: 1, ChannelPriority: 0, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 300}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("spread must demote the slow channel regardless of drag order, got %d", got)
		}
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
