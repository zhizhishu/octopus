package balancer

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// The per-channel「渠道优先级」(ChannelPriority) is the PRIMARY routing key: even when
// the per-item pool priority would prefer another channel, the smaller channel
// priority wins. Covers the fill-first (Failover) strategy.
func TestFailoverChannelPriorityOverridesItemPriority(t *testing.T) {
	// Item priority alone would put ch1 first (Priority 1 < 2), but ch2 carries the
	// smaller CHANNEL priority (0 < 5), so ch2 must lead.
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 5},
		{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0},
	}
	got := (&Failover{}).Candidates(items)
	if len(got) != 2 || got[0].ChannelID != 2 || got[1].ChannelID != 1 {
		t.Fatalf("channel priority must dominate item priority (ch2 first), got %#v", got)
	}
}

// Same guarantee for the load-balancing (Spread) strategy.
func TestSpreadChannelPriorityOverridesItemPriority(t *testing.T) {
	roundRobinCounters = sync.Map{}
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 5},
		{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0},
	}
	got := (&Spread{}).Candidates(items)
	if len(got) != 2 || got[0].ChannelID != 2 {
		t.Fatalf("channel priority must dominate item priority in spread (ch2 first), got %#v", got)
	}
}

// Backward compatibility: with no channel priority set (all default 0), routing falls
// back to the pre-existing per-item priority order — zero behaviour change. This is the
// state of every deployment before an operator ever touches a channel priority.
func TestRoutingFallsBackToItemPriorityWhenChannelPriorityUnset(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 2, ChannelPriority: 0},
		{ChannelID: 2, ModelName: "m", Priority: 1, ChannelPriority: 0},
	}
	got := (&Failover{}).Candidates(items)
	if len(got) != 2 || got[0].ChannelID != 2 || got[1].ChannelID != 1 {
		t.Fatalf("with channel priority unset, item priority must order (ch2 pri1 first), got %#v", got)
	}
}

// When channel + item priority both tie, fill-first orders deterministically by
// ChannelID so it concentrates on the same channel across restarts.
func TestFailoverDeterministicTieBreakByChannelID(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 20, ModelName: "m", Priority: 0, ChannelPriority: 0},
		{ChannelID: 10, ModelName: "m", Priority: 0, ChannelPriority: 0},
	}
	got := (&Failover{}).Candidates(items)
	if len(got) != 2 || got[0].ChannelID != 10 {
		t.Fatalf("equal priorities must tie-break by ChannelID (10 before 20), got %#v", got)
	}
}

// Within the same channel priority, the per-item pool priority still acts as the
// secondary boundary (so access-plan / hand-edited pools keep working).
func TestRoutingItemPriorityBreaksChannelPriorityTie(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 5, ChannelPriority: 3},
		{ChannelID: 2, ModelName: "m", Priority: 1, ChannelPriority: 3},
	}
	got := (&Failover{}).Candidates(items)
	if len(got) != 2 || got[0].ChannelID != 2 {
		t.Fatalf("same channel priority must fall through to item priority (ch2 pri1 first), got %#v", got)
	}
}
