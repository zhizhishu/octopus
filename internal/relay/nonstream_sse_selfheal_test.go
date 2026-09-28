package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/gin-gonic/gin"
)

// selfHealSSEChatBody is what a stream-only upstream (web-chat bridge) returns even
// when the client asked for a non-stream response: content chunks, a finish chunk,
// then [DONE].
const selfHealSSEChatBody = `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"

func newSelfHealChatAttempt(t *testing.T) (*httptest.ResponseRecorder, *gin.Context, *relayAttempt) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIChat,
		inAdapter:    &openaiInbound.ChatInbound{},
		requestModel: "test-model",
	}}
	return rec, c, ra
}

// TestHandleResponseSelfHealsSSEUpstream: a non-stream chat request whose upstream
// ignores "stream": false and replies with an SSE content-type must be aggregated
// into one complete JSON response instead of failing with an unmarshal error on the
// leading 'd' of "data:".
func TestHandleResponseSelfHealsSSEUpstream(t *testing.T) {
	rec, c, ra := newSelfHealChatAttempt(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(selfHealSSEChatBody)),
	}
	if err := ra.handleResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}); err != nil {
		t.Fatalf("handleResponse should self-heal an SSE reply to a non-stream request, got: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 aggregated response, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion"`) {
		t.Fatalf("expected aggregated chat.completion JSON, got %q", body)
	}
	if !strings.Contains(body, "Hello world") {
		t.Fatalf("expected aggregated content, got %q", body)
	}
	if strings.Contains(body, "data:") {
		t.Fatalf("aggregated response must not leak SSE framing, got %q", body)
	}
}

// TestHandleResponseSelfHealsSSEUpstreamWithoutContentType: the same self-heal when
// the upstream omits the Content-Type header — the line-level body framing fallback
// must detect the SSE body and align the header for the aggregation handoff.
func TestHandleResponseSelfHealsSSEUpstreamWithoutContentType(t *testing.T) {
	rec, c, ra := newSelfHealChatAttempt(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(selfHealSSEChatBody)),
	}
	if err := ra.handleResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}); err != nil {
		t.Fatalf("handleResponse should self-heal via body framing fallback, got: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 aggregated response, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Hello world") {
		t.Fatalf("expected aggregated content, got %q", body)
	}
}

// TestHandleResponseJSONWithSSELikeTextNotMisrouted: a plain JSON completion whose
// text content merely contains "data:"/"event:" substrings must stay on the normal
// JSON path — line-level framing cannot misfire, and the peeked body must be fully
// replayed so the outbound still parses the complete JSON.
func TestHandleResponseJSONWithSSELikeTextNotMisrouted(t *testing.T) {
	rec, c, ra := newSelfHealChatAttempt(t)
	jsonBody := `{"id":"chatcmpl-x","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"echo data: foo event: bar"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
	}
	if err := ra.handleResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}); err != nil {
		t.Fatalf("plain JSON response must not be misrouted, got: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "echo data: foo event: bar") {
		t.Fatalf("expected the full JSON body to round-trip after the peek replay, got %q", body)
	}
}
