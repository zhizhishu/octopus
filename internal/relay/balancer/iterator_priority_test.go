package balancer

import (
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// collectOrder drains an iterator into the ChannelID order it will attempt.
func collectOrder(it *Iterator) []int {
	order := make([]int, 0, it.Len())
	for it.Next() {
		order = append(order, it.Item().ChannelID)
	}
	return order
}

// SpreadTiered hard tier = (Priority, ChannelPriority). Both keys must match.
func TestSameRoutingTierSpreadTieredRequiresBothKeys(t *testing.T) {
	base := model.GroupItem{ChannelID: 1, ModelName: "m", Priority: 3, ChannelPriority: 4}

	if !SameRoutingTier(model.GroupModeSpreadTiered, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 3, ChannelPriority: 4}) {
		t.Fatalf("same (Priority, ChannelPriority) must be the same tier")
	}
	if SameRoutingTier(model.GroupModeSpreadTiered, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 4}) {
		t.Fatalf("a different Priority is a different tier under SpreadTiered")
	}
	if SameRoutingTier(model.GroupModeSpreadTiered, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 3, ChannelPriority: 5}) {
		t.Fatalf("a different ChannelPriority is a different tier under SpreadTiered")
	}
}

// FillFirst hard tier = (ChannelPriority, Priority). Both keys must match; the ChannelID
// deterministic tie-break is NOT a tier boundary.
func TestSameRoutingTierFillFirstRequiresBothKeys(t *testing.T) {
	base := model.GroupItem{ChannelID: 1, ModelName: "m", Priority: 7, ChannelPriority: 2}

	if !SameRoutingTier(model.GroupModeFillFirst, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 7, ChannelPriority: 2}) {
		t.Fatalf("same (ChannelPriority, Priority) must be the same tier")
	}
	if SameRoutingTier(model.GroupModeFillFirst, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 8, ChannelPriority: 2}) {
		t.Fatalf("a different Priority is a different tier under FillFirst")
	}
	if SameRoutingTier(model.GroupModeFillFirst, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 7, ChannelPriority: 3}) {
		t.Fatalf("a different ChannelPriority is a different tier under FillFirst")
	}
	// Tied on both keys but different ChannelID => still the same tier (raceable).
	if !SameRoutingTier(model.GroupModeFillFirst, base,
		model.GroupItem{ChannelID: 99, ModelName: "m", Priority: 7, ChannelPriority: 2}) {
		t.Fatalf("a ChannelID difference must not split a tier (tie-break is not a boundary)")
	}
}

// Spread (round-robin) ignores item Priority: a tier is only a ChannelPriority value.
// The same rule covers any unknown/legacy mode (default branch).
func TestSameRoutingTierSpreadIgnoresItemPriority(t *testing.T) {
	base := model.GroupItem{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 0}

	if !SameRoutingTier(model.GroupModeSpread, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 99, ChannelPriority: 0}) {
		t.Fatalf("round-robin must treat different item priorities in the same ChannelPriority as one tier")
	}
	if SameRoutingTier(model.GroupModeSpread, base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 1, ChannelPriority: 1}) {
		t.Fatalf("a different ChannelPriority is a different tier under round-robin")
	}
	if !SameRoutingTier(model.GroupMode(99), base,
		model.GroupItem{ChannelID: 2, ModelName: "m", Priority: 99, ChannelPriority: 0}) {
		t.Fatalf("unknown/legacy mode must fall back to the ChannelPriority-only rule")
	}
}

// A preferred (native) channel may only move WITHIN its own hard tier; it must not
// leapfrog a higher tier. Same-tier preference still promotes to the front of its tier.
func TestPrioritizeChannelsCannotRaiseLowerTier(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeFillFirst,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 0, ChannelPriority: 0}, // top tier
			{ChannelID: 3, ModelName: "m", Priority: 0, ChannelPriority: 0}, // top tier peer
			{ChannelID: 2, ModelName: "m", Priority: 0, ChannelPriority: 5}, // lower tier, also preferred
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)
	it.PrioritizeChannels(map[int]bool{2: true, 3: true})

	order := collectOrder(it)
	if len(order) != 3 {
		t.Fatalf("want 3 candidates, got %v", order)
	}
	if order[2] != 2 {
		t.Fatalf("lower-tier preferred channel rose above the top tier: %v", order)
	}
	if order[0] != 3 || order[1] != 1 {
		t.Fatalf("same-tier preferred channel must lead its own tier: %v", order)
	}
}

// A lower-tier sticky channel must NOT be promoted above a higher tier; sticky marker is
// left off in that case.
func TestStickyMoveCannotRaiseLowerTier(t *testing.T) {
	globalSession = sync.Map{}
	globalBreaker = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode:            model.GroupModeFillFirst,
		SessionKeepTime: 300,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 0, ChannelPriority: 0}, // top tier
			{ChannelID: 2, ModelName: "m", Priority: 0, ChannelPriority: 5}, // lower tier sticky
		},
	}
	SetStickyWithSessionKey(9, "m", "session:low", 2, 22)

	it := NewIteratorWithSession(group, 9, "m", "session:low", true)
	if !it.Next() {
		t.Fatalf("expected a candidate")
	}
	if got := it.Item().ChannelID; got != 1 {
		t.Fatalf("lower-tier sticky channel must not rise above the top tier, got %d", got)
	}
	if it.IsSticky() {
		t.Fatalf("sticky must not be marked when the sticky channel stayed in its lower tier")
	}
}

