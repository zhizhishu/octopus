package relay

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestRescueDeadlineReleaseBeatsLateFire pins the release-vs-fire race on the
// automatic-recovery deadline (R18): once releaseRescueDeadline has run (real
// content committed to the client), a rescue timer that has already fired must
// NOT mark rescueFired — otherwise a fully delivered body is later labeled
// octopus_rescue_timeout by the terminal rescueStopError override (relay.go).
//
// The callback below mirrors relay.go's hold-block time.AfterFunc callback. The
// 50ms sleep is a FIXED red-test injection emulating the production callback's
// dispatch/scheduling latency: time.Timer.Stop cannot cancel a callback that has
// already begun running, so the real race window is "callback started, Store not
// executed yet" — exactly the interleaving this test forces deterministically.
// The product code must stay sleep-free; the latency lives only here.
func TestRescueDeadlineReleaseBeatsLateFire(t *testing.T) {
	req := &relayRequest{rescueClock: &relayRescueClock{}}
	rescueFired := &atomic.Bool{}

	fired := make(chan struct{})
	done := make(chan struct{})
	// Direct product call: the fire path (beginRescueFire) and the release path
	// (releaseRescueDeadline) share the clock's mutex, so this forces the exact
	// "callback started, release lands, Store still pending" interleaving.
	time.AfterFunc(20*time.Millisecond, func() {
		close(fired) // the timer has fired; Stop() can no longer prevent this callback
		// INJECTION (test-only): emulate callback dispatch latency in production
		// (time.Timer.Stop cannot cancel a callback that already began running).
		// The delayed Store must go through the REAL product decision point
		// beginRescueFire so the release-vs-fire mutex is exercised end to end.
		time.Sleep(50 * time.Millisecond)
		stood := req.beginRescueFire(rescueFired)
		if stood {
			t.Error("beginRescueFire stood after releaseRescueDeadline: a committed body would be labeled octopus_rescue_timeout")
		}
		close(done)
	})

	<-fired
	// Product call under test: content was committed, the deadline is released
	// while the fired callback is still in flight.
	req.releaseRescueDeadline()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline callback did not complete")
	}
	if rescueFired.Load() {
		t.Fatal("late fire marked rescueFired after releaseRescueDeadline: a committed body would be labeled octopus_rescue_timeout")
	}
}
