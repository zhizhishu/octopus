package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/utils/log"
)

// The THIRD clock: an absolute ceiling on one whole request.
//
// Why a third one is needed. Two clocks already exist and neither can bound a
// slow-but-alive upstream:
//
//   - relay_stream_data_interval_timeout_seconds (the mid-stream idle window) is reset
//     by EVERY event read from the upstream (relay.go / images.go reset it immediately
//     after each successful read, before the event is even classified), so an upstream
//     that emits one byte just inside the window keeps the request open forever.
//   - first_token_time_out_default is released by the first real content, so it says
//     nothing about a request that has been generating for minutes.
//
// Production shape that motivated this (2026-10 d03, callers on a 1M-token tier): one
// turn took 154s and emitted 17209 output tokens, and the next one was still generating
// after six minutes — no silence to catch, no first-content gap to catch, and the client
// only ended it by cancelling (499 octopus_client_canceled). The same reference
// implementation this fleet already borrows from (axonhub) carries exactly this knob
// (`server.llm_request_timeout: 600s`), which is where the default comes from.
//
// Contract:
//
//   - The deadline is computed ONCE, at request construction, and never moves: no event,
//     no channel switch, no rescue round and no operator action may extend it.
//   - When it fires with content already delivered, the caller gets an honest ending:
//     the content it already received, plus an explicit in-band terminal frame. There is
//     deliberately no channel switch at that point — bytes are out, so a failover could
//     only duplicate the answer.
//   - When it fires before any content was delivered it is an ordinary attempt failure:
//     the existing failover/automatic-rescue machinery takes over exactly as it does for
//     the other two clocks.
//
// It is a ceiling of last resort, not a target: a legitimate long answer that would have
// finished under the budget must never be touched, which is why the default is generous
// and why 0 (disabled) is a supported value.

// requestTotalTimeoutOverride lets tests exercise the ceiling in milliseconds instead of
// waiting the configured seconds. 0 means "use the setting". Same idea as autoRescueCap.
var requestTotalTimeoutOverride time.Duration

// resolvedRequestTotalBudget turns the resolved seconds into the window actually armed.
func resolvedRequestTotalBudget(seconds int) time.Duration {
	if requestTotalTimeoutOverride > 0 {
		return requestTotalTimeoutOverride
	}
	return requestTotalTimeoutDuration(seconds)
}

// errRequestTotalTimeout is the cause attached to the upstream context when the request
// ceiling cancels it, so the failure can be told apart from a client cancellation (which
// must keep its own attribution and must not be counted against a channel).
var errRequestTotalTimeout = errors.New("relay request total timeout")

// relayTotalClock carries the ceiling's fire bookkeeping across attempts. It lives behind
// a pointer on relayRequest so the racer shallow copy shares it without copying a mutex.
type relayTotalClock struct {
	mu   sync.Mutex
	cuts int
	// lastContentAt is when the last MEANINGFUL content went downstream (a content payload,
	// not a keepalive/opener). The ceiling uses it to tell "we are waiting" from "the
	// upstream is answering": see meaningfulContentIdle.
	lastContentAt time.Time
}

// noteMeaningfulContent records real content progress. Called from the write points that
// carry business payload (content/tool_call/reasoning), never from keepalive writes.
func (ra *relayAttempt) noteMeaningfulContent() {
	if ra == nil || ra.totalClock == nil {
		return
	}
	ra.totalClock.mu.Lock()
	ra.totalClock.lastContentAt = time.Now()
	ra.totalClock.mu.Unlock()
}

// meaningfulContentIdle reports whether real content has stopped arriving for long enough
// that the ceiling may act. Content inside the grace window means the upstream is still
// producing an answer, which the ceiling must not cut: it bounds rescue waiting (waiting
// for first content, waiting before a retry or a channel switch), not a live answer.
func (ra *relayAttempt) meaningfulContentIdle() bool {
	if ra == nil || ra.totalClock == nil {
		return true
	}
	ra.totalClock.mu.Lock()
	last := ra.totalClock.lastContentAt
	ra.totalClock.mu.Unlock()
	if last.IsZero() {
		return true
	}
	return time.Since(last) > totalTimeoutFlowGrace
}

func (r *relayRequest) totalClockState() *relayTotalClock {
	if r == nil {
		return nil
	}
	return r.totalClock
}