// Within-tier sticky still promotes to the front of the tier and preserves the key.
func TestStickyMoveWithinTopTierStillPromotes(t *testing.T) {
	globalSession = sync.Map{}
	globalBreaker = sync.Map{}
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode:            model.GroupModeSpread, // same ChannelPriority => same tier
		SessionKeepTime: 300,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 0, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0, Weight: 1},
		},
	}
	SetStickyWithSessionKey(9, "m", "session:top", 2, 22)

	it := NewIteratorWithSession(group, 9, "m", "session:top", true)
	if !it.Next() {
		t.Fatalf("expected a candidate")
	}
	if got := it.Item().ChannelID; got != 2 {
		t.Fatalf("same-tier sticky channel must move to the front of its tier, got %d", got)
	}
	if !it.IsStickyChannelKey(2, 22) {
		t.Fatalf("same-tier sticky key must be preserved")
	}
}

// The owner exception is global: only the owner candidate is pulled to the very front,
// its lower-tier peers are NOT dragged up, and the others keep their relative order.
func TestPrioritizeOwnerChannelRaisesOnlyOwner(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeFillFirst,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 0, ChannelPriority: 0},
			{ChannelID: 2, ModelName: "m", Priority: 0, ChannelPriority: 1},
			{ChannelID: 3, ModelName: "m", Priority: 0, ChannelPriority: 5}, // owner, lowest tier
			{ChannelID: 4, ModelName: "m", Priority: 0, ChannelPriority: 5}, // owner's tier-mate
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)
	it.PrioritizeOwnerChannel(3)

	order := collectOrder(it)
	if len(order) != 4 {
		t.Fatalf("want 4 candidates, got %v", order)
	}
	if order[0] != 3 {
		t.Fatalf("owner channel must move across tiers to the very front, got %v", order)
	}
	if order[1] != 1 || order[2] != 2 || order[3] != 4 {
		t.Fatalf("only the owner may move; tier-mate and others keep relative order, got %v", order)
	}
}

// RemainingTierItems excludes the current item, stops at the tier edge, and returns
// copies (mutating the result must not touch the iterator).
func TestRemainingTierItemsExcludesCurrentAndStopsAtTierEdge(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeSpread, // tier = ChannelPriority only => 1,2 same tier; 3 is lower
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 0, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0, Weight: 1},
			{ChannelID: 3, ModelName: "m", Priority: 3, ChannelPriority: 5, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)
	if !it.Next() {
		t.Fatalf("expected first candidate")
	}
	if got := it.Item().ChannelID; got != 1 {
		t.Fatalf("expected deterministic first candidate 1, got %d", got)
	}
	rest := it.RemainingTierItems()
	if len(rest) != 1 || rest[0].ChannelID != 2 {
		t.Fatalf("remaining tier items must exclude current and stop at the tier edge, got %+v", rest)
	}
	rest[0].ChannelID = 999
	if again := it.RemainingTierItems(); len(again) != 1 || again[0].ChannelID != 2 {
		t.Fatalf("RemainingTierItems must return copies, iterator candidate was mutated: %+v", again)
	}
}

// ConsumeTierItems clamps at the current tier tail, never crossing into a lower tier.
func TestConsumeTierItemsClampsToTierTail(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeSpread,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 0, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0, Weight: 1},
			{ChannelID: 3, ModelName: "m", Priority: 3, ChannelPriority: 5, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)
	if !it.Next() {
		t.Fatalf("expected first candidate")
	}
	// Only ONE same-tier peer (channel 2) follows; 5 must clamp to that tier tail.
	it.ConsumeTierItems(5)
	if it.Index() != 1 {
		t.Fatalf("consume must clamp to the current tier tail (index 1), got %d", it.Index())
	}
	if !it.Next() {
		t.Fatalf("the lower tier must not be skipped by an over-large consume")
	}
	if got := it.Item().ChannelID; got != 3 {
		t.Fatalf("expected lower-tier channel 3 after the clamp, got %d", got)
	}
}

// nil / empty boundary safety for every new entry point.
func TestIteratorTierHelpersBoundarySafe(t *testing.T) {
	var nilIt *Iterator
	nilIt.PrioritizeChannels(map[int]bool{1: true}) // must not panic
	nilIt.PrioritizeOwnerChannel(1)                 // must not panic
	nilIt.ConsumeTierItems(2)                       // must not panic
	if got := nilIt.RemainingTierItems(); got != nil {
		t.Fatalf("nil iterator RemainingTierItems = %+v, want nil", got)
	}

	group := model.Group{
		Mode: model.GroupModeSpread,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 0, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)

	// Before any Next(): index < 0 => no-op / empty.
	it.ConsumeTierItems(3)
	if it.Index() != -1 {
		t.Fatalf("consume before Next must be a no-op, index=%d", it.Index())
	}
	if got := it.RemainingTierItems(); got != nil {
		t.Fatalf("RemainingTierItems before Next must be nil, got %+v", got)
	}
	if !it.Next() {
		t.Fatalf("expected a candidate")
	}
	it.ConsumeTierItems(0)
	it.ConsumeTierItems(-2)
	if it.Index() != 0 {
		t.Fatalf("n<=0 must be a no-op, index=%d", it.Index())
	}

	// Empty candidate set is safe for the new methods too.
	empty := NewIteratorWithSession(model.Group{Mode: model.GroupModeSpread}, 0, "m", "", false)
	if got := empty.RemainingTierItems(); got != nil {
		t.Fatalf("empty iterator RemainingTierItems = %+v, want nil", got)
	}
	empty.ConsumeTierItems(1) // must not panic
	empty.PrioritizeOwnerChannel(1)
}
