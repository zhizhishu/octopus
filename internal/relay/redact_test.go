package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
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

// TestRelayRedactRaceModeRoundTrip: race mode runs each racer with its OWN
// attempt-level redaction session (applyInboundRedaction inside prepareRacerAttempt),
// so racers never share a placeholder mapping. The winner's session rides through
// racerResult into winnerRA so the winner response restores against the same map.
// Regression test for the audit finding (2026-09-25): before the attempt-level
// design, the racer session never reached the parent request handle, restore was
// skipped entirely, and the model's placeholder echo leaked raw {{Redact:...}}
// to the client.
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
// attempt 1 on a redaction-enabled channel creates its own attempt session and
// fails; failover lands on a race-mode channel whose racers each build their own
// attempt-level session (never inherited from the parent request). No loser can
// close a session another racer/attempt is using, so the winner response restores
// against its own map (pre-fix, losers closed a shared inherited session mid-stream
// and the whole winner response leaked placeholders).
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

// newRedactChannelFull registers a channel with explicit type/flags/priority so
// a test can drive failover across channels that redact under different rules.
func newRedactChannelFull(t *testing.T, name, upstream string, channelType outbound.OutboundType, redactEnabled bool, flags string, priority int) dbmodel.Channel {
	t.Helper()
	channel := dbmodel.Channel{
		Name:          name,
		Type:          channelType,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:         "upstream-model",
		ModelMapping:  map[string]string{"request-model": "upstream-model"},
		Priority:      priority,
		RedactEnabled: redactEnabled,
		RedactFlags:   flags,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	balancer.ResetChannel(channel.ID)
	return channel
}

// TestRelayRedactPerAttemptSessionIsolation proves each attempt builds its own
// session with that channel's OWN flags: channel A redacts only email (flags E),
// channel B only secrets (flags S). A shared/reused session would leak one
// channel's detector set into the other.
func TestRelayRedactPerAttemptSessionIsolation(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	bodyA, bodyB := "", ""
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		bodyA = string(buf[:n])
		mu.Unlock()
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(upstreamA.Close)
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		bodyB = string(buf[:n])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-b","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstreamB.Close)

	newRedactChannelFull(t, "redact-flags-e", upstreamA.URL, outbound.OutboundTypeOpenAIChat, true, "E", 1)
	newRedactChannelFull(t, "redact-flags-s", upstreamB.URL, outbound.OutboundTypeOpenAIChat, true, "S", 2)

	// Use a value that matches the secret detector (\bsk-[A-Za-z0-9]{60,}\b) so the
	// two channels provably disagree on what to redact.
	skKey := "sk-" + strings.ToUpper(strings.Repeat("abcdef0123456789", 4)) // 64 chars after sk-
	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqBody := `{"model":"request-model","messages":[{"role":"user","content":"mail a@example.com or use ` + skKey + `"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	engine.ServeHTTP(rec, c.Request)

	mu.Lock()
	a, b := bodyA, bodyB
	mu.Unlock()
	if a == "" || b == "" {
		t.Fatalf("expected both channels to be attempted (failover), got A=%q B=%q", a, b)
	}
	// Channel A (flags E): email redacted, key left alone by ITS OWN flags.
	if strings.Contains(a, "a@example.com") {
		t.Fatalf("channel A (flags E) must redact the email: %s", a)
	}
	if !strings.Contains(a, skKey) {
		t.Fatalf("channel A (flags E only) must not touch the key: %s", a)
	}
	// Channel B (flags S): key redacted, email left alone (flags NOT shared with A).
	if strings.Contains(b, skKey) {
		t.Fatalf("channel B (flags S) must redact the key: %s", b)
	}
	if !strings.Contains(b, "a@example.com") {
		t.Fatalf("channel B (flags S only) must not touch the email (flags leaked from attempt A?): %s", b)
	}
}

// TestRelayRedactStickyAcrossFailover: once an attempt redacts, a later attempt on
// a channel that did NOT opt in must still redact (never silently swap a protected
// request onto an unprotected channel).
func TestRelayRedactStickyAcrossFailover(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var bodyB string
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(upstreamA.Close)
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		token := ""
		if m := redactTokenRe.FindString(string(buf[:n])); m != "" {
			token = m
		}
		mu.Lock()
		bodyB = string(buf[:n])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-b","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"echo back ` + token + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstreamB.Close)

	// A opted in (redacts, then fails); B did NOT opt in but must still redact.
	newRedactChannelFull(t, "redact-on", upstreamA.URL, outbound.OutboundTypeOpenAIChat, true, "", 1)
	newRedactChannelFull(t, "redact-off", upstreamB.URL, outbound.OutboundTypeOpenAIChat, false, "", 2)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	mu.Lock()
	b := bodyB
	mu.Unlock()
	if b == "" {
		t.Fatalf("expected failover to the opted-out channel")
	}
	if strings.Contains(b, "a@example.com") {
		t.Fatalf("opted-out failover channel must still redact (protection sticky): %s", b)
	}
	if !strings.Contains(b, "{{Redact:") && !strings.Contains(b, "Sensitive values are redacted") {
		t.Fatalf("expected the sticky redaction to have produced a placeholder/notice: %s", b)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client must get the restored secret after sticky failover: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRelayRedactFailClosedEngineDown: with the engine unavailable, a
// protection-demanding request fails explicitly and the original bytes never
// reach the upstream (fail-closed, no bare send).
func TestRelayRedactFailClosedEngineDown(t *testing.T) {
	setupRedactDB(t)

	redactEngineTestErr = errors.New("injected redaction engine failure (test)")
	defer func() { redactEngineTestErr = nil }()

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-engine-down", upstream.URL, outbound.OutboundTypeOpenAIChat, true, "", 1)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code == http.StatusOK {
		t.Fatalf("engine-down must not succeed: %d %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("engine-down must not forward the original request upstream, hits=%d", got)
	}
}

// TestRelayRedactClientFormatRestoreNonStream covers restore on the CLIENT-format
// body after a cross-protocol conversion (chat client -> Anthropic channel): the
// upstream echoes the placeholder in Anthropic shape, oct converts to a chat
// completion, and the client must see the restored secret, not the placeholder.
func TestRelayRedactClientFormatRestoreNonStream(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		token := ""
		if m := redactTokenRe.FindString(string(buf[:n])); m != "" {
			token = m
		}
		mu.Lock()
		upstreamBody = string(buf[:n])
		mu.Unlock()
		// Anthropic channels force a real streaming upstream even for non-stream
		// clients (protected Claude behavior against proxies cutting long non-stream
		// waits); oct then aggregates the SSE back into a chat completion. Respond in
		// SSE shape so this test exercises the B3 aggregated client-format restore.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[]}}\n\n"))
		w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"the address is " + token + "\"}}\n\n"))
		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-anthropic", upstream.URL, outbound.OutboundTypeAnthropic, true, "", 1)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	up := upstreamBody
	mu.Unlock()
	if strings.Contains(up, "a@example.com") {
		t.Fatalf("secret leaked to the upstream: %s", up)
	}
	if !strings.Contains(up, "{{Redact:") {
		t.Fatalf("expected a placeholder in the upstream body: %s", up)
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client did not see the restored secret after cross-protocol conversion: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}
}

