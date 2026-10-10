package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

// Half-streams that stop producing bytes without closing the connection are the
// production complaint this file pins: the relay sat until the CALLER gave up
// (measured: 369s and 529s, logged as octopus_client_canceled) instead of ending the
// attempt itself. Two clocks exist for that and both are event/byte driven on the
// UPSTREAM side:
//
//   - the event-interval timer (relay_stream_data_interval_timeout_seconds), reset
//     only inside the upstream read branch;
//   - the absolute pre-content deadline (first_token_time_out_default <- group
//     FirstTokenTimeOut), released only by real content and therefore immune to the
//     content-free event floods a stuck upstream keeps emitting.
//
// The tests below pin: our own keepalives never renew either clock, a silent
// upstream is cut and rescued, an event flood cannot buy unlimited time, and a stall
// AFTER content was delivered ends with an honest terminal frame instead of silence.
// Thresholds here are seconds (test speed); the shipped defaults are documented at
// dbmodel.DefaultFirstTokenTimeOutSeconds / DefaultRelayStreamDataIntervalTimeoutSeconds.

func setIdleSetting(t *testing.T, key dbmodel.SettingKey, value string) {
	t.Helper()
	if err := op.SettingSetString(key, value); err != nil {
		t.Fatalf("set %s=%s: %v", key, value, err)
	}
}

// A content-free opener in each inbound protocol's own dialect. None of these is
// "meaningful": they must not release the pre-content deadline, and they must not
// renew the event-interval timer into infinity either.
func chatOpenerEvent() string { return chatRoleDeltaEvent("1") }

