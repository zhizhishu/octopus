package relay

import (
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

// Pacing for the automatic-rescue rounds.
//
// The schedule is a fixed one-second round, shared by plain and no-breaker channels. The
// earlier build grew the wait exponentially with jitter and dropped a channel after three
// consecutive failures; that is gone. Rescue is instead bounded by exactly two clocks: the
// ONE automatic-recovery window (= first rescuable failure, min(configured budget, 300s))
// and the whole-request ceiling. Within that window the same channel may be retried again
// and again — a single channel can fail six times and still be rescued on the seventh.
const (
	// rescueRoundInterval is the fixed wait before every automatic-rescue round.
	rescueRoundInterval = time.Second
)

// rescueRoundBackoff is the wait before automatic-rescue round `round` (1-based). It is
// always rescueRoundInterval; the arguments are kept for call-site compatibility only (the
// round number and whether a no-breaker channel was seen no longer change the pace).
func rescueRoundBackoff(round int, noBreakerChannelSeen bool) time.Duration {
	_ = round
	_ = noBreakerChannelSeen
	return rescueRoundInterval
}

// excludeAttemptedChannels drops the channels this request already dispatched to. The
// access-route pool fallback is a SECOND pass over another candidate set, and it used to
// re-dispatch a channel that had just failed milliseconds earlier: with a single-candidate
// pool the upstream saw two requests 1ms apart for one retry (duplicate execution, and
// duplicate billing on a non-idempotent model call). The pass exists to reach channels the
// route did not try, so what has been attempted is removed from it.
func excludeAttemptedChannels(group dbmodel.Group, attempts []dbmodel.ChannelAttempt) (dbmodel.Group, bool) {
	if len(attempts) == 0 || len(group.Items) == 0 {
		return group, len(group.Items) > 0
	}
	burned := make(map[int]struct{}, len(attempts))
	for _, attempt := range attempts {
		if attempt.ChannelID != 0 {
			burned[attempt.ChannelID] = struct{}{}
		}
	}
	kept := make([]dbmodel.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		if _, seen := burned[item.ChannelID]; seen {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == len(group.Items) {
		return group, true
	}
	group.Items = kept
	return group, len(kept) > 0
}