// TestRelayRedactClientFormatRestoreSSE covers restore on the client SSE stream
// after a cross-protocol conversion, with the placeholder split across two
// upstream delta events (the restorer must stitch the halves on the client side).
func TestRelayRedactClientFormatRestoreSSE(t *testing.T) {
	setupRedactDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		token := ""
		if m := redactTokenRe.FindString(string(buf[:n])); m != "" {
			token = m
		}
		cut := len(token) / 2
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[]}}\n\n"))
		w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"mail me at " + token[:cut] + "\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + token[cut:] + " ok\"}}\n\n"))
		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-anthropic-stream", upstream.URL, outbound.OutboundTypeAnthropic, true, "", 1)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","stream":true,"messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the stream client: %s", rec.Body.String())
	}
	var content strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		rest := line[len("data: "):]
		for {
			idx := strings.Index(rest, `"content":"`)
			if idx < 0 {
				break
			}
			rest = rest[idx+len(`"content":"`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				break
			}
			content.WriteString(rest[:end])
			rest = rest[end:]
		}
	}
	if !strings.Contains(content.String(), "a@example.com") {
		t.Fatalf("stream client did not see the restored secret, got %q", content.String())
	}
}

// TestRelayRedactMediaZeroRedaction: the image and video generation paths are
// out of scope — a redaction-enabled channel must still forward the media prompt
// verbatim (no placeholder, no notice).
func TestRelayRedactMediaZeroRedaction(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	imageBody, videoBody := "", ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/images"):
			mu.Lock()
			imageBody = string(buf[:n])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"created":1,"data":[{"url":"https://example.com/i.png"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		case strings.HasPrefix(r.URL.Path, "/v1/videos"):
			mu.Lock()
			videoBody = string(buf[:n])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"vid_1","status":"queued"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	// A redaction-enabled channel: the media paths must ignore redaction entirely.
	newRedactChannelFull(t, "redact-media", upstream.URL, outbound.OutboundTypeOpenAIChat, true, "", 1)

	cRec := httptest.NewRecorder()
	cImg, _ := gin.CreateTestContext(cRec)
	reqImg := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"request-model","prompt":"contact a@example.com"}`))
	reqImg.Header.Set("Content-Type", "application/json")
	cImg.Request = reqImg
	cImg.Set("api_key_id", 0)
	cImg.Set("user_id", 0)
	cImg.Set("request_ip", "127.0.0.1")
	ImagesHandler("/images/generations", cImg)
	if cRec.Code != http.StatusOK {
		t.Fatalf("images handler status %d: %s", cRec.Code, cRec.Body.String())
	}

	cRecV := httptest.NewRecorder()
	cVid, _ := gin.CreateTestContext(cRecV)
	reqVid := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"request-model","prompt":"contact a@example.com"}`))
	reqVid.Header.Set("Content-Type", "application/json")
	cVid.Request = reqVid
	cVid.Set("api_key_id", 0)
	cVid.Set("user_id", 0)
	cVid.Set("request_ip", "127.0.0.1")
	VideosHandler(cVid)
	if cRecV.Code != http.StatusOK {
		t.Fatalf("videos handler status %d: %s", cRecV.Code, cRecV.Body.String())
	}

	mu.Lock()
	ib, vb := imageBody, videoBody
	mu.Unlock()
	if ib == "" || vb == "" {
		t.Fatalf("expected both media upstreams to be hit, images=%q videos=%q", ib, vb)
	}
	for name, body := range map[string]string{"images": ib, "videos": vb} {
		if !strings.Contains(body, "a@example.com") {
			t.Fatalf("%s path must forward the prompt verbatim, got %s", name, body)
		}
		if strings.Contains(body, "{{Redact:") || strings.Contains(body, "Sensitive values are redacted") {
			t.Fatalf("%s path must not redact (out of scope), got %s", name, body)
		}
	}
}

