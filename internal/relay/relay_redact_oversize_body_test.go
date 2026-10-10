package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// D02 finding G3 regression: the scan cost is linear in body size (measured on the
// candidate: 1MB ≈ 207s, and a 1.1MB request stalled the instance for ~18.4 minutes
// until the client's connection died — the client saw a dead socket, not an error).
// An over-limit body must therefore fail FAST and LOUD, and must never be forwarded:
// a raw send is exactly what the module exists to prevent.
func TestRelayRedactOversizeBodyFailsClosed(t *testing.T) {
	setupRedactDB(t)

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"upstream-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-oversize",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:         "upstream-model",
		ModelMapping:  map[string]string{"request-model": "upstream-model"},
		Priority:      1,
		RedactEnabled: true,
		RedactFlags:   "HPSIBEG",
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)

	// One byte over the documented limit, built as a plain filler body (no secret is
	// needed: the guard runs before any scanning).
	padding := strings.Repeat("a", redactMaxScanBytes)
	body := `{"model":"request-model","stream":false,"messages":[{"role":"user","content":"` + padding + `"}]}`

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want %d for an over-limit body, got %d: %s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), redactBodyTooLargeCode) {
		t.Fatalf("the client must get the explicit code %q: %s", redactBodyTooLargeCode, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "redaction scan limit") {
		t.Fatalf("the client must get an actionable message, not a generic one: %s", rec.Body.String())
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("an over-limit body must never reach the upstream; upstream was called %d time(s)", hits)
	}
}
