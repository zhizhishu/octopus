package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/gin-gonic/gin"
)

// errFirstCritical / errLaterDifferent model a body whose first read returns
// bytes together with a terminal error, and whose later reads (which must never
// happen once the error is known) would return a different error.
var (
	errFirstCritical  = errors.New("first critical read error")
	errLaterDifferent = errors.New("later different error")
)

// errSequenceBody: first Read returns (first, firstErr); every later Read
// returns (0, laterErr) and is counted. The counter lets tests prove the
// underlying body is NOT read again after the pending error is known.
type errSequenceBody struct {
	first    string
	firstErr error
	laterErr error
	reads    int32
	served   bool
}

func (b *errSequenceBody) Read(p []byte) (int, error) {
	atomic.AddInt32(&b.reads, 1)
	if !b.served {
		b.served = true
		n := copy(p, b.first)
		return n, b.firstErr
	}
	return 0, b.laterErr
}

func (b *errSequenceBody) Close() error { return nil }

// F9: peekedBody must return the peek-consumed error as soon as the replayed
// bytes are drained — BEFORE any new underlying read. bufio.Peek consumed the
// one-shot readErr, so br.Read would go back to the underlying body: a stalled
// body would delay the known error indefinitely, and a body returning a
// different error would overwrite the first critical one.
func TestPeekedBodyReturnsPendingErrorBeforeUnderlyingRead(t *testing.T) {
	underlying := &errSequenceBody{first: "hello", firstErr: errFirstCritical, laterErr: errLaterDifferent}
	br := bufio.NewReaderSize(underlying, sseFramingPeekWindow)
	peek, peekErr := br.Peek(sseFramingPeekWindow)
	if string(peek) != "hello" || !errors.Is(peekErr, errFirstCritical) {
		t.Fatalf("peek setup mismatch: peek=%q err=%v", peek, peekErr)
	}
	if got := atomic.LoadInt32(&underlying.reads); got != 1 {
		t.Fatalf("setup must have read the underlying exactly once, got %d", got)
	}

	p := &peekedBody{br: br, orig: underlying, pending: peekErr}
	buf := make([]byte, 64)

	// 1) replayed bytes come back first, error deferred.
	n, err := p.Read(buf)
	if n != len("hello") || err != nil {
		t.Fatalf("first Read = (%d, %v), want (%d, nil)", n, err, len("hello"))
	}

	// 2) buffer drained: the pending error must surface WITHOUT another
	//    underlying read, and must not be replaced by the later error.
	n, err = p.Read(buf)
	if n != 0 || !errors.Is(err, errFirstCritical) {
		t.Fatalf("post-drain Read = (%d, %v), want (0, %v)", n, err, errFirstCritical)
	}
	if got := atomic.LoadInt32(&underlying.reads); got != 1 {
		t.Fatalf("underlying must not be read again after the pending error is known, reads=%d", got)
	}
}

// stalledPreludeBody delivers a short prelude (fewer bytes than the peek
// window, no decidable SSE line) and then parks the read until Close. This is
// the F8 shape: an upstream that opens the body, sends a partial prefix, and
// stalls with a missing/mislabeled Content-Type.
type stalledPreludeBody struct {
	prelude   string
	stall     chan struct{}
	closed    chan struct{}
	delivered bool
}

func newStalledPreludeBody(prelude string) *stalledPreludeBody {
	return &stalledPreludeBody{prelude: prelude, stall: make(chan struct{}), closed: make(chan struct{})}
}

func (b *stalledPreludeBody) Read(p []byte) (int, error) {
	if !b.delivered {
		b.delivered = true
		return copy(p, b.prelude), nil
	}
	<-b.stall
	return 0, errors.New("body closed during stalled identification")
}

func (b *stalledPreludeBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
		close(b.stall)
	}
	return nil
}

func (b *stalledPreludeBody) wasClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

// F8: the identification peek must be bounded by the first-token timeout. A
// stalled upstream (partial prefix, no decidable framing, connection held
// open) previously blocked Peek(512) forever — the aggregation timers and
// heartbeats never started, and the non-stream client hung until its own
// deadline. Now the attempt fails fast (existing channel-switch behavior) and
// the real body is closed so the parked probe read ends.
func TestHandleResponseIdentificationBoundedByFirstTokenTimeout(t *testing.T) {
	rec, c, ra := newSelfHealChatAttempt(t)
	ra.firstTokenTimeOutSec = 1
	body := newStalledPreludeBody("even") // partial field name, not yet decidable
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:       body,
	}

	done := make(chan error, 1)
	go func() {
		done <- ra.handleResponse(c.Request.Context(), response, &openai.ChatOutbound{})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled identification must fail the attempt, not succeed")
		}
		if !strings.Contains(err.Error(), "first-token timeout") {
			t.Fatalf("expected the identification timeout error, got: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("identification peek must be bounded by the first-token timeout (F8): still blocked after 6s")
	}
	if !body.wasClosed() {
		t.Fatal("timeout must close the real body so the parked probe read can end")
	}
	// httptest 录制器 Code 默认就是 200, "没写字节"要用 Body 判: 识别超时发生在
	// 任何下游写出之前, 不能把客户端留在一个已提交的 200 上。
	if rec.Body.Len() != 0 {
		t.Fatalf("no bytes must be committed to the client on an identification timeout, got %q", rec.Body.String())
	}
}

