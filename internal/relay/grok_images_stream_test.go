package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// TestImagesHandlerGrokStreamRequestUsesNonStream covers the bug where a client
// sends {"model":"grok-2-image","stream":true} to /v1/images/generations. Grok
// has no image streaming variant, so normalizeGrokImagesPayload drops "stream"
// from the upstream payload and the upstream replies with application/json.
// Previously the local stream flag stayed true, routing into proxySSE, which
// failed with a non-SSE content-type error. The handler must now route through
// proxyNonStream and succeed.
func TestImagesHandlerGrokStreamRequestUsesNonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var gotBody map[string]any
	var gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		// Grok image upstream replies with plain JSON, not SSE, even though the
		// client asked for stream=true.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"url":"https://example.test/grok.png"}]}`))
	}))
	t.Cleanup(upstream.Close)

	createRawImagesGroupWithType(t, ctx, upstream.URL+"/v1/chat/completions", "grok-stream-image", "grok-2-image", "grok-key", outbound.OutboundTypeCustomOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"grok-stream-image",
		"prompt":"draw a grok image",
		"stream":true,
		"n":1,
		"response_format":"url"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected grok stream image request to succeed via non-stream path, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"url":"https://example.test/grok.png"`) {
		t.Fatalf("expected non-stream JSON image response, got %s", rec.Body.String())
	}
	// Response must be plain JSON (non-stream), never an SSE stream.
	if ct := rec.Header().Get("Content-Type"); strings.Contains(strings.ToLower(ct), "text/event-stream") {
		t.Fatalf("expected non-SSE response content-type, got %q", ct)
	}
	// The upstream payload must have dropped "stream", and the request should
	// not have negotiated an SSE accept header.
	if _, ok := gotBody["stream"]; ok {
		t.Fatalf("expected grok image payload to drop stream, got %#v", gotBody)
	}
	if strings.Contains(strings.ToLower(gotAccept), "text/event-stream") {
		t.Fatalf("expected non-SSE Accept header upstream, got %q", gotAccept)
	}
}
