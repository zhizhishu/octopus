package relay

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// autoRescueCap is the hard ceiling for the ONE machine-driven automatic-recovery
// window. It is measured from the first rescuable failure and shared by every branch
// that the same request may take afterwards — plain channels, no-breaker channels and
// operator-triggered retries alike. No switch, channel change or operator click may
// reset or widen it; the operator hold timeout (relay_intervention_timeout_seconds,
// 1800s default) can only tighten it, never lend extra life to an automatic retry.
// Overridable in tests so the cap can be exercised without waiting the full 300s.
var autoRescueCap = 300 * time.Second

// markRecoveryStart pins the start of the automatic-recovery window at the first
// rescuable failure and never moves it again. A request whose earlier attempts failed
// with a deterministic (non-rescuable) error does not start the clock, and a value
// already set is preserved verbatim — that is what makes the window branch-independent.
func markRecoveryStart(current time.Time, err error) time.Time {
	if !current.IsZero() || !isEligibleForInterventionRescue(err) {
		return current
	}
	return time.Now()
}

// rescueWindowDeadline returns the absolute instant the automatic-recovery window closes.
// It is always capped at autoRescueCap; the configured rescue budget
// (relay_no_breaker_retry_budget_seconds, shared by plain and no-breaker channels) and the
// operator hold timeout may only tighten it further (they are ignored when larger, and
// when <= 0).
func rescueWindowDeadline(recoveryStart time.Time, rescueBudget time.Duration, rescueBudgetActive bool, operatorHold time.Duration) time.Time {
	cap := autoRescueCap
	if rescueBudgetActive && rescueBudget > 0 && rescueBudget < cap {
		cap = rescueBudget
	}
	if operatorHold > 0 && operatorHold < cap {
		cap = operatorHold
	}
	return recoveryStart.Add(cap)
}

// rescueDeadlineExpired reports whether the automatic-recovery window has already
// elapsed, so no further attempt may be started. The sweep guard uses the loosest cap
// (autoRescueCap): the branch-specific tightening only becomes known once the hold block
// runs, and that block re-derives the deadline and opens an immediately-expired window.
func rescueDeadlineExpired(recoveryStartedAt, now time.Time) bool {
	return !recoveryStartedAt.IsZero() && !now.Before(recoveryStartedAt.Add(autoRescueCap))
}

// relayRescueClock owns the releasable automatic-recovery deadline state. It lives on
// its own struct behind a pointer on relayRequest so the racer shallow copy
// (prepareRacerAttempt) shares the state without copying a sync.Mutex. All fields are
// guarded by mu; a nil clock outside rescue is handled by the nil-safe accessors.
// attemptCancelTimer is the current PRE-HOLD attempt's cancel-on-deadline timer (see
// beginControlledAttempt): releaseRescueDeadline stops it together with the window
// timer, so a delivery that commits mid-attempt survives past the cap.
type relayRescueClock struct {
	mu                 sync.Mutex
	timer              *time.Timer
	attemptCancelTimer *time.Timer
	at                 time.Time
	released           bool
}

func (r *relayRequest) rescueClockState() *relayRescueClock {
	if r == nil {
		return nil
	}
	return r.rescueClock
}

// releaseRescueDeadline disarms the releasable automatic-recovery deadline. It is called
// the instant real content is committed to the client: from then on the response counts
// as recovered and must survive the rescue clock (a body that then stalls is still
// bounded by the stream data-interval and first-token guards). A no-op outside rescue and
// idempotent, so callers do not need to know whether a rescue is even in progress.
//
// Guarded by the clock's mutex so it cannot interleave with the fired callback: if the
// callback has already begun running, Stop() reports false but cannot stop it — the
// released flag (read under the same lock by the fire decision) is what makes that
// in-flight callback back off instead of marking rescueFired on a committed body.
func (r *relayRequest) releaseRescueDeadline() {
	clock := r.rescueClockState()
	if clock == nil {
		return
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	// released marks "content committed" regardless of whether a window timer was
	// ever armed (a pre-hold attempt creates the clock without one) — the flag is
	// what stale fire decisions read, so it must be set on every release.
	if clock.timer != nil {
		clock.timer.Stop()
		clock.timer = nil
	}
	if clock.attemptCancelTimer != nil {
		clock.attemptCancelTimer.Stop()
		clock.attemptCancelTimer = nil
	}
	clock.released = true
}

// beginRescueFire is the fire-side half of the release-vs-fire mutual exclusion
// (R18) and the single decision point used by the rescue deadline timer callback: when
// the deadline was already released (content committed), a callback that already
// started running (Timer.Stop cannot stop it) is stale and must not mark rescueFired —
// otherwise a fully delivered body gets relabeled octopus_rescue_timeout by the
// terminal override. Returns true when the fire stands.
func (r *relayRequest) beginRescueFire(rescueFired *atomic.Bool) bool {
	clock := r.rescueClockState()
	if clock == nil {
		return false
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.released {
		return false
	}
	rescueFired.Store(true)
	return true
}

// armRescueDeadline arms (first hold entry, timer == nil) or tightens (hold re-entry:
// machine rescue retry / operator channel switch) the releasable automatic-recovery
// deadline. The window is ONE per request: a recomputed deadline that is equal or
// later never extends the current one (tighten-only), and a deadline already released
// (content committed) is ignored entirely. The fire callback routes through
// beginRescueFire so the R18 release-vs-fire mutual exclusion holds for both arming
// paths. onFire is invoked only when the fire stands.
func (r *relayRequest) armRescueDeadline(deadline time.Time, rescueFired *atomic.Bool, onFire func()) {
	if r.rescueClock == nil {
		r.rescueClock = &relayRescueClock{}
	}
	clock := r.rescueClock
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.released {
		return
	}
	if clock.timer != nil && !deadline.Before(clock.at) {
		// Tighten-only: an equal or later deadline never lends extra life.
		return
	}
	if clock.timer == nil {
		clock.timer = time.AfterFunc(time.Until(deadline), func() {
			if !r.beginRescueFire(rescueFired) {
				return
			}
			onFire()
		})
	} else {
		if !clock.timer.Stop() {
			// The callback already fired or is firing; drain the channel so Reset
			// (Go >= 1.23: always reschedules) never waits on a stale send.
			select {
			case <-clock.timer.C:
			default:
			}
		}
		clock.timer.Reset(time.Until(deadline))
	}
	clock.at = deadline
}

// armAttemptDeadlineCancel bounds a PRE-HOLD attempt (one that starts before the
// intervention hold exists — R10) with a cancel fired at the automatic-recovery
// deadline. A plain context.WithDeadline cannot be undone, but a recovered delivery
// that commits mid-attempt must survive past the cap, so the deadline rides a
// stoppable timer around the attempt's own cancel: releaseRescueDeadline stops it,
// and a fire that already started re-checks released under the same lock before
// canceling (the R18 decision-point pattern). Returns the timer's stop func for the
// attempt's endAttempt cleanup; a no-op when the clock was already released.
func (r *relayRequest) armAttemptDeadlineCancel(at time.Time, cancel context.CancelFunc) func() {
	if r.rescueClock == nil {
		r.rescueClock = &relayRescueClock{}
	}
	clock := r.rescueClock
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.released {
		return func() {}
	}
	timer := time.AfterFunc(time.Until(at), func() {
		clock.mu.Lock()
		stale := clock.released
		clock.mu.Unlock()
		if !stale {
			cancel()
		}
	})
	clock.attemptCancelTimer = timer
	return func() { timer.Stop() }
}
