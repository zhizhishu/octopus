package relay

import "time"

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
// It is always capped at autoRescueCap; an active no-breaker budget and the operator hold
// timeout may only tighten it further (they are ignored when larger, and when <= 0).
func rescueWindowDeadline(recoveryStart time.Time, noBreakerBudget time.Duration, noBreakerActive bool, operatorHold time.Duration) time.Time {
	cap := autoRescueCap
	if noBreakerActive && noBreakerBudget > 0 && noBreakerBudget < cap {
		cap = noBreakerBudget
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

// releaseRescueDeadline disarms the releasable automatic-recovery deadline. It is called
// the instant real content is committed to the client: from then on the response counts
// as recovered and must survive the rescue clock (a body that then stalls is still
// bounded by the stream data-interval and first-token guards). A no-op outside rescue and
// idempotent, so callers do not need to know whether a rescue is even in progress.
func (r *relayRequest) releaseRescueDeadline() {
	if r == nil || r.rescueDeadlineTimer == nil {
		return
	}
	r.rescueDeadlineTimer.Stop()
	r.rescueDeadlineTimer = nil
}
