package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
)

// The third clock pins the shape the other two cannot see: an upstream that never goes
// silent and never stops producing (production: a 154s turn that emitted 17k output
// tokens, and a turn still generating past six minutes). The event-interval clock is
// reset by every event, and the pre-content deadline is released by the first content,
// so only an absolute ceiling measured from the request start can end such a request.
//
// These tests drive the ceiling through requestTotalTimeoutOverride (milliseconds) so
// they do not wait the configured seconds; the shipped seconds value is asserted in the
// model package instead.

func withRequestCeiling(t *testing.T, window time.Duration) {
	t.Helper()
	previous := requestTotalTimeoutOverride
	requestTotalTimeoutOverride = window
	t.Cleanup(func() { requestTotalTimeoutOverride = previous })
}

func postChatStream(t *testing.T, c *gin.Context) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
}

// TestDribblingUpstreamIsCutByTheRequestCeiling is the case neither existing clock can
// catch: bytes keep arriving well inside the idle window, forever. Half a second of
// silence never happens, so the idle timer is useless here, and content arrived long ago,
// so the pre-content deadline is already released.
func TestDribblingUpstreamIsCutByTheRequestCeiling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	withRequestCeiling(t, 1200*time.Millisecond)
	// Both older clocks are parked far away on purpose: whatever ends this request must be
	// the ceiling, not a stall.
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	stop := make(chan struct{})
	defer close(stop)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A real content event every 200ms: never silent, never finished.
		writeLoopThenStall(w, chatContentEvent("DRIP"), 200*time.Millisecond, stop)
	}))
	t.Cleanup(upstream.Close)
	createChatChannel(t, upstream.URL, "dribbling-forever", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("an always-dribbling upstream must end at the 1.2s ceiling, took %s", elapsed)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DRIP") {
		t.Fatalf("content delivered before the ceiling must be preserved, got %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("the ceiling must end with an explicit error frame, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("the stream must be terminated in the caller's protocol, got %q", body)
	}
	if strings.Contains(body, "dribbling-forever") || strings.Contains(body, upstream.URL) {
		t.Fatalf("the terminal frame must not leak channel or upstream identity, got %q", body)
	}
	if strings.Contains(body, `"finish_reason"`) {
		t.Fatalf("the ceiling must not let the upstream pretend the answer finished, got %q", body)
	}
}

// TestDribblingOpenersFailOverBeforeTheCeiling covers the uncommitted branch the way the
// relay actually reaches it: a channel that never delivers content is cut (here by the
// pre-content clock, well inside the ceiling) and the request is handed to the next
// channel. The ceiling must not interfere with that rescue.
func TestDribblingOpenersFailOverBeforeTheCeiling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	withRequestCeiling(t, 30*time.Second)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "1")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	stopSlow := make(chan struct{})
	defer close(stopSlow)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeLoopThenStall(w, chatOpenerEvent(), 200*time.Millisecond, stopSlow)
	}))
	t.Cleanup(slow.Close)
	createChatChannel(t, slow.URL, "opener-dribble", 1)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("RESCUED-ANSWER")))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(fast.Close)
	createChatChannel(t, fast.URL, "healthy-answer", 2)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	body := rec.Body.String()
	if !strings.Contains(body, "RESCUED-ANSWER") {
		t.Fatalf("nothing was delivered, so the request must fail over to the healthy channel, got %q", body)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the rescue must happen well inside the ceiling, took %s", elapsed)
	}
}

// TestCeilingSpentBudgetStopsInsteadOfHammering pins the anti-hot-loop half of the
// ceiling: once the budget is spent, the relay must not spend another upstream request
// (production measured 138 attempts against one channel in 300s). The caller gets an
// explicit failure instead.
func TestCeilingSpentBudgetStopsInsteadOfHammering(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	withRequestCeiling(t, 1200*time.Millisecond)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	stop := make(chan struct{})
	defer close(stop)

	var upstreamRequests atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeLoopThenStall(w, chatOpenerEvent(), 200*time.Millisecond, stop)
	}))
	t.Cleanup(slow.Close)
	createChatChannel(t, slow.URL, "spent-budget-channel", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if got := upstreamRequests.Load(); got > 2 {
		t.Fatalf("the spent budget must not be re-spent on the same channel, upstream saw %d requests", got)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("an exhausted budget must end the request promptly, took %s", elapsed)
	}
	if body := rec.Body.String(); !strings.Contains(body, "octopus_upstream_total_timeout") {
		t.Fatalf("the caller must be told the request ceiling was reached, got %q", body)
	}
}

