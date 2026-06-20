package balancer

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestSpreadTierDemotesAtConcurrencyCap pins the per-channel concurrency cap: a
// channel at/over MaxConcurrent (in-flight + selection reservations) is demoted to the
// soft tier so bursts spread to peers with spare capacity, while channels under their
// cap (or with no cap) keep their normal idle/busy tier.
func TestSpreadTierDemotesAtConcurrencyCap(t *testing.T) {
	mk := func(maxConc int, inFlight, pending int64) model.GroupItem {
		return model.GroupItem{RoutingStats: model.RoutingRuntimeStats{
			AvailableKeyCount: 1, HealthyKeyCount: 1,
			MaxConcurrent: maxConc, InFlight: inFlight, PendingSelections: pending,
		}}
	}

	if got := spreadTier(mk(4, 2, 0)); got != 1 {
		t.Fatalf("under cap (2/4) should be busy tier 1, got %d", got)
	}
	if got := spreadTier(mk(4, 3, 1)); got != 2 {
		t.Fatalf("at cap (3+1/4) should be demoted to tier 2, got %d", got)
	}
	if got := spreadTier(mk(4, 5, 0)); got != 2 {
		t.Fatalf("over cap (5/4) should be demoted to tier 2, got %d", got)
	}
	if got := spreadTier(mk(0, 9, 0)); got != 1 {
		t.Fatalf("no cap (0) must never demote on load alone, got %d", got)
	}
	if got := spreadTier(mk(4, 0, 0)); got != 0 {
		t.Fatalf("idle under cap should stay idle tier 0, got %d", got)
	}
}
