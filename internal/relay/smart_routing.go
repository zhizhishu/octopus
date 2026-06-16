package relay

import (
	"context"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
)

func enrichGroupForSmartRouting(ctx context.Context, group dbmodel.Group, preferStream ...bool) dbmodel.Group {
	// Fill-first keeps a stable priority order and does not consult runtime
	// capacity, so it needs no snapshot. Every other (spread/round-robin) mode is
	// load-aware and must be hydrated with channel priority, stats, and runtime
	// telemetry before ranking.
	if group.Mode == dbmodel.GroupModeFillFirst || len(group.Items) == 0 {
		return group
	}
	stream := len(preferStream) > 0 && preferStream[0]

	items := make([]dbmodel.GroupItem, len(group.Items))
	copy(items, group.Items)
	for i := range items {
		var keys []dbmodel.ChannelKey
		channel, err := op.ChannelGet(items[i].ChannelID, ctx)
		if err == nil && channel != nil {
			items[i].ChannelPriority = channel.Priority
			keys = channel.GetAvailableChannelKeys()
		}
		items[i].ChannelStats = op.StatsChannelGet(items[i].ChannelID)
		items[i].RoutingStats = balancer.SnapshotRoutingRuntime(items[i].ChannelID, items[i].ModelName, keys, stream)
	}
	group.Items = items
	return group
}