// TestRelayRedactIdentityFieldsEndToEnd: identity/tool-declaration metadata must
// survive the (redacted) chat request untouched while the message text is
// redacted — the module only rewrites message-bearing text, never identity,
// cursor, cache or tool-declaration structures.
func TestRelayRedactIdentityFieldsEndToEnd(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var upstreamBytes []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		upstreamBytes = append([]byte(nil), buf[:n]...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannelFull(t, "redact-identity", upstream.URL, outbound.OutboundTypeOpenAIChat, true, "", 1)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqBody := `{"model":"request-model","user":"user-abc-123","metadata":{"user_id":"meta-user-1","session_id":"sess-2"},"tools":[{"type":"function","function":{"name":"lookup_city","description":"lookup a weather city","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"messages":[{"role":"user","content":"my email is a@example.com"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	raw := upstreamBytes
	mu.Unlock()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, string(raw))
	}
	if gotUser, _ := body["user"].(string); gotUser != "user-abc-123" {
		t.Fatalf("user identity field mutated: %v", body["user"])
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		t.Fatalf("metadata identity field missing/mutated: %#v", body["metadata"])
	}
	mUserID, _ := meta["user_id"].(string)
	mSessionID, _ := meta["session_id"].(string)
	if mUserID != "meta-user-1" || mSessionID != "sess-2" {
		t.Fatalf("metadata identity fields mutated: %#v", body["metadata"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools declaration dropped/added: %#v", body["tools"])
	}
	tool0, _ := tools[0].(map[string]any)
	if tool0 == nil {
		t.Fatalf("tool declaration mutated: %#v", tools[0])
	}
	fn, _ := tool0["function"].(map[string]any)
	if fn == nil {
		t.Fatalf("tool declaration function missing: %#v", tools[0])
	}
	fnName, _ := fn["name"].(string)
	fnDesc, _ := fn["description"].(string)
	if fnName != "lookup_city" || fnDesc != "lookup a weather city" {
		t.Fatalf("tool declaration mutated: %#v", tools[0])
	}
	// The message text itself must still be redacted.
	if strings.Contains(string(raw), "a@example.com") {
		t.Fatalf("message secret not redacted: %s", string(raw))
	}
	if !strings.Contains(string(raw), "{{Redact:") {
		t.Fatalf("expected a placeholder for the redacted message: %s", string(raw))
	}
}

// TestRelayRedactBridgedHistoryRedacted: a plain responses client continuing on a
// CHAT channel triggers the history bridge (bridgeResponsesHistoryForChat), which
// rebuilds Messages from the prior turn's transcript. The transcript stores
// ORIGINAL (unredacted) text, so the redaction layer must redact BOTH the current
// turn and the bridged prior-turn text — a wholesale Messages replacement
// (reparse or otherwise) would have clobbered the rebuilt history; the per-field
// pass redacts the CURRENT field values in place and never whole-field-replaces
// the bridge output. Asserts: bridged old credential is a placeholder
// (not plaintext), current credential is a placeholder, the rebuilt history text is
// still present, >=2 distinct placeholders, notice injected, and the client gets
// both secrets restored (same-session round trip).
func TestRelayRedactBridgedHistoryRedacted(t *testing.T) {
	setupRedactDB(t)
	clearResponsesSessionCacheForTest()

	previous := "resp_redact_bridged_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("old contact is old@example.com")}},
	})

	var mu sync.Mutex
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		upstreamBody = string(buf[:n])
		mu.Unlock()
		tokens := make([]string, 0, 2)
		for _, m := range redactTokenRe.FindAllStringSubmatch(upstreamBody, -1) {
			if len(m) > 0 {
				tokens = append(tokens, m[0])
			}
		}
		joined := strings.Join(tokens, " ")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-b","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"echo ` + joined + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	newRedactChannel(t, upstream.URL, true)

	engine := gin.New()
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIResponse, c)
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqBody := `{"model":"request-model","previous_response_id":"` + previous + `","input":"new contact is new@example.com"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	engine.ServeHTTP(rec, c.Request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	up := upstreamBody
	mu.Unlock()
	if up == "" {
		t.Fatalf("upstream was not reached")
	}
	if strings.Contains(up, "old@example.com") {
		t.Fatalf("bridged prior-turn credential leaked to upstream (history text not redacted): %s", up)
	}
	if strings.Contains(up, "new@example.com") {
		t.Fatalf("current-turn credential leaked to upstream: %s", up)
	}
	if !strings.Contains(up, "old contact is") {
		t.Fatalf("bridged history was clobbered/absent upstream: %s", up)
	}
	if !strings.Contains(up, "Sensitive values are redacted") {
		t.Fatalf("notice must be injected into the bridged turn: %s", up)
	}
	distinct := make(map[string]struct{})
	for _, m := range redactTokenRe.FindAllStringSubmatch(up, -1) {
		if len(m) > 0 {
			distinct[m[0]] = struct{}{}
		}
	}
	if len(distinct) < 2 {
		t.Fatalf("expected >=2 distinct placeholders (bridged old + current new), got %#v", distinct)
	}
	if !strings.Contains(rec.Body.String(), "old@example.com") {
		t.Fatalf("client did not get the bridged secret restored: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new@example.com") {
		t.Fatalf("client did not get the current-turn secret restored: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to client: %s", rec.Body.String())
	}
}

// TestRelayRedactSynthesizedCodexInputRaw: a CHAT client routed onto a codex
// (OpenAI Responses) channel gets its Responses `input` SYNTHESIZED from Messages
// by prepareCodexRequestShape (applyTransformOptions) — BEFORE redaction runs.
// Redaction must therefore cover the synthesized input too, from the SAME session
// as the Messages pass (idempotent tokenFor), so both wire views carry the SAME
// placeholder, and it must never re-parse or replace the synthesized field down
// stream of the shape pass.
// Asserts: the upstream input array carries the credential only as a placeholder,
// and the client gets the echoed placeholder restored with no token leaked.
func TestRelayRedactSynthesizedCodexInputRaw(t *testing.T) {
	setupRedactDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRedactNoticeEnabled, "false"); err != nil {
		t.Fatalf("disable notice: %v", err)
	}

	var mu sync.Mutex
	var gotPath string
	var gotBody string
	var gotInputText string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawB, _ := io.ReadAll(r.Body)
		raw := string(rawB)
		var body map[string]any
		_ = json.Unmarshal([]byte(raw), &body)
		var text string
		if items, ok := body["input"].([]any); ok && len(items) > 0 {
			if item, ok := items[0].(map[string]any); ok {
				if content, ok := item["content"].([]any); ok && len(content) > 0 {
					if part, ok := content[0].(map[string]any); ok {
						if s, ok := part["text"].(string); ok {
							text = s
						}
					}
				}
			}
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = raw
		gotInputText = text
		mu.Unlock()
		// Echo the (placeholder) prompt text back so the client-format restore path is
		// exercised end to end.
		writeCodexShapeResponsesSSEWithText(w, "resp_redact_synth", "mapped-gpt-5.5", text)
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-codex-channel",
		Type:          outbound.OutboundTypeOpenAIResponse,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
		Model:         "mapped-gpt-5.5",
		ModelMapping:  map[string]string{"gpt-5.5": "mapped-gpt-5.5"},
		Priority:      1,
		RedactEnabled: true,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"contact me at old@example.com"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, up, inputText := gotPath, gotBody, gotInputText
	mu.Unlock()
	if path != "/v1/responses" {
		t.Fatalf("expected the codex responses upstream, got %q", path)
	}
	if inputText == "" {
		t.Fatalf("upstream never received a synthesized Responses input")
	}
	if strings.Contains(up, "old@example.com") {
		t.Fatalf("credential leaked inside the synthesized upstream request: %s", up)
	}
	if matched := redactTokenRe.MatchString(up); !matched {
		t.Fatalf("upstream request must carry the placeholder: %s", up)
	}
	if matched2 := redactTokenRe.MatchString(inputText); !matched2 {
		t.Fatalf("synthesized Responses input must be the redacted (placeholder) text, got: %s", inputText)
	}
	if !strings.Contains(rec.Body.String(), "old@example.com") {
		t.Fatalf("client did not get the credential restored: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}
}

// TestRelayRedactResponsesReasoningStateUntouched: a Responses client's raw input
// array carries a reasoning item (encrypted_content) plus a user text item with a
// credential. The scan runs on the {"input": [...]} WRAPPED view, so the core's
// openai_responses model-state skip matches (path ["input","<idx>"]) and the
// reasoning item survives byte-for-byte; on a bare array that skip misses and the
// high-entropy detector would redact encrypted_content, which a real codex upstream
// rejects with invalid_encrypted_content (dropResponsesEncryptedContent retry = the
// codex-shape cost regression this layer must not cause).
// Asserts: the reasoning item stays first with encrypted_content unchanged, the user
// credential is only a placeholder upstream, the notice IS injected into the raw
// input's first user item (for a responses session the wrapped scan is the notice
// carrier — redact.go's other branch), and the client gets the echoed placeholder
// restored.
func TestRelayRedactResponsesReasoningStateUntouched(t *testing.T) {
	setupRedactDB(t)

	const encryptedContent = "enc-neutral-0f3a91c47b28de56a1c90b3f7d42e85a6b19c0d3e7f2a48b5c6d0e9f1a2b3c4d"
	const userText = "my email is a@example.com"

	var mu sync.Mutex
	var gotPath string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawB, _ := io.ReadAll(r.Body)
		raw := string(rawB)
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = raw
		mu.Unlock()
		// Echo only the placeholder token as the model reply — the request's notice text
		// is not part of a real model answer — so the client-side restore assertion stays
		// independent of the notice body (which legitimately contains "{{Redact:sha256}}").
		token := redactTokenRe.FindString(raw)
		writeCodexShapeResponsesSSEWithText(w, "resp_redact_reasoning", "upstream-model", "the address is "+token)
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-codex-reasoning",
		Type:          outbound.OutboundTypeOpenAIResponse,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
		Model:         "upstream-model",
		ModelMapping:  map[string]string{"request-model": "upstream-model"},
		Priority:      1,
		RedactEnabled: true,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqBody := `{"model":"request-model","stream":false,"input":[{"type":"reasoning","encrypted_content":"` + encryptedContent + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"` + userText + `"}]}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, up := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/responses" {
		t.Fatalf("expected the codex responses upstream, got %q", path)
	}
	if up == "" {
		t.Fatalf("upstream was not reached")
	}
	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(up), &sent); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, up)
	}
	if len(sent.Input) < 2 {
		t.Fatalf("expected the reasoning + user input items upstream, got %#v", sent.Input)
	}
	if typ, _ := sent.Input[0]["type"].(string); typ != "reasoning" {
		t.Fatalf("expected the reasoning item to stay first, got %#v", sent.Input[0])
	}
	if sig, _ := sent.Input[0]["encrypted_content"].(string); sig != encryptedContent {
		t.Fatalf("encrypted_content must be forwarded byte-for-byte untouched, got %q", sig)
	}
	if strings.Contains(up, "a@example.com") {
		t.Fatalf("user credential leaked upstream: %s", up)
	}
	if matched := redactTokenRe.MatchString(up); !matched {
		t.Fatalf("expected a placeholder in the redacted input: %s", up)
	}
	if !strings.Contains(up, "Sensitive values are redacted") {
		t.Fatalf("notice must be injected into the raw input's first user item: %s", up)
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client did not get the credential restored: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}
}

