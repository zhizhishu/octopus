package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
)

// The live anchor: an upstream answered 200 with ZERO bytes, and the relay sent the same request
// body to the same channel five more times at uneven intervals (62s/617s/33s/142s), some of the
// answers being 1024/2048-byte fragments. Two things are wrong with that and both are pinned
// here:
//
//  1. a 200 that carries no bytes is a BAD response: it must be retried with backoff, may not be
//     reported to the caller as a normal turn, and may not be re-sent forever;
//  2. the same request body may only be sent to the same channel a bounded number of times per
//     request — at the cap the relay switches channel or ends the request;
//
// and the caller-facing rule that decides whether any of it is acceptable: a bad response must
// surface either as an explicit error or as complete content. A truncated body is worse than an
// error, so the assertions below refuse it.

// emptyUpstream answers every request with 200 and an immediate EOF (no bytes, no headers-flush
// worth reading) and records when each dispatch arrived.
func emptyUpstream(t *testing.T) (*httptest.Server, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var stamps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Nothing follows: exactly the "200 / 0 B" the anchor recorded.
	}))
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	return server, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), stamps...)
	}
}

// callerVerdict classifies what the caller ended up holding.
type callerVerdict struct {
	code      int
	body      string
	hasError  bool
	hasDone   bool
	hasFinish bool
	content   int
}

func classifyCaller(rec *httptest.ResponseRecorder) callerVerdict {
	body := rec.Body.String()
	return callerVerdict{
		code:      rec.Code,
		body:      body,
		hasError:  strings.Contains(body, `"error"`) || rec.Code >= 400,
		hasDone:   strings.Contains(body, "[DONE]"),
		hasFinish: strings.Contains(body, `"finish_reason":"stop"`),
		content:   strings.Count(body, `"content"`),
	}
}

// TestEmpty200UpstreamIsRetriedWithBackoffAndBounded is the anchor's case: nothing but 200/0 B.
func TestEmpty200UpstreamIsRetriedWithBackoffAndBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 2500*time.Millisecond)

	const requestModel = "empty200-model"
	upstream, stamps := emptyUpstream(t)
	channel := newRouteFallbackChannel(t, ctx, upstream.URL, "empty200-channel", requestModel, "empty200-upstream")
	withAccessRouteTo(t, ctx, requestModel, channel.ID, "empty200-upstream")

	rec, c := newStreamGinFor(requestModel)
	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	measured := stamps()
	verdict := classifyCaller(rec)
	sub50 := subFiftyMillisecondPairs(measured)
	t.Logf("bad-response handling: dispatches=%d sub-50ms pairs=%d elapsed=%s verdict=%+v series=%v",
		len(measured), sub50, elapsed, verdict, measured)
	if sub50 != 0 {
		t.Fatalf("sub-50ms pairs=%d (must be 0): a bad response must be retried with backoff, never back-to-back (series=%v)",
			sub50, measured)
	}

	// 1. The caller must never be told this was a normal turn.
	if verdict.hasFinish && !verdict.hasError {
		t.Fatalf("a 200 with no bytes must not look like a completed answer, body=%q", verdict.body)
	}
	// 2. Either an explicit error or complete content — never a silent half-answer.
	if !verdict.hasError && !verdict.hasDone {
		t.Fatalf("the caller must get an explicit error or a terminated stream, got %d body=%q", verdict.code, verdict.body)
	}
	// 3. Same body, same channel: bounded repeats, and each repeat is paced (no hot resend).
	if len(measured) > 4 {
		t.Fatalf("the same channel was sent the same body %d times; the cap is 4 per request", len(measured))
	}
	for i := 1; i < len(measured); i++ {
		if gap := measured[i].Sub(measured[i-1]); gap < 200*time.Millisecond {
			t.Fatalf("two dispatches %s apart: a bad response must be retried with backoff, not immediately (series=%v)",
				gap, measured)
		}
	}
}