func responsesOpenerEvent(id string) string {
	return `data: {"type":"response.created","response":{"id":"` + id + `","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n"
}

func anthropicOpenerEvent(id string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"` + id + `","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n"
}

// writeLoopThenStall writes payload forever until the reader goes away, then keeps the
// connection open and silent. This is the "upstream produced some bytes and stopped
// producing bytes, without ending the stream" shape: an EOF-based recovery can never
// fire here, only a clock can.
func writeLoopThenStall(w http.ResponseWriter, payload string, every time.Duration, stop <-chan struct{}) {
	if f, ok := w.(http.Flusher); ok {
		defer f.Flush()
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, err := w.Write([]byte(payload)); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}

// TestStreamIdleTimerFiresWhileOurKeepalivesKeepFlowing pins the direction of the
// event-interval clock: it is reset by upstream events, never by the SSE comments we
// write ourselves. If our keepalives renewed it, a stalled upstream would hold the
// request open forever — which is exactly the reported symptom.
func TestStreamIdleTimerFiresWhileOurKeepalivesKeepFlowing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamKeepaliveSec, "1")
	setIdleSetting(t, dbmodel.SettingKeyFirstByteKeepaliveDelaySeconds, "1")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "2")

	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)
	ra.firstTokenTimeOutSec = 0 // isolate: only the event-interval clock may cut this

	pr, pw := io.Pipe()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer pw.Close()
		if _, err := io.WriteString(pw, chatOpenerEvent()); err != nil {
			return
		}
		<-stop // connected, alive, and never sending another byte
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	start := time.Now()
	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSE event") {
		t.Fatalf("a silent upstream must be cut by the event-interval clock, got err=%v after %s", err, elapsed)
	}
	// With renewal from our own writes the loop would run until the writer's stall
	// ended (never, in production). It must fire at the configured 2s instead.
	if elapsed > 4*time.Second {
		t.Fatalf("event-interval clock must fire at the configured 2s, took %s", elapsed)
	}
	if body := rec.Body.String(); !strings.Contains(body, ":\n\n") {
		t.Fatalf("precondition not met: our keepalives must have flowed while the clock ran, body=%q", body)
	}
}

// TestOpenerThenSilentUpstreamIsRescuedWithinTheDeadline is the end-to-end rescue the
// caller asked for: the first channel opens the stream, sends nothing meaningful, and
// never ends. Nothing meaningful reached the caller, so the attempt must be failed and
// the next channel must serve the answer inside the same caller window — unaware.
func TestOpenerThenSilentUpstreamIsRescuedWithinTheDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	// The pre-content deadline rescues; keep the event-interval clock far away so this
	// test can only pass through the deadline.
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "1")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamKeepaliveSec, "1")

	var deadHits, liveHits int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deadHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatOpenerEvent()))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(dead.Close)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("RESCUED")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(live.Close)

	createChatChannel(t, dead.URL, "stalled-mid-stream", 1)
	createChatChannel(t, live.URL, "healthy-after-rescue", 2)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if atomic.LoadInt32(&deadHits) == 0 || atomic.LoadInt32(&liveHits) == 0 {
		t.Fatalf("expected the stalled channel to be abandoned and the healthy one to serve, dead=%d live=%d",
			deadHits, liveHits)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the rescue to deliver a 200 stream, got %d body %s", rec.Code, rec.Body.String())
	}
	if elapsed > 6*time.Second {
		t.Fatalf("rescue must happen inside the deadline (~1s), took %s", elapsed)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "RESCUED") {
		t.Fatalf("the healthy channel's answer must reach the caller, got %q", body)
	}
	if strings.Contains(body, `"type":"upstream_error"`) || strings.Contains(body, `"error"`) {
		t.Fatalf("a successful rescue must not surface an error frame, got %q", body)
	}
	if strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("the abandoned channel's opener must not leak into the rescued stream, got %q", body)
	}
}

// TestResponsesOpenerThenSilentUpstreamIsRescued covers the same rescue on the
// responses path, which is what the codex-side small requests ride on.
func TestResponsesOpenerThenSilentUpstreamIsRescued(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "1")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")

	var deadHits, liveHits int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deadHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(responsesOpenerEvent("resp_stalled")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(dead.Close)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(responsesOpenerEvent("resp_ok")))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"RESCUED"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_ok","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(live.Close)

	first := dbmodel.Channel{
		Name:     "responses-stalled-mid-stream",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		Model:    "gpt-5.5",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: dead.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "dead-key"}},
	}
	if err := op.ChannelCreate(&first, context.Background()); err != nil {
		t.Fatalf("create stalled channel: %v", err)
	}
	second := dbmodel.Channel{
		Name:     "responses-healthy-after-rescue",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		Model:    "gpt-5.5",
		Priority: 2,
		BaseUrls: []dbmodel.BaseUrl{{URL: live.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "live-key"}},
	}
	if err := op.ChannelCreate(&second, context.Background()); err != nil {
		t.Fatalf("create healthy channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-5.5","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIResponse, c)
	elapsed := time.Since(start)

	if atomic.LoadInt32(&deadHits) == 0 || atomic.LoadInt32(&liveHits) == 0 {
		t.Fatalf("expected both channels to be attempted, dead=%d live=%d", deadHits, liveHits)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the rescue to deliver a 200 stream, got %d body %s", rec.Code, rec.Body.String())
	}
	if elapsed > 6*time.Second {
		t.Fatalf("rescue must happen inside the deadline (~1s), took %s", elapsed)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "RESCUED") {
		t.Fatalf("the healthy channel's answer must reach the caller, got %q", body)
	}
	if strings.Contains(body, "resp_stalled") {
		t.Fatalf("the abandoned channel's opener must not leak into the rescued stream, got %q", body)
	}
}

// TestContentFreeEventFloodCannotExtendTheDeadline is the P1 pin: the ONLY clock that
// survives an upstream which keeps emitting non-meaningful events (openers, bare role
// deltas, data-bearing pings) is the absolute pre-content deadline — the event-interval
// timer is reset by every one of those events, so before this deadline shipped enabled
// such a request could be kept alive indefinitely with the caller seeing heartbeats only.
func TestContentFreeEventFloodCannotExtendTheDeadline(t *testing.T) {
	cases := []struct {
		name    string
		inbound inbound.InboundType
		path    string
		payload string
	}{
		{"chat-role-delta-flood", inbound.InboundTypeOpenAIChat, "/v1/chat/completions", chatOpenerEvent()},
		{"responses-created-flood", inbound.InboundTypeOpenAIResponse, "/v1/responses", responsesOpenerEvent("resp_flood")},
		{"anthropic-message-start-flood", inbound.InboundTypeAnthropic, "/v1/messages", anthropicOpenerEvent("msg_flood")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupRelayErrorDB(t)
			setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "1")
			// Far beyond the test window: if this is what fires, the flood won.
			setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

			stop := make(chan struct{})
			defer close(stop)

			var hits int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeLoopThenStall(w, tc.payload, 50*time.Millisecond, stop)
			}))
			t.Cleanup(upstream.Close)

			channel := dbmodel.Channel{
				Name:     "content-free-flood",
				Type:     outboundChannelTypeFor(tc.inbound),
				Enabled:  true,
				Model:    "flood-model",
				Priority: 1,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "flood-key"}},
			}
			if err := op.ChannelCreate(&channel, context.Background()); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(
				`{"model":"flood-model","stream":true,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			start := time.Now()
			Handler(tc.inbound, c)
			elapsed := time.Since(start)

			if atomic.LoadInt32(&hits) == 0 {
				t.Fatalf("upstream was never called")
			}
			if elapsed > 5*time.Second {
				t.Fatalf("an event flood must not buy unlimited time: the absolute deadline (~1s) has to end it, took %s", elapsed)
			}
			if rec.Code < 400 {
				t.Fatalf("nothing meaningful was ever delivered, so the caller must get an explicit failure, got %d body %q",
					rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); strings.Contains(body, `"in_progress"`) {
				t.Fatalf("the flood's openers must not be handed to the caller as an answer, got %q", body)
			}
		})
	}
}