// F8: client cancellation must also break a stalled identification promptly.
func TestHandleResponseIdentificationCancelable(t *testing.T) {
	_, c, ra := newSelfHealChatAttempt(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	ra.firstTokenTimeOutSec = 30          // long budget: only cancellation can break it
	body := newStalledPreludeBody("data") // partial field name, not decidable
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       body,
	}

	done := make(chan error, 1)
	go func() {
		done <- ra.handleResponse(ctx, response, &openai.ChatOutbound{})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled identification must fail, not succeed")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the context cancellation to surface, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation must break a stalled identification promptly (F8)")
	}
	if !body.wasClosed() {
		t.Fatal("cancellation must close the real body so the parked probe read can end")
	}
}

// slowThenStallBody waits for the given delay, then delivers a decidable SSE
// line and stalls (no EOF). Used to measure the budget handoff: the
// identification consumes `delay`, so the aggregation must only get the
// REMAINING first-token budget, not a fresh full window.
type slowThenStallBody struct {
	delay   time.Duration
	line    string
	stall   chan struct{}
	closed  chan struct{}
	started bool
}

func (b *slowThenStallBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		time.Sleep(b.delay)
		return copy(p, b.line), nil
	}
	<-b.stall
	return 0, errors.New("body closed during stalled aggregation")
}

func (b *slowThenStallBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
		close(b.stall)
	}
	return nil
}

// F8 budget accounting: the identification elapsed time counts against the
// aggregation's first-token budget. With a 700ms identification delay and a 1s
// budget, the stalled aggregation must fail around the 1s mark — a fresh full
// window would push it to ~1.7s.
func TestStreamAggregationBudgetCountsIdentificationTime(t *testing.T) {
	_, c, ra := newSelfHealChatAttempt(t)
	ra.firstTokenTimeOutSec = 1
	// 前奏事件不带内容(空 delta): 带内容的分片会解除首 token 守卫, 测不到预算。
	// 识别 ~0.7s 后交给聚合器, 聚合器只拿剩余 ~0.3s 预算等"有意义的首 token"。
	body := &slowThenStallBody{
		delay:  700 * time.Millisecond,
		stall:  make(chan struct{}),
		closed: make(chan struct{}),
		line:   `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":null}]}` + "\n\n",
	}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{}, // no content-type: forces the peek path
		Body:       body,
	}

	startedAt := time.Now()
	err := ra.handleResponse(c.Request.Context(), response, &openai.ChatOutbound{})
	elapsed := time.Since(startedAt)
	defer response.Body.Close()

	if err == nil {
		t.Fatal("a stalled aggregation after the first chunk must fail the attempt")
	}
	// 识别 ~0.7s + 剩余预算 ~0.3s(+调度余量) ≈ 1s。若重新赠送完整 1s 窗口,
	// 失败点会到 ~1.7s。上限 1.4s 区分两种语义; 下限 0.9s 防止误判成
	// "识别阶段直接失败"。
	if elapsed < 900*time.Millisecond || elapsed > 1400*time.Millisecond {
		t.Fatalf("aggregation must fail at ~1s (identification counted against the budget), took %v", elapsed)
	}
}

// F8 regression guard: a healthy SSE body with a missing Content-Type still
// self-heals through the bounded identification path, byte-complete.
func TestBoundedIdentificationStillSelfHealsSSE(t *testing.T) {
	rec, c, ra := newSelfHealChatAttempt(t)
	ra.firstTokenTimeOutSec = 5
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(selfHealSSEChatBody)),
	}
	if err := ra.handleResponse(c.Request.Context(), response, &openai.ChatOutbound{}); err != nil {
		t.Fatalf("bounded identification must not break the SSE self-heal, got: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 aggregated response, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Hello world") {
		t.Fatalf("expected aggregated content, got %q", body)
	}
}

// F8 regression guard: an ordinary JSON body with a missing Content-Type is
// still NOT misjudged as SSE and its bytes stay complete.
func TestBoundedIdentificationLeavesPlainJSONUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  0,
		inAdapter:    declaredModelInbound{internal: nil},
		requestModel: "test-model",
	}}
	ra.firstTokenTimeOutSec = 5
	jsonBody := `{"id":"chatcmpl-1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
	}
	// declaredModelOutbound.TransformResponse returns a canned response after
	// draining the body; the peek must have replayed every byte (EOF reached).
	if err := ra.handleResponse(c.Request.Context(), response, declaredModelOutbound{declared: "test-model"}); err != nil {
		t.Fatalf("plain JSON with no content-type must pass through the bounded identification, got: %v", err)
	}
}
