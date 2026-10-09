package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// Upstream half-stream truncation: the upstream dropped the socket after real
// content had already been flushed to the client but WITHOUT any terminal marker
// (finish_reason / [DONE] / response.completed / message_stop). Before the fix the
// relay ran the "benign end" branch — commitPendingPrelude -> flushRedactRestore ->
// synthesizeStreamDone -> return nil — i.e. it FAKED a successful turn (no error
// frame, a synthesized success tail, and the request recorded as success).
//
// These tests drive a fake upstream through the whole relay for each inbound
// protocol and assert the client now gets the protocol's in-band failure frame, no
// duplicated content and no faked success terminal. They also pin the false-positive
// boundary: a stream that DOES carry a terminal marker must stay byte-for-byte on the
// old success path.
const truncationTestModel = "trunc-model"

// truncationChatChannel registers one OpenAI-chat channel pointed at the fake
// upstream. All three inbound protocols are routed onto this chat channel, so the
// inbound protocol alone decides which terminal frame the client receives.
func truncationChatChannel(t *testing.T, ctx context.Context, upstreamURL string) {
	t.Helper()
	balancer.ResetRuntimeTelemetry()
	channel := dbmodel.Channel{
		Name:     "truncation-upstream",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    truncationTestModel,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "trunc-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
}

// driveTruncationRequest runs a streaming request of the given inbound protocol
// through the full relay and returns the client-visible body.
func driveTruncationRequest(t *testing.T, inboundType inbound.InboundType) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var path, body string
	switch inboundType {
	case inbound.InboundTypeOpenAIChat:
		path = "/v1/chat/completions"
		body = fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, truncationTestModel)
	case inbound.InboundTypeOpenAIResponse:
		path = "/v1/responses"
		body = fmt.Sprintf(`{"model":%q,"stream":true,"input":"hi"}`, truncationTestModel)
	case inbound.InboundTypeAnthropic:
		path = "/v1/messages"
		body = fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, truncationTestModel)
	default:
		t.Fatalf("unsupported inbound type %v", inboundType)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inboundType, c)
	return rec.Body.String()
}

func writeSSEWire(w http.ResponseWriter, flusher http.Flusher, payloads ...string) {
	for _, p := range payloads {
		_, _ = w.Write([]byte("data: " + p + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func chatChunk(content string) string {
	return `{"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"` +
		truncationTestModel + `","choices":[{"index":0,"delta":{"content":"` + content + `"}}]}`
}

func chatRoleChunk() string {
	return `{"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"` +
		truncationTestModel + `","choices":[{"index":0,"delta":{"role":"assistant"}}]}`
}

func chatFinishChunk() string {
	return `{"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"` +
		truncationTestModel + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
}

// writeTruncatedChatBody streams a role + two content chunks over the OpenAI chat
// wire and then returns: no finish_reason, no [DONE]. The handler returning closes the
// socket at an event boundary, which go-sse reports as a clean EOF — the very case the
// relay used to treat as a successful end.
func writeTruncatedChatBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEWire(w, flusher, chatRoleChunk(), chatChunk("A-1"), chatChunk("A-2"))
}

// writeCompleteChatBody streams the same content but ends the turn properly: a
// finish_reason chunk, then [DONE]. This is the control that must stay on the old
// success path (no failure frame).
func writeCompleteChatBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEWire(w, flusher, chatRoleChunk(), chatChunk("A-1"), chatChunk("A-2"), chatFinishChunk())
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// writeFinishThenEOFChatBody ends the turn with a finish_reason chunk but closes the
// socket before a [DONE] sentinel — a legitimate completion on the chat wire that the
// existing relay tests already treat as success. It guards the false-positive
// boundary: the new truncation detector must NOT fire here.
func writeFinishThenEOFChatBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEWire(w, flusher, chatRoleChunk(), chatChunk("A-1"), chatChunk("A-2"), chatFinishChunk())
}

// --- truncated upstream (must emit the in-band failure frame) ---

func TestChatTruncatedUpstreamEmitsFailureFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(writeTruncatedChatBody))
	t.Cleanup(upstream.Close)
	truncationChatChannel(t, ctx, upstream.URL)

	body := driveTruncationRequest(t, inbound.InboundTypeOpenAIChat)
	t.Logf("chat truncation wire=%q", body)

	if !strings.Contains(body, "octopus_upstream_stream_truncated") {
		t.Fatalf("chat truncation must end with the failure frame code, got %q", body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("chat truncation frame must be an error object, got %q", body)
	}
	if strings.Contains(body, "response.failed") || strings.Contains(body, "message_stop") {
		t.Fatalf("chat inbound must not carry another protocol's terminal, got %q", body)
	}
	if got := strings.Count(body, "A-1"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-1 seen %d times: %q", got, body)
	}
	if got := strings.Count(body, "A-2"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-2 seen %d times: %q", got, body)
	}
}

func TestResponsesTruncatedUpstreamEmitsFailureFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(writeTruncatedChatBody))
	t.Cleanup(upstream.Close)
	truncationChatChannel(t, ctx, upstream.URL)

	body := driveTruncationRequest(t, inbound.InboundTypeOpenAIResponse)
	t.Logf("responses truncation wire=%q", body)

	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("responses truncation must end with event: response.failed, got %q", body)
	}
	if !strings.Contains(body, "octopus_upstream_stream_truncated") {
		t.Fatalf("responses truncation frame must carry the failure code, got %q", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("responses truncation must NOT fake response.completed, got %q", body)
	}
	if got := strings.Count(body, "A-1"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-1 seen %d times: %q", got, body)
	}
	if got := strings.Count(body, "A-2"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-2 seen %d times: %q", got, body)
	}
}