// TestCommittedContentThenStallEndsWithAnHonestFrame: once real content has reached the
// caller the channel can no longer be swapped. Silence after that must still end with
// the caller's own terminal frame — never with "wait until the client times out".
func TestCommittedContentThenStallEndsWithAnHonestFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	// The event-interval clock is the cutter here; the pre-content deadline must be out
	// of the way because content DOES arrive.
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1")

	stop := make(chan struct{})
	defer close(stop)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("PARTIAL-ANSWER")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stop // and then nothing, with the connection still open
	}))
	t.Cleanup(upstream.Close)

	createChatChannel(t, upstream.URL, "committed-then-silent", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if elapsed > 6*time.Second {
		t.Fatalf("a stall after content must end at the configured 1s, took %s", elapsed)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PARTIAL-ANSWER") {
		t.Fatalf("already-delivered content must be preserved, got %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("the caller must get an explicit terminal error frame, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("the stream must be terminated in the caller's protocol, got %q", body)
	}
	if strings.Contains(body, "committed-then-silent") || strings.Contains(body, upstream.URL) {
		t.Fatalf("the terminal frame must not leak channel or upstream identity, got %q", body)
	}
}

// TestForcedStreamAggregationOpenerThenSilentIsCut covers the aggregation path used
// when the caller asked for a non-streamed body: it has its own copy of both clocks,
// and it is the path the "small side request" traffic takes.
func TestForcedStreamAggregationOpenerThenSilentIsCut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "1")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	stop := make(chan struct{})
	defer close(stop)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SSE body for a non-stream caller: the aggregation path has to handle it.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatOpenerEvent()))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stop
	}))
	t.Cleanup(upstream.Close)

	createChatChannel(t, upstream.URL, "aggregation-stalled", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(start)

	if elapsed > 6*time.Second {
		t.Fatalf("the aggregation path must cut a silent upstream at the deadline (~1s), took %s", elapsed)
	}
	if rec.Code < 400 {
		t.Fatalf("a non-stream caller must get an explicit error instead of a hang/empty body, got %d body %q",
			rec.Code, rec.Body.String())
	}
}

