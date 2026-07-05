package relay

import (
	"context"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
)

func enrichGroupForSmartRouting(ctx context.Context, group dbmodel.Group, preferStream ...bool) dbmodel.Group {
	// Resolve fleet-wide routing overrides/defaults FIRST: a route_mode_override must
	// be visible to the fill-first short-circuit below (a spread override still needs
	// stats hydrated), and first_token_time_out_default must ride along on every
	// routing path (this is the single funnel all relay entry points pass through).
	group = applyGroupGlobalDefaults(group)
	if len(group.Items) == 0 {
		return group
	}
	stream := len(preferStream) > 0 && preferStream[0]
	// Fill-first keeps a stable priority order and does not consult runtime
	// capacity, so it needs no telemetry snapshot. Every other (spread/round-robin)
	// mode is load-aware and must be hydrated with stats + runtime telemetry before
	// ranking. Channel priority, however, is the PRIMARY routing key in BOTH
	// strategies (see balancer.Failover/Spread), so it is hydrated for every mode —
	// fill-first orders by it too.
	fillFirst := group.Mode == dbmodel.GroupModeFillFirst

	items := make([]dbmodel.GroupItem, len(group.Items))
	copy(items, group.Items)
	for i := range items {
		channel, err := op.ChannelGet(items[i].ChannelID, ctx)
		if err == nil && channel != nil {
			// The per-channel「渠道优先级」field is the primary routing priority:
			// smaller = selected first. It defaults to 0, so a deployment where no
			// channel priority was ever set leaves every candidate tied on this key
			// and the balancer falls back to the existing per-item priority — zero
			// behaviour change until an operator actually sets a channel priority.
			items[i].ChannelPriority = channel.Priority
		}
		if fillFirst {
			continue
		}
		var keys []dbmodel.ChannelKey
		maxConcurrent := 0
		rpmLimit := 0
		if err == nil && channel != nil {
			keys = channel.GetAvailableChannelKeys()
			maxConcurrent = channel.MaxConcurrent
			rpmLimit = channel.RPMLimit
		}
		items[i].ChannelStats = op.StatsChannelGet(items[i].ChannelID)
		items[i].RoutingStats = balancer.SnapshotRoutingRuntime(items[i].ChannelID, items[i].ModelName, keys, stream)
		items[i].RoutingStats.MaxConcurrent = maxConcurrent
		items[i].RoutingStats.RPMLimit = rpmLimit
	}
	group.Items = items
	return group
}
