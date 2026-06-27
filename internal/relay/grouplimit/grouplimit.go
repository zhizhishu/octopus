// Package grouplimit implements an in-memory, per-group admission gate for the
// relay: a hard concurrency cap and a hard requests-per-minute (RPM) cap, scoped
// to a model group.
//
// Why a HARD gate here (unlike the channel-level soft demote): a request that has
// been routed to a group is already pinned to that group's candidate set — there
// is nowhere else to spread it. So the channel-level "demote and let it bleed to a
// peer" trick does not apply. To actually protect the upstream from being maxed
// out (the whole point of the knob), the group cap rejects with 429 once it is
// reached, mirroring new-api's group-level model rate limiter. The channel-level
// RPM/concurrency knobs remain soft (spread within the group); the group-level
// knobs are the real ceiling on total throughput into the group.
package grouplimit

import (
	"sync"
	"time"
)

// entry holds one group's live in-flight count and a 60-bucket-per-second
// sliding-window request counter (for RPM).
type entry struct {
	mu       sync.Mutex
	inFlight int64
	// reqWindow[i] counts requests admitted during the epoch-second recorded in
	// reqBucketSec[i]. A slot whose recorded second falls outside the trailing 60s
	// window is treated as empty, so the window self-expires without a sweeper.
	reqWindow    [60]int32
	reqBucketSec [60]int64
}

func (e *entry) recordRequestLocked(now time.Time) {
	sec := now.Unix()
	idx := sec % 60
	if e.reqBucketSec[idx] != sec {
		e.reqBucketSec[idx] = sec
		e.reqWindow[idx] = 0
	}
	e.reqWindow[idx]++
}

func (e *entry) recentRequestCountLocked(now time.Time) int64 {
	cutoff := now.Unix() - 59
	var total int64
	for i := 0; i < 60; i++ {
		if e.reqBucketSec[i] >= cutoff {
			total += int64(e.reqWindow[i])
		}
	}
	return total
}

var groups sync.Map // groupID(int) -> *entry

func getEntry(groupID int) *entry {
	e := &entry{}
	actual, _ := groups.LoadOrStore(groupID, e)
	return actual.(*entry)
}

// noop is the release returned on the disabled / rejected paths.
func noop() {}

// Acquire admits one request into the group subject to its concurrency and RPM
// caps. maxConcurrent <= 0 disables the concurrency cap; rpmLimit <= 0 disables
// the RPM cap (so a group with both at 0 is never gated and Acquire is a no-op).
//
// On admission it returns ok=true and a release func that MUST be called exactly
// once when the request finishes, to free the concurrency slot. On rejection it
// returns ok=false with a human-readable reason and a no-op release; the caller
// should reply HTTP 429. A rejected request is NOT counted toward the RPM window,
// and the RPM window is only advanced for admitted requests.
func Acquire(groupID, maxConcurrent, rpmLimit int) (release func(), ok bool, reason string) {
	if maxConcurrent <= 0 && rpmLimit <= 0 {
		return noop, true, ""
	}

	e := getEntry(groupID)
	now := time.Now()

	e.mu.Lock()
	defer e.mu.Unlock()

	if rpmLimit > 0 && e.recentRequestCountLocked(now) >= int64(rpmLimit) {
		return noop, false, "group requests-per-minute limit reached, please retry shortly"
	}
	if maxConcurrent > 0 && e.inFlight >= int64(maxConcurrent) {
		return noop, false, "group concurrency limit reached, please retry shortly"
	}

	// Admit: reserve a concurrency slot and count the request toward the RPM window.
	countsConcurrency := maxConcurrent > 0
	if countsConcurrency {
		e.inFlight++
	}
	if rpmLimit > 0 {
		e.recordRequestLocked(now)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if !countsConcurrency {
				return
			}
			e.mu.Lock()
			if e.inFlight > 0 {
				e.inFlight--
			}
			e.mu.Unlock()
		})
	}, true, ""
}

// ResetForTest clears all group limiter state. Tests only; never called in
// production.
func ResetForTest() {
	groups = sync.Map{}
}
