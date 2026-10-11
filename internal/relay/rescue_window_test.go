package relay

import (
	"errors"
	"testing"
	"time"
)

// TestRescueWindowDeadlineCapsAtAutoRescueCap proves the ONE automatic-recovery window is
// always capped at autoRescueCap and that the no-breaker budget and the operator hold
// timeout can only tighten it — never extend it (the 1800s operator hold is inert here).
func TestRescueWindowDeadlineCapsAtAutoRescueCap(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name            string
		noBreakerBudget time.Duration
		noBreakerActive bool
		operatorHold    time.Duration
		// The whole-request ceiling (third clock) is the outermost bound: a hold may
		// not outlive it.
		requestCeilingActive    bool
		requestCeilingRemaining time.Duration
		want                    time.Duration
	}{
		{name: "plain channel ignores the 1800s operator hold", noBreakerActive: false, operatorHold: 1800 * time.Second, want: autoRescueCap},
		{name: "operator hold can only tighten", noBreakerActive: false, operatorHold: 120 * time.Second, want: 120 * time.Second},
		{name: "no-breaker budget above the cap is ignored", noBreakerActive: true, noBreakerBudget: 600 * time.Second, operatorHold: 1800 * time.Second, want: autoRescueCap},
		{name: "no-breaker budget tightens", noBreakerActive: true, noBreakerBudget: 30 * time.Second, operatorHold: 1800 * time.Second, want: 30 * time.Second},
		{name: "zero no-breaker budget does not tighten", noBreakerActive: true, noBreakerBudget: 0, operatorHold: 1800 * time.Second, want: autoRescueCap},
		{name: "no caps present still bounded", noBreakerActive: false, want: autoRescueCap},
		{name: "request ceiling tightens the hold", requestCeilingActive: true, requestCeilingRemaining: 45 * time.Second, operatorHold: 1800 * time.Second, want: 45 * time.Second},
		{name: "expired request ceiling collapses the hold", requestCeilingActive: true, requestCeilingRemaining: 0, want: 0},
		{name: "inactive request ceiling changes nothing", requestCeilingActive: false, requestCeilingRemaining: 0, want: autoRescueCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rescueWindowDeadline(base, tc.noBreakerBudget, tc.noBreakerActive, tc.operatorHold, tc.requestCeilingActive, tc.requestCeilingRemaining)
			if want := base.Add(tc.want); !got.Equal(want) {
				t.Fatalf("deadline = %v, want %v", got, want)
			}
		})
	}
}

func TestRescueDeadlineExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if rescueDeadlineExpired(time.Time{}, now) {
		t.Fatal("an unset recovery start must not read as expired")
	}
	if rescueDeadlineExpired(now.Add(-autoRescueCap+time.Second), now) {
		t.Fatal("a window with time left must not read as expired")
	}
	if !rescueDeadlineExpired(now.Add(-autoRescueCap), now) {
		t.Fatal("a window that has just elapsed must read as expired")
	}
	if !rescueDeadlineExpired(now.Add(-autoRescueCap-time.Minute), now) {
		t.Fatal("a long-elapsed window must read as expired")
	}
}

// TestMarkRecoveryStart pins the start at the first rescuable failure and never moves it.
func TestMarkRecoveryStart(t *testing.T) {
	if got := markRecoveryStart(time.Time{}, nil); !got.IsZero() {
		t.Fatal("a nil error must not start the recovery clock")
	}
	if got := markRecoveryStart(time.Time{}, newUpstreamError(400, []byte(`{"error":{"message":"bad"}}`))); !got.IsZero() {
		t.Fatal("a deterministic 400 must not start the recovery clock")
	}
	transient := errors.New("upstream hiccup")
	started := markRecoveryStart(time.Time{}, transient)
	if started.IsZero() {
		t.Fatal("a transient failure must start the recovery clock")
	}
	if got := markRecoveryStart(started, transient); !got.Equal(started) {
		t.Fatalf("an already-pinned start must not move: %v -> %v", started, got)
	}
	if got := markRecoveryStart(started, nil); !got.Equal(started) {
		t.Fatalf("an already-pinned start must not be cleared: %v -> %v", started, got)
	}
}
