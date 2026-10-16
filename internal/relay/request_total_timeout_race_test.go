package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newCeilingAttemptForLifetime builds the minimum attempt boundUpstreamLifetime reads: the
// captured ceiling (deadline/budget/clock). It deliberately has no gin context, adapter or
// key, because the function under test must not need them.
func newCeilingAttemptForLifetime(deadline time.Time, budget time.Duration) *relayAttempt {
	return &relayAttempt{relayRequest: &relayRequest{
		totalDeadline: deadline,
		totalBudget:   budget,
		totalClock:    &relayTotalClock{},
	}}
}

// Regression 1: a parent that is ALREADY canceled must not panic and must not leave a timer
// armed. schedule() sees ctx.Err() != nil and returns before ever creating a timer, so the
// parent-cleanup callback reaches a nil timer; the nil-guard is what keeps that from
// dereferencing (the old code called timer.Stop() unconditionally).
func TestBoundUpstreamLifetimeCanceledParentDoesNotPanicOrLeaveTimer(t *testing.T) {
	ra := newCeilingAttemptForLifetime(time.Now().Add(time.Hour), time.Hour)

	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx := ra.boundUpstreamLifetime(parent)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a canceled parent must produce an already-done context")
	}
}

// Regression 2: a timer rearm racing a cancel, hammered in a loop with very short deadlines
// so `go test -race` exercises the timerMu/stopped window. Every iteration must terminate
// (the loop would hang, or the race detector would flag a torn timer write, otherwise) and no
// timer may be left behind. The knobs are shrunk for the whole test and restored only after
// the final chain is quiesced (t.Cleanup runs after this body returns).
func TestBoundUpstreamLifetimeRearmRacesCancel(t *testing.T) {
	withFlowKnobs(t, 2*time.Millisecond, 2*time.Millisecond)

	for i := 0; i < 300; i++ {
		ra := newCeilingAttemptForLifetime(
			time.Now().Add(time.Duration(i%5)*time.Millisecond+time.Millisecond),
			50*time.Millisecond,
		)
		// Some iterations carry live content, so the schedule path rearms on the recheck
		// interval exactly while the parent cancel races it.
		if i%3 == 0 {
			ra.noteMeaningfulContent()
		}

		parent, cancelParent := context.WithCancel(context.Background())
		ctx := ra.boundUpstreamLifetime(parent)
		go cancelParent()

		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("bound upstream context never completed")
		}
	}

	// Let every cleanup callback run (it sets stopped under the lock and Stops the timer)
	// before the flow knobs are written back by t.Cleanup, so no rearm can read a
	// concurrently-restored global.
	time.Sleep(50 * time.Millisecond)
}

// Regression 3: after the race winner handoff repoints relayRequest (and flips the old
// cross-attempt bool), the captured deadline must still fire on time — the closure must not
// read ra/relayRequest. A handoff request has a ZERO totalDeadline, so a closure that read
// ra.totalDeadline live would rearm forever instead of firing.
func TestBoundUpstreamLifetimeSurvivesWinnerHandoff(t *testing.T) {
	ra := newCeilingAttemptForLifetime(time.Now().Add(120*time.Millisecond), 500*time.Millisecond)
	ctx := ra.boundUpstreamLifetime(context.Background())

	// Simulate the main-goroutine handoff: swap the embedded request and flip the bool.
	handoff := &relayRequest{}
	ra.relayRequest = handoff
	ra.wroteMeaningfulDownstream = true

	select {
	case <-ctx.Done():
		t.Fatal("the captured deadline must not fire before it is due")
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errRequestTotalTimeout) {
			t.Fatalf("cause = %v, want errRequestTotalTimeout", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("winner handoff must not stall the captured deadline")
	}
}

// Regression 4: the live-answer stand-down must come from the CAPTURED clock's lastContentAt
// (non-zero + within grace), not from the racy wroteMeaningfulDownstream bool. With content
// marked just before the ceiling, the context must stay alive well past the deadline.
func TestBoundUpstreamLifetimeStandsDownForLiveContent(t *testing.T) {
	withFlowKnobs(t, 20*time.Millisecond, 400*time.Millisecond)

	ra := newCeilingAttemptForLifetime(time.Now().Add(50*time.Millisecond), 500*time.Millisecond)
	ra.noteMeaningfulContent() // real content just went downstream

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx := ra.boundUpstreamLifetime(parent)

	select {
	case <-ctx.Done():
		t.Fatal("the ceiling must stand down while real content is still flowing")
	case <-time.After(250 * time.Millisecond):
		// Still alive far past the 50ms deadline: the rearm kept the ceiling from cutting.
	}

	// Quiesce the rearm chain before t.Cleanup writes the flow knobs back.
	cancelParent()
	time.Sleep(50 * time.Millisecond)
}