func TestAnthropicTruncatedUpstreamEmitsFailureFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(writeTruncatedChatBody))
	t.Cleanup(upstream.Close)
	truncationChatChannel(t, ctx, upstream.URL)

	body := driveTruncationRequest(t, inbound.InboundTypeAnthropic)
	t.Logf("anthropic truncation wire=%q", body)

	if !strings.Contains(body, "event: error") {
		t.Fatalf("anthropic truncation must end with event: error, got %q", body)
	}
	if !strings.Contains(body, "upstream stream ended before a terminal event") {
		t.Fatalf("anthropic truncation frame must carry the neutral message, got %q", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("anthropic truncation must NOT fake message_stop, got %q", body)
	}
	if got := strings.Count(body, "A-1"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-1 seen %d times: %q", got, body)
	}
	if got := strings.Count(body, "A-2"); got != 1 {
		t.Fatalf("business content must not be duplicated, A-2 seen %d times: %q", got, body)
	}
}

// --- control group: a real terminal marker must keep the old success path ---

func TestNormalCompletionIsNotReportedAsTruncated(t *testing.T) {
	cases := []struct {
		name        string
		inboundType inbound.InboundType
		upstream    http.HandlerFunc
		// successTerminal is a marker the synthesized success tail MUST contain;
		// absentWhenFailure must be absent; deltaMarker is the per-protocol content
		// delta that must appear exactly once (aggregated done events repeat the
		// accumulated text by design, so only the delta is a duplication signal).
		successTerminal   string
		absentWhenFailure string
		deltaMarker       string
	}{
		{"chat", inbound.InboundTypeOpenAIChat, writeCompleteChatBody, "data: [DONE]", "octopus_upstream_stream_truncated", `"content":"A-1"`},
		{"responses", inbound.InboundTypeOpenAIResponse, writeCompleteChatBody, "response.completed", "response.failed", `"delta":"A-1"`},
		{"anthropic", inbound.InboundTypeAnthropic, writeCompleteChatBody, "message_stop", "event: error", `"text":"A-1"`},
		// The finish_reason-then-EOF shape (no [DONE]) is a completed chat turn: the
		// new detector must not misfire on it.
		{"chat-finish-then-eof", inbound.InboundTypeOpenAIChat, writeFinishThenEOFChatBody, "data: [DONE]", "octopus_upstream_stream_truncated", `"content":"A-1"`},
		{"responses-finish-then-eof", inbound.InboundTypeOpenAIResponse, writeFinishThenEOFChatBody, "response.completed", "response.failed", `"delta":"A-1"`},
		{"anthropic-finish-then-eof", inbound.InboundTypeAnthropic, writeFinishThenEOFChatBody, "message_stop", "event: error", `"text":"A-1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)

			upstream := httptest.NewServer(tc.upstream)
			t.Cleanup(upstream.Close)
			truncationChatChannel(t, ctx, upstream.URL)

			body := driveTruncationRequest(t, tc.inboundType)

			if strings.Contains(body, tc.absentWhenFailure) {
				t.Fatalf("a completed stream must not be reported as a failure, got %q", body)
			}
			if !strings.Contains(body, tc.successTerminal) {
				t.Fatalf("a completed stream must keep its success terminal %q, got %q", tc.successTerminal, body)
			}
			if got := strings.Count(body, tc.deltaMarker); got != 1 {
				t.Fatalf("control content delta must appear once, %s seen %d times: %q", tc.deltaMarker, got, body)
			}
		})
	}
}
