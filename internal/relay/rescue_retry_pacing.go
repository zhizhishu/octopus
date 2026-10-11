package relay

import (
	"math/rand/v2"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/intervention"
)

// Pacing and rotation for the automatic-rescue rounds.
//
// Production defect this closes (2026-10 d03, exp4): a request whose channel kept failing
// was retried 138 times inside the 300s rescue window — one attempt every ~2.2s, the same
// channel every time, no growth, ending in a 504 for the caller. Two independent causes,
// both here:
//
//  1. The no-breaker path threw the exponential schedule away. relay.go set
//     `backoff = time.Second` for every round as soon as ANY channel in the request had
//     DisableCircuitBreaker set ("keep trying like a direct CLI"), so 300s / (1s wait +
//     ~1.2s attempt) = ~138 rounds. A platform upstream that answers fast with a hard
//     failure therefore turns the rescue window into a hot loop against one channel.
//  2. Nothing counted failures per channel, so the loop had no reason to stop using a
//     channel that had just failed and could never rotate away from it.
//
// The fix keeps the intent of the no-breaker feature (its first rounds stay eager — 1s,
// like a direct client) but the schedule then grows, gets jitter, is clamped by the
// request's absolute ceiling, and the channel that keeps failing is dropped from the
// candidate set so the rescue spends its remaining budget on a DIFFERENT channel.
const (
	// noBreakerEagerRounds is how many rescue rounds a no-breaker channel may still run on
	// the flat 1s rhythm before the exponential schedule takes over.
	noBreakerEagerRounds = 2

	// maxRescueRoundsPerSameChannel is how many consecutive automatic-rescue rounds may
	// fail on the same channel before it is excluded and the loop rotates elsewhere.
	maxRescueRoundsPerSameChannel = 3

	// rescueBackoffJitter spreads simultaneous retries of different requests (thundering
	// herd) without changing the schedule's shape: monotonic growth survives because each
	// step doubles while the jitter band is +-20%.
	rescueBackoffJitter = 0.2
)

// rescueRoundBackoff is the wait before automatic-rescue round `round` (1-based). It starts
// eager — 1s for the first rounds, whether or not the request touched a no-breaker channel
// — and then follows the exponential schedule (2s, 4s, 8s, capped at 15s by
// intervention.BackoffDuration) with jitter. Retry-After may still raise it; the request's
// absolute ceiling may still clamp it down (both handled by the caller).
func rescueRoundBackoff(round int, noBreakerChannelSeen bool) time.Duration {
	base := intervention.BackoffDuration(round)
	if noBreakerChannelSeen && round <= noBreakerEagerRounds {
		base = time.Second
	}
	return jitteredBackoff(base)
}

func jitteredBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	// rand.Float64 in [-1, 1) scaled by the band, so the result stays within
	// [base*(1-band), base*(1+band)).
	delta := (rand.Float64()*2 - 1) * rescueBackoffJitter
	scaled := float64(base) * (1 + delta)
	if scaled < 0 {
		return 0
	}
	return time.Duration(scaled)
}

// rescueRotation counts consecutive rescue-round failures per channel and remembers which
// channels have been excluded. It is request-scoped (built inside the rescue block) and
// never shared between requests.
type rescueRotation struct {
	lastChannelID  int
	sameChannel    int
	exhausted      map[int]string
	exhaustedOrder []int
}

func newRescueRotation() *rescueRotation {
	return &rescueRotation{exhausted: make(map[int]string)}
}

// noteRound records the channel that the round just attempted (the last entry of the
// cumulative attempt log) and reports whether that channel has now crossed the exclusion
// threshold — true exactly once per channel, so the caller can log the switch without
// repeating itself every round.
func (r *rescueRotation) noteRound(attempts []dbmodel.ChannelAttempt) (exhaustedNow bool, channelID int, channelName string, rounds int) {
	id, name := lastAttemptChannel(attempts)
	if id == 0 {
		return false, 0, "", 0
	}
	if id != r.lastChannelID {
		r.lastChannelID = id
		r.sameChannel = 0
	}
	r.sameChannel++
	if r.sameChannel < maxRescueRoundsPerSameChannel {
		return false, id, name, r.sameChannel
	}
	if _, seen := r.exhausted[id]; seen {
		return false, id, name, r.sameChannel
	}
	r.exhausted[id] = name
	r.exhaustedOrder = append(r.exhaustedOrder, id)
	return true, id, name, r.sameChannel
}

// exclude drops every channel that has burned its consecutive-failure budget from the
// next round's candidate list, so the rescue switches channel instead of retrying in
// place. balancer.Iterator.Skip only records a decision, so the removal has to happen on
// the group the iterator is built from. A copy is returned; the caller's group is not
// mutated. A no-op before anything is exhausted.
func (r *rescueRotation) exclude(group dbmodel.Group) (dbmodel.Group, bool) {
	if len(r.exhaustedOrder) == 0 {
		return group, true
	}
	kept := make([]dbmodel.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		if _, dropped := r.exhausted[item.ChannelID]; dropped {
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

// exhaustedCount is how many distinct channels have been dropped so far.
func (r *rescueRotation) exhaustedCount() int {
	return len(r.exhaustedOrder)
}

// lastAttemptChannel reads the channel of the newest attempt. An empty log means no attempt
// was made this round (route selection failed), which must not count as a channel failure.
func lastAttemptChannel(attempts []dbmodel.ChannelAttempt) (int, string) {
	if len(attempts) == 0 {
		return 0, ""
	}
	last := attempts[len(attempts)-1]
	return last.ChannelID, last.ChannelName
}