// totalBudgetActive reports whether this request has a ceiling at all.
func (r *relayRequest) totalBudgetActive() bool {
	return r != nil && !r.totalDeadline.IsZero()
}

// totalBudgetRemaining is the time left before the ceiling, and whether the budget is
// still useful (a non-positive remainder means the ceiling has already passed).
func (r *relayRequest) totalBudgetRemaining() (time.Duration, bool) {
	if !r.totalBudgetActive() {
		return 0, false
	}
	return time.Until(r.totalDeadline), true
}

// totalBudgetRemainingClamped is the ceiling's contribution to the automatic-recovery
// hold: the hold may never outlive the ceiling, so it is tightened to what is left.
func (r *relayRequest) totalBudgetRemainingClamped() (time.Duration, bool) {
	remaining, active := r.totalBudgetRemaining()
	if !active {
		return 0, false
	}
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// totalBudgetExhausted reports whether the ceiling has already fired for this request.
func (r *relayRequest) totalBudgetExhausted() bool {
	clock := r.totalClockState()
	if clock == nil {
		return false
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.cuts > 0
}

// noteTotalBudgetCut records a fire and returns how many times the ceiling has now cut
// this request. The ordinal is what keeps a request that is already out of budget from
// being retried into a storm: the first cut may still be handed to the rescue chain, any
// later one must stop instead of starting another channel.
func (r *relayRequest) noteTotalBudgetCut() int {
	if r.totalClock == nil {
		r.totalClock = &relayTotalClock{}
	}
	clock := r.totalClock
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.cuts++
	return clock.cuts
}

// totalTimeoutError is the failure handed to the failover/rescue machinery when the
// ceiling fires before any content was delivered.
func (ra *relayAttempt) totalTimeoutError() error {
	budget := ra.totalBudget
	ordinal := ra.noteTotalBudgetCut()
	log.Warnf("relay request total timeout (%s, cut #%d), %s", budget, ordinal, ra.totalBudgetDisposition())
	return &localRelayError{
		status:   http.StatusGatewayTimeout,
		code:     "octopus_upstream_total_timeout",
		strategy: "request_total_timeout;upstream_forwarded=true",
		message:  fmt.Sprintf("relay request total timeout (%s) before any content was delivered", budget),
	}
}

// totalBudgetDisposition describes, for the log line only, which branch the ceiling took.
func (ra *relayAttempt) totalBudgetDisposition() string {
	if ra.wroteMeaningfulDownstream {
		return "content was already delivered, ending honestly"
	}
	return "no content delivered, switching channel"
}

// The ceiling's streaming behaviour has two knobs. They are vars (not consts) so tests can
// shrink them; production keeps the defaults below.
var (
	// totalTimeoutRecheckInterval is how long a cut point waits before looking at the stream
	// again once the ceiling passed while content was still flowing.
	totalTimeoutRecheckInterval = 30 * time.Second
	// totalTimeoutFlowGrace is the window in which real content counts as "still producing".
	// It matches the recheck interval: content seen within the last check means the answer is
	// still coming, so the ceiling keeps standing down (the stream-silence clock remains the
	// mechanism that ends a stream which went quiet).
	totalTimeoutFlowGrace = 30 * time.Second
)

// committedTotalTimeoutError is the failure used on the honest-ending branch. It is a
// plain error on purpose: the central attempt-failure path already writes the caller's
// protocol-specific terminal frame (writeCommittedStreamFailure) once real content has
// been delivered, and returning the typed localRelayError there would suppress it.
func (ra *relayAttempt) committedTotalTimeoutError() error {
	budget := ra.totalBudget
	ra.noteTotalBudgetCut()
	log.Warnf("relay request total timeout (%s) after content was delivered, ending the stream honestly", budget)
	return fmt.Errorf("relay request total timeout (%s) after content was delivered", budget)
}

// armTotalTimeout arms the ceiling for the current attempt. The budget is whatever is
// LEFT of the request deadline (never a fresh full window), which is what makes the
// ceiling absolute across attempts: a retry on another channel inherits the remainder
// instead of restarting the clock. Returns a nil channel and a no-op stopper when the
// ceiling is disabled; an already-expired deadline yields a channel that is closed at
// once, so the caller cuts without waiting.
func (ra *relayAttempt) armTotalTimeout() (<-chan time.Time, func()) {
	remaining, active := ra.totalBudgetRemaining()
	if !active {
		return nil, func() {}
	}
	if remaining <= 0 {
		expired := make(chan time.Time)
		close(expired)
		return expired, func() {}
	}
	timer := time.NewTimer(remaining)
	return timer.C, func() { timer.Stop() }
}

// rearmTotalTimeoutCheck returns a channel that fires after one recheck interval. The
// streaming cut point uses it when its ceiling passed while real content was still flowing:
// instead of ending a live answer it waits, looks again, and only cuts once content has
// actually stopped (or the stream-silence clock ends it first).
func (ra *relayAttempt) rearmTotalTimeoutCheck() (<-chan time.Time, func()) {
	timer := time.NewTimer(totalTimeoutRecheckInterval)
	return timer.C, func() { timer.Stop() }
}

// boundUpstreamLifetime derives the context used for ONE upstream attempt, cancelled when
// the request ceiling passes. It is the uniform net under every relay path: the streaming
// paths cut the stream on their own timer (so they can pick the right terminal frame), but
// a path that has no such timer — the plain non-stream body read — is still bounded here
// instead of holding the caller until the client gives up.
//
// The cause is errRequestTotalTimeout so classifyTotalTimeout can tell our own ceiling
// apart from a client cancellation. On the normal (unexpired) path the returned context is
// the parent itself, so nothing about the request changes.
func (ra *relayAttempt) boundUpstreamLifetime(parent context.Context) context.Context {
	remaining, active := ra.totalBudgetRemaining()
	if !active {
		return parent
	}
	if remaining <= 0 {
		// Already out of budget: do not spend another upstream request. The caller sees
		// the ordinary attempt failure and the loop moves on without contacting anyone.
		ctx, cancel := context.WithCancelCause(parent)
		cancel(errRequestTotalTimeout)
		return ctx
	}
	ctx, cancel := context.WithCancelCause(parent)
	var timer *time.Timer
	// The net re-checks instead of firing blind: a ceiling that passed while the upstream was
	// still producing content must not abort a live answer, so schedule looks at content
	// progress and either stands down for another interval or cancels for real. Without this
	// the context timer would kill a dribbling-but-alive stream even though the streaming cut
	// point now refuses to.
	var schedule func()
	schedule = func() {
		left, active := ra.totalBudgetRemaining()
		if !active {
			return
		}
		if left > 0 {
			timer = time.AfterFunc(left, schedule)
			return
		}
		if ra.wroteMeaningfulDownstream && !ra.meaningfulContentIdle() {
			log.Warnf("relay request total timeout (%s) passed while content is still flowing, letting the stream run", ra.totalBudget)
			timer = time.AfterFunc(totalTimeoutRecheckInterval, schedule)
			return
		}
		cancel(errRequestTotalTimeout)
	}
	schedule()
	// Clean both up when the request itself ends (client gone, response finished): a
	// ceiling timer must never outlive its request. The child context is deliberately
	// NOT cancelled here — the response body is still being read by the caller.
	context.AfterFunc(parent, func() {
		timer.Stop()
		cancel(errRequestTotalTimeout)
	})
	return ctx
}

// classifyTotalTimeout rewrites the generic "context canceled" a fired ceiling produces
// into the same typed failure the streaming paths return, so logs, metrics and the
// caller-facing error name the real cause. It only ever fires when the request was still
// alive (a client cancellation keeps its own attribution) and the deadline had passed.
func (ra *relayAttempt) classifyTotalTimeout(err error) error {
	if err == nil || !ra.totalBudgetActive() {
		return err
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	remaining, _ := ra.totalBudgetRemaining()
	if remaining > 0 {
		// The ceiling had not passed, so this cancellation is somebody else's.
		return err
	}
	if ra.c != nil && ra.c.Request != nil && ra.c.Request.Context().Err() != nil {
		// The caller's own context is gone: that is a client abort, not our ceiling.
		return err
	}
	if ra.wroteMeaningfulDownstream && !ra.meaningfulContentIdle() {
		// Content is still being produced: this cancellation is not the ceiling acting on a
		// stalled answer, so keep its own attribution instead of relabelling it.
		return err
	}
	if ra.wroteMeaningfulDownstream {
		return ra.committedTotalTimeoutError()
	}
	return ra.totalTimeoutError()
}
