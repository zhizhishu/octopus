package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// Guard for the body-scan/graft boundary (D02 finding §2.4).
//
// applyInboundRedaction runs `s.RedactJSONBody(RawRequest)` to mint tokens and count
// hits, and DISCARDS the returned bytes because the core's whole-body strategy is
// wider than the contract: it rewrites every string leaf it judges sensitive,
// including model signatures, which the boundary rule says must not be rewritten.
// This test pins both halves of that statement:
//
//  1. the hazard is real — the core's own scan of this exact body DOES rewrite the
//     signature (so grafting that result would corrupt model state);
//  2. oct does not graft it — the body oct actually sends upstream still carries the
//     signature byte-for-byte, while the sensitive value sitting next to it IS
//     replaced by a placeholder (which proves redaction ran on this request at all).
//
// A redaction no-op would make assertion 2 vacuous, which is why the placeholder
// assertion is part of it.
func TestRelayRedactModelSignatureStaysByteIdentical(t *testing.T) {
	setupRedactDB(t)

	const (
		// High-entropy hex: the H detector fires on it, so a graft would rewrite it.
		signature = "3f9a70c1e5b28d64af03c7e91d5b6802ac47e1f39b8d2506cf71a4e0d39b6c82"
		secret    = "c81f4a6d39b0e2754a9c6f180d3b7e5a2948dc0f61b3a75e8c4d2091fb63a5e7"
	)

	var mu sync.Mutex
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		upstreamBody = string(buf[:n])
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// oct drives the anthropic outbound as a real upstream stream (stream:true on
		// the wire), so the fake has to answer SSE even for a non-streaming caller.
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-signature-identity",
		Type:          outbound.OutboundTypeAnthropic,
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

	// An assistant turn that carries a thinking block with a signature (the shape a
	// Claude client replays), plus a user turn holding the sensitive value.
	clientBody := `{"model":"request-model","max_tokens":64,"stream":false,` +
		`"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"let me think","signature":"` + signature + `"},{"type":"text","text":"the answer"}]},` +
		`{"role":"user","content":[{"type":"text","text":"my key is ` + secret + `"}]}` +
		`]}`

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeAnthropic, c)

	mu.Lock()
	sent := upstreamBody
	mu.Unlock()
	if rec.Code != http.StatusOK {
		if sent == "" {
			t.Fatalf("status %d (upstream never received a body): %s", rec.Code, rec.Body.String())
		}
		t.Fatalf("status %d: %s\noutbound body: %s", rec.Code, rec.Body.String(), sent)
	}
	if sent == "" {
		t.Fatalf("upstream never saw a body")
	}

	// 1. The hazard is real: on the chat-shaped body the core's whole-body scan
	//    rewrites `messages[].reasoning_signature` (measured offline against the
	//    vendored core: core-policy-map.txt:15 core_changed=true, while the anthropic
	//    thinking `signature` leaf is exempted by the core's protocol table — line 49).
	//    Neither leaf is oct's to rewrite, so the scan output has to stay discarded.
	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	chatShaped := `{"model":"request-model","messages":[` +
		`{"role":"assistant","content":"answer","reasoning_signature":"` + signature + `"},` +
		`{"role":"user","content":"my key is ` + secret + `"}]}`
	sess, err := engine.NewSession("HPSIBEG", inboundRedactProtocol(inbound.InboundTypeOpenAIChat), false)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	scanned, err := sess.RedactJSONBody([]byte(chatShaped))
	if err != nil {
		t.Fatalf("core body scan: %v", err)
	}
	if strings.Contains(string(scanned), signature) {
		t.Fatalf("expected the core's whole-body scan to rewrite messages[].reasoning_signature (that is why its output must stay discarded), but it left it intact — the contract note in applyInboundRedaction needs revisiting")
	}
	if !strings.Contains(string(scanned), "{{Redact:") {
		t.Fatalf("core body scan produced no placeholder at all; test input is not exercising the detector")
	}
	t.Logf("core scan rewrote reasoning_signature (hazard confirmed); scan minted %d token(s)", sess.Count())

	// 2. oct does not graft that result: the signature is byte-identical upstream...
	if !strings.Contains(sent, signature) {
		t.Fatalf("model signature was rewritten on the wire; outbound body=%s", sent)
	}
	// ...while the sensitive value next to it was replaced (redaction really ran).
	if strings.Contains(sent, secret) {
		t.Fatalf("sensitive value reached the upstream raw; outbound body=%s", sent)
	}
	if !strings.Contains(sent, "{{Redact:") {
		t.Fatalf("no placeholder in the outbound body; redaction did not run: %s", sent)
	}
}
