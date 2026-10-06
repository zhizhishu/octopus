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

// upstream_header_timeout_seconds bounds the one gap the first-content guard cannot
// reach: the wait for response headers. first_token_time_out_default starts a timer
// only AFTER headers arrive, so an upstream that accepted the request and then stayed
// completely silent was bounded by nothing — measured on an isolated instance with the
// guard armed at 10s: the attempt still occupied the whole 60s client window, because
// the guard had never started.
//
// Both halves are pinned here: the silent upstream is cut, and a slow-but-talking
// upstream is NOT cut once its headers are in (bounding only the header wait, never the
// SSE body, is the whole point — a naive cancel-after-Do would break every long answer).

func headerTimeoutChannel(t *testing.T, ctx context.Context, upstreamURL, model string) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:     "header-timeout-upstream",
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

func headerTimeoutClient(t *testing.T, model string) (*gin.Context, *httptest.ResponseRecorder) {
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

// An upstream that takes the request and never sends response headers must be cut at
// the budget instead of holding the attempt for the client's whole lifetime.
func TestUpstreamHeaderTimeoutCutsSilentUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "1"); err != nil {
		t.Fatalf("enable upstream header timeout: %v", err)
	}

	// Release the handler explicitly before closing the server: blocking it on
	// r.Context().Done() alone makes httptest.Server.Close() wait forever, because the
	// relay abandons the header wait without the server-side context ever being
	// cancelled.
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept and answer nothing: no status line, no headers, no body.
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		upstream.Close()
	})

	headerTimeoutChannel(t, ctx, upstream.URL, "header-timeout-silent")
	c, rec := headerTimeoutClient(t, "header-timeout-silent")

	startedAt := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(startedAt)

	if elapsed > 8*time.Second {
		t.Fatalf("a silent upstream must be cut at the 1s budget, took %s", elapsed)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 for a silent upstream, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "octopus_upstream_header_timeout") {
		t.Fatalf("expected the header-timeout error code, got %s", rec.Body.String())
	}
}

// The budget covers the header wait only. An upstream that sends headers immediately
// and then streams for longer than the budget must deliver the whole answer.
func TestUpstreamHeaderTimeoutDoesNotCutStreamAfterHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "1"); err != nil {
		t.Fatalf("enable upstream header timeout: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Headers are out; now stream slowly, well past the 1s header budget.
		for i := 0; i < 6; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_slow\",\"object\":\"chat.completion.chunk\",\"model\":\"slow-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CHUNK%d\"}}]}\n\n", i)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(400 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	headerTimeoutChannel(t, ctx, upstream.URL, "header-timeout-slow-body")
	c, rec := headerTimeoutClient(t, "header-timeout-slow-body")

	startedAt := time.Now()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(startedAt)

	if rec.Code != http.StatusOK {
		t.Fatalf("a slow body after fast headers must succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CHUNK5") {
		t.Fatalf("the header budget must not cut the body stream; body %s", rec.Body.String())
	}
	if elapsed < 2*time.Second {
		t.Fatalf("expected the body to stream past the 1s header budget, took only %s", elapsed)
	}
}

// Default-off is the contract that keeps every existing deployment unchanged.
func TestUpstreamHeaderTimeoutConfig(t *testing.T) {
	setupRelayErrorDB(t)

	if got := currentUpstreamHeaderTimeout(); got != 0 {
		t.Fatalf("expected upstream header timeout to default to disabled, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "120"); err != nil {
		t.Fatalf("set upstream header timeout: %v", err)
	}
	if got := currentUpstreamHeaderTimeout(); got != 120*time.Second {
		t.Fatalf("expected 120s upstream header timeout, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "0"); err != nil {
		t.Fatalf("disable upstream header timeout: %v", err)
	}
	if got := currentUpstreamHeaderTimeout(); got != 0 {
		t.Fatalf("expected disabled upstream header timeout, got %s", got)
	}
}