// TestRelayRedactSynthesizedCodexInputRawNotice locks the chat-session notice path of
// redact.go's dual-path InputRaw handling: a CHAT client routed to a codex-shaped
// responses channel SYNTHESIZES the upstream input, and the session protocol
// (openai_chat) can never match the core's body.input injection branch — so
// InjectNoticeInputRaw must prepend the notice explicitly, or the wire silently loses
// it. Also locks single injection: the notice text appears EXACTLY ONCE in the whole
// upstream body (the follow-up RedactJSONBody call carries the session protocol, whose
// injectRedactNotice only matches body.messages — absent from the wrapper).
// The no-leak check uses the 64-hex token regex, not a raw "{{Redact:" substring: the
// notice itself legitimately contains the literal "{{Redact:sha256}}" example.
func TestRelayRedactSynthesizedCodexInputRawNotice(t *testing.T) {
	setupRedactDB(t) // notice stays ENABLED (the production default)

	var mu sync.Mutex
	var gotPath string
	var gotBody string
	var gotInputText string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawB, _ := io.ReadAll(r.Body)
		raw := string(rawB)
		var body map[string]any
		_ = json.Unmarshal([]byte(raw), &body)
		var text string
		if items, ok := body["input"].([]any); ok && len(items) > 0 {
			if item, ok := items[0].(map[string]any); ok {
				if content, ok := item["content"].([]any); ok && len(content) > 0 {
					if part, ok := content[0].(map[string]any); ok {
						if s, ok := part["text"].(string); ok {
							text = s
						}
					}
				}
			}
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = raw
		gotInputText = text
		mu.Unlock()
		writeCodexShapeResponsesSSEWithText(w, "resp_redact_synth_notice", "mapped-gpt-5.5", text)
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:          "redact-codex-channel-notice",
		Type:          outbound.OutboundTypeOpenAIResponse,
		Enabled:       true,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
		Model:         "mapped-gpt-5.5",
		ModelMapping:  map[string]string{"gpt-5.5": "mapped-gpt-5.5"},
		Priority:      1,
		RedactEnabled: true,
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"contact me at old@example.com"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, up, inputText := gotPath, gotBody, gotInputText
	mu.Unlock()
	if path != "/v1/responses" {
		t.Fatalf("expected the codex responses upstream, got %q", path)
	}
	if inputText == "" {
		t.Fatalf("upstream never received a synthesized Responses input")
	}
	if strings.Contains(up, "old@example.com") {
		t.Fatalf("credential leaked inside the synthesized upstream request: %s", up)
	}
	if n := strings.Count(up, "Sensitive values are redacted"); n != 1 {
		t.Fatalf("expected the notice exactly once on the wire (dual-path must not double-inject), got %d: %s", n, up)
	}
	if !strings.HasPrefix(inputText, "Sensitive values are redacted") {
		t.Fatalf("the synthesized chat input must carry the notice, got: %s", inputText)
	}
	if matched := redactTokenRe.MatchString(inputText); !matched {
		t.Fatalf("synthesized Responses input must carry the redacted placeholder, got: %s", inputText)
	}
	if !strings.Contains(rec.Body.String(), "old@example.com") {
		t.Fatalf("client did not get the credential restored: %s", rec.Body.String())
	}
	if redactTokenRe.MatchString(rec.Body.String()) {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}
}