// TestCeilingAlsoBoundsTheRecoveryHold pins that the automatic-recovery hold cannot
// outlive the ceiling: without this clamp a request that ends "at the ceiling" would
// really end 300s (the rescue window) after it.
func TestCeilingAlsoBoundsTheRecoveryHold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	withRequestCeiling(t, 1500*time.Millisecond)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	stop := make(chan struct{})
	defer close(stop)

	// One channel only, and it never delivers content: after the ceiling cuts it there is
	// nowhere to fail over to, which is the case that used to sit in the rescue hold.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeLoopThenStall(w, chatOpenerEvent(), 200*time.Millisecond, stop)
	}))
	t.Cleanup(slow.Close)
	createChatChannel(t, slow.URL, "only-dribbling-channel", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if elapsed > 6*time.Second {
		t.Fatalf("the recovery hold must not extend past the 1.5s ceiling, took %s", elapsed)
	}
	if body := rec.Body.String(); !strings.Contains(body, "error") && !strings.Contains(body, "failed") {
		t.Fatalf("the caller must get an explicit failure, got %q", body)
	}
}

// TestCeilingLeavesANormalAnswerAlone: the ceiling is a last resort, not a target. A
// short healthy answer must pass through untouched even with a small ceiling.
func TestCeilingLeavesANormalAnswerAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	withRequestCeiling(t, 10*time.Second)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("COMPLETE-ANSWER")))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	createChatChannel(t, upstream.URL, "healthy", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	body := rec.Body.String()
	if !strings.Contains(body, "COMPLETE-ANSWER") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("a healthy answer must pass through untouched, got %q", body)
	}
	if strings.Contains(body, "upstream_error") {
		t.Fatalf("no error frame may be injected into a healthy answer, got %q", body)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a healthy answer must not wait for the ceiling, took %s", elapsed)
	}
}

// TestCeilingDisabledByZeroKeepsTheOldBehaviour: with the setting at 0 there is no
// ceiling, and a dribbling upstream is then unbounded by design — the older clocks cannot
// see it (one never goes silent, the other is released by the content it keeps sending).
// The test proves the ceiling is what ends such a request, by showing that without it the
// request is still running after several times the window the ceiling would have used.
func TestCeilingDisabledByZeroKeepsTheOldBehaviour(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyRelayRequestTotalTimeoutSec, "0")
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "60")

	stop := make(chan struct{})
	stopped := false
	stopOnce := func() {
		if !stopped {
			stopped = true
			close(stop)
		}
	}
	defer stopOnce()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeLoopThenStall(w, chatContentEvent("DRIP"), 100*time.Millisecond, stop)
	}))
	t.Cleanup(upstream.Close)
	createChatChannel(t, upstream.URL, "dribbling-disabled", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	postChatStream(t, c)

	done := make(chan struct{})
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("with the ceiling disabled a dribbling upstream must not be cut by a clock the relay does not have")
	case <-time.After(3 * time.Second):
		// Expected: nothing cuts it. Release the upstream so the request can end.
	}
	stopOnce()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the request did not end after the upstream stopped")
	}
}

// TestClientCancelIsNotRelabelledAsTheCeiling guards the classification: a caller that
// goes away must keep its own attribution instead of being reported as our ceiling.
func TestClientCancelIsNotRelabelledAsTheCeiling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// A real client context that has already been cancelled (the caller went away).
	cancelled := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	cancelledCtx, cancelClient := context.WithCancel(cancelled.Context())
	cancelClient()
	c.Request = cancelled.WithContext(cancelledCtx)

	req := &relayRequest{c: c, totalBudget: time.Second, totalDeadline: time.Now().Add(-time.Second)}
	ra := &relayAttempt{relayRequest: req}

	original := context.Canceled
	if got := ra.classifyTotalTimeout(original); got != original {
		t.Fatalf("a cancellation whose client context is gone must not be relabelled, got %v", got)
	}

	// With the client still connected and the ceiling passed, the failure IS ours.
	live := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	ctx, cancel := context.WithCancel(live.Context())
	defer cancel()
	c.Request = live.WithContext(ctx)
	relabelled := ra.classifyTotalTimeout(context.Canceled)
	if relabelled == nil || !strings.Contains(relabelled.Error(), "relay request total timeout") {
		t.Fatalf("a live request past its ceiling must be reported as such, got %v", relabelled)
	}
	if _, code, strategy, ok := localRelayErrorDetails(relabelled); !ok || code != "octopus_upstream_total_timeout" {
		t.Fatalf("expected the named ceiling failure, got code=%q strategy=%q ok=%v", code, strategy, ok)
	}
}