// TestContentFreeEventFloodKeepsRenewingTheIntervalTimer is the falsifiable form of the
// diagnosis this whole change rests on. Read the three reset sites yourself:
//
//	relay.go:2688  (stream path)          resetDataTimeout() runs BEFORE classifyStreamEvent,
//	relay.go:3148  (aggregation path)     same position, no content condition,
//	images.go:1259 (side path)            resets on every upstream SSE LINE.
//
// So any event read from the upstream renews the interval timer, content or not. Here the
// absolute pre-content deadline is DISABLED (that is the old shipped configuration) and
// the interval timer is 2s: if a content-free flood renewed the timer, this request is
// unbounded and the test must see it still running well past 2s. If the flood did NOT
// renew it, the relay would end the request at ~2s and this test fails.
func TestContentFreeEventFloodKeepsRenewingTheIntervalTimer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "0") // old shipped default
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "2")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamKeepaliveSec, "1")
	setIdleSetting(t, dbmodel.SettingKeyFirstByteKeepaliveDelaySeconds, "1")

	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)
	ra.firstTokenTimeOutSec = 0

	pr, pw := io.Pipe()
	floodStop := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		defer pw.Close()
		for {
			select {
			case <-floodStop:
				<-release // keep the connection open and silent
				return
			default:
			}
			if _, err := io.WriteString(pw, chatOpenerEvent()); err != nil {
				return
			}
			select {
			case <-floodStop:
				<-release
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	done := make(chan error, 1)
	go func() {
		done <- ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	}()

	// 2s interval timer, a content-free event every 100ms: only renewal can explain
	// survival here.
	select {
	case err := <-done:
		t.Fatalf("a content-free event flood must not be bounded by the interval timer, but the request ended: %v", err)
	case <-time.After(5 * time.Second):
	}

	close(floodStop) // flood stops, connection stays open
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSE event") {
			t.Fatalf("once the flood stops, the interval timer is what must fire, got err=%v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("the interval timer did not fire after the flood stopped; it must be armed")
	}
}

// TestCommittedContentThenStallHonestFramePerProtocol covers the real production shape B
// (a stream that had already delivered content and then went quiet for 529s until the
// caller left) in each protocol we serve. A channel cannot be swapped once content
// reached the caller, so the only acceptable ending is the caller's own terminal frame.
func TestCommittedContentThenStallHonestFramePerProtocol(t *testing.T) {
	const partialText = "PARTIAL-ANSWER-TEXT"

	cases := []struct {
		name         string
		inbound      inbound.InboundType
		outboundType outbound.OutboundType
		path         string
		body         string
		upstream     string
		wantError    string
		wantTerminal string
	}{
		{
			name:         "chat",
			inbound:      inbound.InboundTypeOpenAIChat,
			outboundType: outbound.OutboundTypeOpenAIChat,
			path:         "/v1/chat/completions",
			body:         `{"model":"stall-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream:     chatOpenerEvent() + chatContentEvent(partialText),
			wantError:    `"type":"upstream_error"`,
			wantTerminal: "data: [DONE]",
		},
		{
			name:         "responses",
			inbound:      inbound.InboundTypeOpenAIResponse,
			outboundType: outbound.OutboundTypeOpenAIResponse,
			path:         "/v1/responses",
			body:         `{"model":"stall-model","input":"hi","stream":true}`,
			upstream: responsesOpenerEvent("resp_partial") +
				`data: {"type":"response.output_text.delta","delta":"` + partialText + `"}` + "\n\n",
			wantError:    "event: response.failed",
			wantTerminal: "data: [DONE]",
		},
		{
			// The real stream B ran through an Anthropic-type channel: this row exists
			// because of that, and it is the protocol most likely to be assumed covered.
			name:         "anthropic",
			inbound:      inbound.InboundTypeAnthropic,
			outboundType: outbound.OutboundTypeAnthropic,
			path:         "/v1/messages",
			body:         `{"model":"stall-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
			upstream: anthropicOpenerEvent("msg_partial") +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + partialText + `"}}` + "\n\n",
			wantError:    "event: error",
			wantTerminal: `"type":"error"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupRelayErrorDB(t)
			// Post-content silence budget is the clock under test here; the pre-content
			// deadline must be far away because content DOES arrive.
			setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
			setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1")

			stop := make(chan struct{})
			defer close(stop)

			channelName := "committed-stall-" + tc.name
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.upstream))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-stop // content delivered, then nothing, connection still open
			}))
			t.Cleanup(upstream.Close)

			channel := dbmodel.Channel{
				Name:     channelName,
				Type:     tc.outboundType,
				Enabled:  true,
				Model:    "stall-model",
				Priority: 1,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "stall-key"}},
			}
			if err := op.ChannelCreate(&channel, context.Background()); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			start := time.Now()
			Handler(tc.inbound, c)
			elapsed := time.Since(start)

			if elapsed > 6*time.Second {
				t.Fatalf("a stall after delivered content must end at the configured 1s, took %s", elapsed)
			}
			body := rec.Body.String()
			if !strings.Contains(body, partialText) {
				t.Fatalf("content already delivered to the caller must be preserved, got %q", body)
			}
			if !strings.Contains(body, tc.wantError) {
				t.Fatalf("the caller must get %q as an explicit terminal error, got %q", tc.wantError, body)
			}
			if !strings.Contains(body, tc.wantTerminal) {
				t.Fatalf("the stream must be terminated in the caller's own protocol (%q), got %q", tc.wantTerminal, body)
			}
			if strings.Contains(body, channelName) || strings.Contains(body, upstream.URL) {
				t.Fatalf("the terminal frame must not leak channel or upstream identity, got %q", body)
			}
		})
	}
}

func createChatChannel(t *testing.T, upstreamURL, name string, priority int) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:     name,
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gpt-5.5",
		Priority: priority,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: fmt.Sprintf("%s-key", name)}},
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
}

func outboundChannelTypeFor(inboundType inbound.InboundType) outbound.OutboundType {
	switch inboundType {
	case inbound.InboundTypeAnthropic:
		return outbound.OutboundTypeAnthropic
	case inbound.InboundTypeOpenAIResponse:
		return outbound.OutboundTypeOpenAIResponse
	default:
		return outbound.OutboundTypeOpenAIChat
	}
}
