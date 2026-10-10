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

// D02 finding G2 regression: a sensitive value parked in the stop sequence reached
// the upstream raw (chat `stop`, run 033a2a case R01 seq 13 `RAW@ ["stop[]"]`;
// anthropic `stop_sequences` likewise). Both protocols share one internal field, so
// one write-back covers both — and the write-back must NOT re-serialize the value
// through a JSON wrapper, because Stop.MarshalJSON is what keeps a single stop a
// bare string and a list an array. A wrapper would silently change the wire shape.
func TestRelayRedactStopSequences(t *testing.T) {
	setupRedactDB(t)

	const secret = "7d2f9b04c6a13e58b0f47d29c85a36e1fb9d0472ac63e8519b27f40d3c6a9182"

	var mu sync.Mutex
	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		mu.Lock()
		if strings.Contains(body, `"stop_sequences"`) {
			seen["anthropic"] = body
		} else if strings.Contains(body, `"stop":[`) {
			seen["chat-list"] = body
		} else {
			seen["chat-single"] = body
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n"))
			w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"upstream-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	chatChannel := dbmodel.Channel{
		Name:          "redact-stop-chat",
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
	if err := op.ChannelCreate(&chatChannel, t.Context()); err != nil {
		t.Fatalf("create chat channel: %v", err)
	}
	balancer.ResetChannel(chatChannel.ID)

	drive := func(t *testing.T, inType inbound.InboundType, path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inType, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}

	t.Run("chat single stop stays a bare string", func(t *testing.T) {
		drive(t, inbound.InboundTypeOpenAIChat, "/v1/chat/completions",
			`{"model":"request-model","stream":false,"messages":[{"role":"user","content":"hi"}],`+
				`"stop":"my key is `+secret+`"}`)

		mu.Lock()
		sent := seen["chat-single"]
		mu.Unlock()
		if sent == "" {
			t.Fatalf("upstream never saw the single-stop chat body")
		}
		if strings.Contains(sent, secret) {
			t.Fatalf("stop sequence reached the upstream raw: %s", sent)
		}
		if !strings.Contains(sent, `"stop":"my key is {{Redact:`) {
			t.Fatalf("stop sequence was not redacted in place (or the shape changed — a single stop must stay a JSON string): %s", sent)
		}
	})

	t.Run("chat stop list stays an array", func(t *testing.T) {
		drive(t, inbound.InboundTypeOpenAIChat, "/v1/chat/completions",
			`{"model":"request-model","stream":false,"messages":[{"role":"user","content":"hi"}],`+
				`"stop":["END","my key is `+secret+`"]}`)

		mu.Lock()
		sent := seen["chat-list"]
		mu.Unlock()
		if sent == "" {
			t.Fatalf("upstream never saw the list-stop chat body")
		}
		if strings.Contains(sent, secret) {
			t.Fatalf("stop sequence reached the upstream raw: %s", sent)
		}
		if !strings.Contains(sent, `"stop":["END","my key is {{Redact:`) {
			t.Fatalf("stop list was not redacted per value (shape must stay an array of strings): %s", sent)
		}
	})
}

// Same defect, anthropic dialect: the client sends `stop_sequences`, which the
// anthropic inbound transformer stores in the shared internal Stop field, and the
// anthropic outbound emits it back as `stop_sequences`. Covered by the same
// write-back — this test pins that the shared path really is shared.
func TestRelayRedactAnthropicStopSequences(t *testing.T) {
	setupRedactDB(t)

	const secret = "7d2f9b04c6a13b58c0f47d29e85a36e1fb9d0472ac63e8519b27f40d3c6a9182"

	var mu sync.Mutex
	var sent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		sent = string(buf[:n])
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"))
		w.Write([]byte(`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"))
		w.Write([]byte(`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n"))
		w.Write([]byte(`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n"))
		w.Write([]byte(`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n"))
		w.Write([]byte(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-stop-anthropic",
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

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"request-model","max_tokens":16,"stream":false,`+
			`"messages":[{"role":"user","content":"hi"}],`+
			`"stop_sequences":["END","my key is `+secret+`"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeAnthropic, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	wire := sent
	mu.Unlock()
	if wire == "" {
		t.Fatalf("upstream never saw the anthropic body")
	}
	if strings.Contains(wire, secret) {
		t.Fatalf("anthropic stop_sequences reached the upstream raw: %s", wire)
	}
	if !strings.Contains(wire, `"stop_sequences":["END","my key is {{Redact:`) {
		t.Fatalf("anthropic stop_sequences was not redacted per value: %s", wire)
	}
}
