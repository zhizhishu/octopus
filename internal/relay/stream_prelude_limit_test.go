package relay

import (
	"errors"
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

// newChatPreludeAttempt builds the minimal harness for driving handleStreamResponse
// on the OpenAI Chat path, where a bare role delta is the classic buffered opener.
func newChatPreludeAttempt(rec *httptest.ResponseRecorder) (*relayAttempt, *gin.Context) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIChat,
		inAdapter:    &openaiInbound.ChatInbound{},
		requestModel: "gpt-5.5",
	}}, c
}

// chatRoleDeltaEvent is a valid, non-meaningful opener: it carries only a role, so
// it is buffered until real content arrives.
func chatRoleDeltaEvent(filler string) string {
	return `data: {"id":"chatcmpl-` + filler + `","object":"chat.completion.chunk","created":123,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
}

// An upstream that streams nothing but opener events must not be able to grow the
// deferred-commit buffer without bound. Neither existing clock bounds it: the
// upstream data-interval timer is reset by every event read, and the first-token
// timer is opt-in (off by default).
func TestStreamPreludeBufferIsBounded(t *testing.T) {
	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)

	// ~1KiB per event: enough events to sail past the cap if the buffer is unbounded.
	event := chatRoleDeltaEvent(strings.Repeat("x", 900))

	pr, pw := io.Pipe()
	go func() {
		for range 8192 {
			if _, err := io.WriteString(pw, event); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	if err == nil {
		t.Fatal("expected prelude-over-limit failure for a content-free opener stream, got nil")
	}
	var relayErr *localRelayError
	if !errors.As(err, &relayErr) || !strings.Contains(relayErr.strategy, "stream_prelude_over_limit") {
		t.Fatalf("expected stream_prelude_over_limit, got %v", err)
	}
	if relayErr.status != http.StatusBadGateway {
		t.Fatalf("expected 502 for over-limit prelude, got %d", relayErr.status)
	}
	if !strings.Contains(relayErr.strategy, "upstream_forwarded=false") {
		t.Fatalf("nothing meaningful reached the client, so failover must stay allowed: %q", relayErr.strategy)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("over-limit prelude must not be flushed downstream, got %q", body)
	}
}

// The cap must not disturb the normal path: a small opener still buffers, then
// flushes ahead of the first content chunk.
func TestStreamPreludeWithinLimitStillDeliversOpenerThenContent(t *testing.T) {
	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			chatRoleDeltaEvent("1") +
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":123,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"OK"}}]}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}); err != nil {
		t.Fatalf("handle stream response: %v", err)
	}
	body := rec.Body.String()
	openerAt := strings.Index(body, `"role":"assistant"`)
	contentAt := strings.Index(body, `"content":"OK"`)
	if openerAt < 0 || contentAt < 0 {
		t.Fatalf("expected opener and content downstream, got %q", body)
	}
	if openerAt > contentAt {
		t.Fatalf("buffered opener must flush ahead of the first content chunk, got %q", body)
	}
}
