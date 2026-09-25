package relay

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

var redactTokenRe = regexp.MustCompile(`\{\{Redact:[a-f0-9]{64}\}\}`)

func setupRedactDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	balancer.ResetRuntimeTelemetry()
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "true"); err != nil {
		t.Fatalf("enable redaction: %v", err)
	}
}

// newRedactChannel registers an OpenAI-chat channel with redaction enabled.
func newRedactChannel(t *testing.T, upstream string, redactEnabled bool) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:          "redact-channel",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:         "upstream-model",
		ModelMapping:  map[string]string{"request-model": "upstream-model"},
		Priority:      1,
		RedactEnabled: redactEnabled,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
}

func newRedactGinEngine() *gin.Engine {
	engine := gin.New()
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIChat, c)
	})
	return engine
}

// TestRelayRedactNonStreamRoundTrip: a secret in the request body must reach the
// upstream as a placeholder and the model's echo of that placeholder must reach
// the client restored to the original secret. Fingerprint safety is structural:
// redaction only rewrites the body bytes; the outbound request goes through the
// same fingerprint stack as before.
func TestRelayRedactNonStreamRoundTrip(t *testing.T) {
	setupRedactDB(t)

	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		upstreamBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		// Echo the first placeholder back as the model reply.
		token := ""
		if m := redactTokenRe.FindString(upstreamBody); m != "" {
			token = m
		}
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"the address is ` + token + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannel(t, upstream.URL, true)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com please remember"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(upstreamBody, "a@example.com") {
		t.Fatalf("secret leaked to upstream: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, "{{Redact:") {
		t.Fatalf("no placeholder in upstream body: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, "Sensitive values are redacted") && !strings.Contains(upstreamBody, "{{Redact:") {
		t.Fatalf("neither notice nor placeholder: %s", upstreamBody)
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client did not see restored secret: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to client: %s", rec.Body.String())
	}
}

// TestRelayRedactStreamRoundTrip: the model streams the placeholder split across
// two delta events; the client must receive the restored secret.
func TestRelayRedactStreamRoundTrip(t *testing.T) {
	setupRedactDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		token := ""
		if m := redactTokenRe.FindString(body); m != "" {
			token = m
		}
		cut := len(token) / 2
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"the address is ` + token[:cut] + `"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"` + token[cut:] + ` ok"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannel(t, upstream.URL, true)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","stream":true,"messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to stream client: %s", rec.Body.String())
	}
	// Concatenate delta contents and expect the restored secret.
	var content strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		// extract "content":"..." occurrences
		idx := strings.Index(line, `"content":"`)
		for idx >= 0 {
			rest := line[idx+len(`"content":"`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				break
			}
			content.WriteString(unescapeJSONString(rest[:end]))
			line = rest[end:]
			idx = strings.Index(line, `"content":"`)
		}
	}
	if !strings.Contains(content.String(), "a@example.com") {
		t.Fatalf("stream content missing restored secret, got %q", content.String())
	}
}

// TestRelayRedactDisabledPassthrough: with the channel opted out, the body must
// reach the upstream byte-identical (redaction layer is a strict no-op).
func TestRelayRedactDisabledPassthrough(t *testing.T) {
	setupRedactDB(t)

	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		upstreamBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannel(t, upstream.URL, false)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamBody, "a@example.com") {
		t.Fatalf("opted-out channel must pass the body through untouched: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "{{Redact:") || strings.Contains(upstreamBody, "Sensitive values are redacted") {
		t.Fatalf("opted-out channel must not be redacted: %s", upstreamBody)
	}
}

func unescapeJSONString(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// TestRelayRedactStreamAbruptEndNoHang: the upstream cuts the stream right after
// a delta whose content ends with a split placeholder's first half (no DONE, no
// terminal event). The restorer holds that event buffered; the end-of-stream
// force flush surfaces it. A truncated placeholder fragment cannot be restored
// (the mapping has no key for a half token — same as upstream Cosy), so the
// fragment passes through verbatim; what must hold is that the stream
// terminates cleanly with the buffered text delivered, no hang, no drop.
func TestRelayRedactStreamAbruptEndNoHang(t *testing.T) {
	setupRedactDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		token := ""
		if m := redactTokenRe.FindString(body); m != "" {
			token = m
		}
		cut := len(token) / 2
		w.Header().Set("Content-Type", "text/event-stream")
		// First half of the placeholder, then the stream just ends.
		w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"mail me at ` + token[:cut] + `"}}]}` + "\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannel(t, upstream.URL, true)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","stream":true,"messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// The buffered delta text must reach the client (not dropped by the buffer):
	// the fragment itself passes through verbatim — a half token has no mapping
	// key, matching upstream Cosy behavior.
	if !strings.Contains(rec.Body.String(), "mail me at") {
		t.Fatalf("buffered event text was dropped on abrupt end: %s", rec.Body.String())
	}
	// Stream must terminate cleanly with a DONE marker (oct synthesizes it).
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream did not terminate cleanly: %s", rec.Body.String())
	}
}

// TestRelayRedactRaceModeRoundTrip: race mode runs each racer on an isolated
// relayRequest clone, so each racer lazily creates its OWN redaction session.
// Regression test for the audit finding (2026-09-25): the winner's response is
// handled by a winnerRA attached to the PARENT relayRequest — before the fix,
// the racer's session never transferred, restore was skipped entirely, and the
// model's placeholder echo leaked raw {{Redact:...}} to the client. The fix
// carries redactSession/redactApplied through racerResult into parentReq.
func TestRelayRedactRaceModeRoundTrip(t *testing.T) {
	setupRedactDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		token := ""
		if m := redactTokenRe.FindString(body); m != "" {
			token = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"the address is ` + token + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:               "race-redact-channel",
		Type:               outbound.OutboundTypeOpenAIChat,
		Enabled:            true,
		BaseUrls:           []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:               []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "key-a"}, {Enabled: true, ChannelKey: "key-b"}},
		Model:              "upstream-model",
		ModelMapping:       map[string]string{"request-model": "upstream-model"},
		Priority:           1,
		RaceMode:           true,
		RaceKeyConcurrency: 2,
		RedactEnabled:      true,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com please remember"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("race mode: placeholder leaked to client (session transfer broken): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("race mode: client did not see restored secret: %s", rec.Body.String())
	}
}

// TestRelayRedactRaceInheritedSessionRoundTrip locks the audit-critical fix:
// attempt 1 on a redaction-enabled channel creates the parent's session and
// fails; failover lands on a race-mode channel whose racers must NOT inherit
// (and losers must NOT close) the parent's session — the winner's response
// must be fully restored. Pre-fix, losers closed the shared inherited session
// mid-stream and the whole winner response leaked placeholders.
func TestRelayRedactRaceInheritedSessionRoundTrip(t *testing.T) {
	setupRedactDB(t)

	// Channel A: redaction enabled, always 500 — creates the parent session,
	// then fails so failover moves on.
	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(failUpstream.Close)

	// Channel B: race mode, redaction enabled, echoes whatever token it got.
	raceUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		token := ""
		if m := redactTokenRe.FindString(body); m != "" {
			token = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"echo back ` + token + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(raceUpstream.Close)

	channelA := dbmodel.Channel{
		Name:          "redact-fail-first",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: failUpstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "key-fail"}},
		Model:         "upstream-model",
		ModelMapping:  map[string]string{"request-model": "upstream-model"},
		Priority:      1,
		RedactEnabled: true,
	}
	if err := op.ChannelCreate(&channelA, t.Context()); err != nil {
		t.Fatalf("create channel A: %v", err)
	}
	balancer.ResetChannel(channelA.ID)

	channelB := dbmodel.Channel{
		Name:               "redact-race-second",
		Type:               outbound.OutboundTypeOpenAIChat,
		Enabled:            true,
		BaseUrls:           []dbmodel.BaseUrl{{URL: raceUpstream.URL}},
		Keys:               []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "key-r1"}, {Enabled: true, ChannelKey: "key-r2"}},
		Model:              "upstream-model",
		ModelMapping:       map[string]string{"request-model": "upstream-model"},
		Priority:           2,
		RaceMode:           true,
		RaceKeyConcurrency: 2,
		RedactEnabled:      true,
	}
	if err := op.ChannelCreate(&channelB, t.Context()); err != nil {
		t.Fatalf("create channel B: %v", err)
	}
	balancer.ResetChannel(channelB.ID)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com please remember"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("inherited-session race: placeholder leaked to client (losers closed the shared session): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("inherited-session race: client did not see restored secret: %s", rec.Body.String())
	}
}
