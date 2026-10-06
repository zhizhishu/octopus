package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// The caller must never be left staring at a stream that has already been committed
// and then simply stops producing anything. Two committed-failure shapes exist and
// both have to end in the caller's own protocol with a generic message:
//
//   1. meaningful content already reached the caller → writeCommittedStreamFailure
//      writes the protocol's in-band terminal frame right away, because the channel
//      can no longer be swapped;
//   2. only comment heartbeats went out (the response head is committed, no envelope
//      is) → the relay may still fail over, and only when it finally gives up does the
//      final path write the same kind of frame.
//
// The second shape is what an operator sees as "the caller only ever got heartbeats".
// The assertions below also pin the privacy half of the contract: the frame must say
// nothing about the inside of the relay, even when the upstream's own error text
// carries identifiers.

func committedChatChannel(t *testing.T, ctx context.Context, upstreamURL, model string) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:     "Committed-Failure-Upstream",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    model,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-upstream"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

func committedChatClient(t *testing.T, model string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(
		`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return c, rec
}

// Shape 1: content was delivered, then the upstream fails. The caller keeps the
// content it already saw, gets a terminal error frame, and learns nothing internal.
func TestChatCommittedContentFailureEndsStreamCleanly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_committed","object":"chat.completion.chunk","model":"committed-chat","choices":[{"index":0,"delta":{"content":"HELLO"}}]}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// The upstream's own failure text deliberately carries a host-looking token: it
		// must not survive into the caller's stream.
		_, _ = w.Write([]byte(`data: {"error":{"type":"server_error","message":"SECRET-UPSTREAM-DETAIL at 10.9.9.9"}}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	committedChatChannel(t, ctx, upstream.URL, "committed-chat")
	c, rec := committedChatClient(t, "committed-chat")

	Handler(inbound.InboundTypeOpenAIChat, c)
	body := rec.Body.String()

	if !strings.Contains(body, "HELLO") {
		t.Fatalf("the content already delivered must stay on the wire, got %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("a committed stream failure must end with an in-band error frame, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("the terminal frame must close the stream with [DONE] so the caller does not wait for more, got %q", body)
	}
	if !strings.Contains(body, "upstream stream failed") {
		t.Fatalf("expected the generic public message, got %q", body)
	}
	if strings.Contains(body, "SECRET-UPSTREAM-DETAIL") {
		t.Fatalf("the upstream's own error text must not reach the caller, got %q", body)
	}
	if strings.Contains(body, "Committed-Failure-Upstream") || strings.Contains(body, upstream.URL) {
		t.Fatalf("channel/routing detail must not reach the caller, got %q", body)
	}
}

// Shape 2: nothing but comment heartbeats went out, then the attempt budget expires.
// This is the "the caller only ever saw heartbeats" complaint, and it must end with a
// real terminal frame rather than silence.
func TestChatHeartbeatCommittedFailureEndsStreamCleanly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	// A heartbeat a second after the wait starts commits the response head; the header
	// budget then expires on an upstream that never answers at all.
	if err := op.SettingSetString(dbmodel.SettingKeyFirstByteKeepaliveDelaySeconds, "1"); err != nil {
		t.Fatalf("set first-byte keepalive delay: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set keepalive interval: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "3"); err != nil {
		t.Fatalf("set upstream header timeout: %v", err)
	}

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		upstream.Close()
	})

	committedChatChannel(t, ctx, upstream.URL, "heartbeat-committed-chat")
	c, rec := committedChatClient(t, "heartbeat-committed-chat")

	startedAt := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(startedAt)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("the heartbeat must have committed HTTP 200 before the failure, got %d body %q", rec.Code, body)
	}
	if !strings.HasPrefix(body, ":\n\n") {
		t.Fatalf("expected the pre-content comment heartbeat to be the first bytes, got %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("a heartbeat-committed failure must still end with an error frame, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("the terminal frame must close the stream with [DONE], got %q", body)
	}
	if strings.Contains(body, "Committed-Failure-Upstream") || strings.Contains(body, upstream.URL) {
		t.Fatalf("channel/routing detail must not reach the caller, got %q", body)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("a silent upstream must be cut by the header budget, took %s", elapsed)
	}
}
