package balancer

import (
	"fmt"
	"strings"
)

// DecisionLogHook, when non-nil, is invoked at the end of iterator construction with
// the final candidate ordering. It lets an upper layer (relay) emit a debug trace
// WITHOUT the balancer importing the logger/settings packages, which would create an
// import cycle. nil = disabled (the default; zero per-request cost).
var DecisionLogHook func(requestModel string, it *Iterator)

// DecisionTrace renders why the candidates ended up in this order: for each candidate,
// the (priority, spreadTier, spreadRank) it sorted on plus the capacity inputs that fed
// those bands, and which one (if any) is the sticky pick. Used by the load-balancer
// decision debug log so operators can answer "why this channel / why slow".
func (it *Iterator) DecisionTrace() string {
	if it == nil || len(it.candidates) == 0 {
		return "no candidates"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d candidates (order = final pick order)", len(it.candidates))
	for i, item := range it.candidates {
		rt := item.RoutingStats
		fmt.Fprintf(&b, " | #%d ch=%d pri=%d tier=%d rank=%d keys=%d/%d load=%d fails=%d lat=%.0fms",
			i, item.ChannelID, item.Priority, spreadTier(item), spreadRank(item),
			rt.HealthyKeyCount, rt.AvailableKeyCount,
			rt.InFlight+rt.PendingSelections, rt.ConsecutiveFailures,
			effectiveSpreadLatencyMs(rt))
		if rt.CircuitTripped {
			b.WriteString(" circuit-open")
		}
		if rt.CooldownRemainingMs > 0 {
			fmt.Fprintf(&b, " cooldown=%dms", rt.CooldownRemainingMs)
		}
		if it.stickyIdx == i {
			b.WriteString(" <STICKY>")
		}
	}
	return b.String()
}
