package relay

import (
	"context"
	"encoding/json"
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

func TestAnthropicStreamForwardsPingKeepalive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// The upstream ping sits AFTER the first content delta on purpose: with
		// deferred commit, message_start is buffered until real content arrives, so a
		// ping that precedes content is downgraded to an ignorable ":\n\n" comment
		// (an "event: ping" is illegal before message_start). Once content commits, the
		// native Anthropic ping is forwarded as-is — that is what this test guards.
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_keepalive","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: ping
data: {"type":"ping"}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "anthropic-ping-upstream",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-upstream",
		ModelMapping: map[string]string{
			"claude-request": "claude-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-request",
		"max_tokens":16,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected anthropic stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: ping") || !strings.Contains(body, `"type":"ping"`) {
		t.Fatalf("expected downstream anthropic ping keepalive, got %s", body)
	}
	if !strings.Contains(body, "event:message_stop") && !strings.Contains(body, "event: message_stop") {
		t.Fatalf("expected downstream message_stop, got %s", body)
	}
}

func TestAnthropicStreamReturnsAfterMessageStopEvenWhenUpstreamKeepsPinging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_terminal_ping","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
		if flusher != nil {
			flusher.Flush()
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "anthropic-terminal-ping-upstream",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-upstream",
		ModelMapping: map[string]string{
			"claude-terminal-ping-request": "claude-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-terminal-ping-request",
		"max_tokens":16,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	started := time.Now()
	Handler(inbound.InboundTypeAnthropic, c)
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected anthropic stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("message_stop should end relay without waiting for post-stop pings, elapsed=%s body=%s", elapsed, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"text":"OK"`, "event:message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected body to contain %q, got %s", want, body)
		}
	}
}

func TestAnthropicStreamFallsBackToNonStreamUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %q", r.URL.Path)
		}
		calls++
		var payload struct {
			Stream *bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if calls == 1 {
			if payload.Stream == nil || !*payload.Stream {
				t.Fatalf("first upstream request should be stream")
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if payload.Stream != nil && *payload.Stream {
			t.Fatalf("fallback upstream request should be non-stream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_fallback",
			"type":"message",
			"role":"assistant",
			"model":"claude-upstream",
			"content":[{"type":"text","text":"OK"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "anthropic-stream-fallback-upstream",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-upstream",
		ModelMapping: map[string]string{
			"claude-fallback-request": "claude-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-fallback-request",
		"max_tokens":16,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("expected stream request plus non-stream fallback, got %d calls", calls)
	}
	if rec.Header().Get("X-Octopus-Stream-Fallback") != "non-stream-upstream" {
		t.Fatalf("expected fallback header, got %q", rec.Header().Get("X-Octopus-Stream-Fallback"))
	}
	body := rec.Body.String()
	for _, want := range []string{"message_start", `"text":"OK"`, "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected fallback SSE body to contain %q, got %s", want, body)
		}
	}
}

func TestAnthropicStreamErrorEventFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "Claude-Error-Stream",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-opus-4-8",
		ModelMapping: map[string]string{
			"claude-error": "claude-opus-4-8",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-error",
		"max_tokens":128,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected Anthropic error event to fail, got %d body %s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicStreamErrorEventAfterContentFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_error_after_content","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"busy after partial"}}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "Claude-Error-After-Content",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-opus-4-8",
		ModelMapping: map[string]string{
			"claude-error-after-content": "claude-opus-4-8",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-error-after-content",
		"max_tokens":128,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("expected partial content before error, got %s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("stream error after content must not synthesize success stop, got %s", body)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream stream failed before terminal event") {
		t.Fatalf("stream error after content should send explicit Anthropic error event, got %s", body)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 || logs[0].ErrorStatus == 0 {
		t.Fatalf("expected failed relay log after stream error, logs=%#v", logs)
	}
}

func TestAnthropicCPAOneMillionStreamPrefersStreamUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %q", r.URL.Path)
		}
		calls++
		var payload struct {
			Stream *bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if payload.Stream == nil || !*payload.Stream {
			t.Fatalf("CPA [1m] should try real stream upstream first")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_cpa_1m","type":"message","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "Claude-CPA",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-opus-4-7[1m]",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-4-7[1m]",
		"max_tokens":128000,
		"stream":true,
		"tools":[],
		"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.126")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Timeout", "600")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected CPA [1m] stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("expected one stream upstream call, got %d", calls)
	}
	if got := rec.Header().Get("X-Octopus-Stream-Fallback"); got != "" {
		t.Fatalf("did not expect fallback header, got %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"text":"OK"`) || strings.Contains(body, "Service Unavailable") {
		t.Fatalf("unexpected stream body: %s", body)
	}
}

func TestAnthropicOneMillionPlainClientGetsClaudeCompatibleStreamShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	// adaptive thinking + clear_thinking context management are now opt-in.
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeCLIAutoCompact, "true"); err != nil {
		t.Fatalf("set auto compact: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeCLIReasoningEffort, "high"); err != nil {
		t.Fatalf("set reasoning effort: %v", err)
	}

	var sawPath string
	var sawModel string
	var sawStream bool
	var sawBeta string
	var sawAPIKey string
	var sawAuthorization string
	var sawUserAgent string
	var sawClientRequestID string
	var sawClaudeSessionID string
	var sawTrace string
	var sawStainlessTimeout string
	var sawContextManagement string
	var sawThinking string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawBeta = r.Header.Get("Anthropic-Beta")
		sawAPIKey = r.Header.Get("X-API-Key")
		sawAuthorization = r.Header.Get("Authorization")
		sawUserAgent = r.Header.Get("User-Agent")
		sawClientRequestID = r.Header.Get("X-Client-Request-Id")
		sawClaudeSessionID = r.Header.Get("X-Claude-Code-Session-Id")
		sawTrace = r.Header.Get("AH-Trace-Id")
		sawStainlessTimeout = r.Header.Get("X-Stainless-Timeout")
		var payload struct {
			Model  string `json:"model"`
			Stream *bool  `json:"stream"`
		}
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if data, err := json.Marshal(raw["context_management"]); err == nil {
			sawContextManagement = string(data)
		}
		if data, err := json.Marshal(raw["thinking"]); err == nil {
			sawThinking = string(data)
		}
		rawData, _ := json.Marshal(raw)
		if err := json.Unmarshal(rawData, &payload); err != nil {
			t.Fatalf("decode upstream request shape: %v", err)
		}
		sawModel = payload.Model
		sawStream = payload.Stream != nil && *payload.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_plain_1m","type":"message","role":"assistant","model":"claude-opus-4-8[1m]","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "the relay Claude 1M",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-opus-4-8[1m]",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-4-8[1m]",
		"max_tokens":128000,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plain-http-client/1.0")
	req.Header.Set("Authorization", "Bearer client-should-not-leak")
	req.Header.Set("AH-Trace-Id", "trace-should-not-leak")
	req.Header.Set("X-Stainless-Timeout", "1")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected plain-client [1m] stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if sawPath != "/v1/messages" || sawModel != "claude-opus-4-8" || !sawStream {
		t.Fatalf("unexpected upstream shape: path=%q model=%q stream=%t", sawPath, sawModel, sawStream)
	}
	if !strings.Contains(sawBeta, defaultClaudeOneMillionBeta) {
		t.Fatalf("expected 1m beta %q, got %q", defaultClaudeOneMillionBeta, sawBeta)
	}
	if sawAPIKey != "anthropic-key" || sawAuthorization != "Bearer anthropic-key" {
		t.Fatalf("unexpected upstream auth headers: x-api-key=%q authorization=%q", sawAPIKey, sawAuthorization)
	}
	if sawClaudeSessionID == "" {
		t.Fatalf("expected X-Claude-Code-Session-Id to be set, got %q", sawClaudeSessionID)
	}
	// Genuine claude-cli (2.1.168 and 2.1.178, captured on the wire) does NOT send
	// X-Client-Request-Id; synthesizing one is a detectable non-CLI tell, so octopus
	// must leave it absent upstream.
	if sawClientRequestID != "" {
		t.Fatalf("X-Client-Request-Id must be absent to match genuine claude-cli, got %q", sawClientRequestID)
	}
	if !strings.Contains(sawThinking, `"adaptive"`) || !strings.Contains(sawContextManagement, "clear_thinking_20251015") {
		t.Fatalf("expected Claude CLI-like 1M body shape, thinking=%s context=%s", sawThinking, sawContextManagement)
	}
	if sawUserAgent == "plain-http-client/1.0" || sawTrace != "" || sawStainlessTimeout == "1" {
		t.Fatalf("client headers leaked upstream: ua=%q trace=%q stainless_timeout=%q", sawUserAgent, sawTrace, sawStainlessTimeout)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"text":"OK"`) {
		t.Fatalf("unexpected downstream body: %s", body)
	}
}

// TestAnthropicOneMillionPlainClientWithOrdinarySystemGetsFallbackTools locks the
// captured failure mode: octopus injects metadata before 1M shape preparation, while a
// plain client may already have an ordinary system prompt. That is not a genuine Claude
// Code request, so the fallback body must still include the small CLI tool triplet.
func TestAnthropicOneMillionPlainClientWithOrdinarySystemGetsFallbackTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var sawSystem []struct {
		Text string `json:"text"`
	}
	var sawTools []struct {
		Name string `json:"name"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			System json.RawMessage `json:"system"`
			Tools  json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if err := json.Unmarshal(raw.System, &sawSystem); err != nil {
			t.Fatalf("decode upstream system: %v (raw %s)", err, string(raw.System))
		}
		if err := json.Unmarshal(raw.Tools, &sawTools); err != nil {
			t.Fatalf("decode upstream tools: %v (raw %s)", err, string(raw.Tools))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_plain_tools","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:               "the relay Claude 1M fallback tools",
		Type:               outbound.OutboundTypeAnthropic,
		Enabled:            true,
		AnthropicContext1M: true,
		Model:              "claude-opus-4-8",
		BaseUrls:           []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:               []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
		SelectedModels:     []string{"claude-opus-4-8"},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-4-8",
		"max_tokens":32,
		"stream":false,
		"system":[{"type":"text","text":"Answer in one word.","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"Reply with exactly OK."}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected plain-client [1m] request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if len(sawSystem) != 3 || !strings.Contains(sawSystem[2].Text, "Answer in one word") {
		t.Fatalf("ordinary client system prompt should survive after billing+identity, got %#v", sawSystem)
	}
	seenTools := map[string]bool{}
	for _, tool := range sawTools {
		seenTools[tool.Name] = true
	}
	for _, want := range []string{"Bash", "Edit", "Read"} {
		if !seenTools[want] {
			t.Fatalf("expected fallback Claude Code tool %q in %#v", want, sawTools)
		}
	}
}

// TestAnthropicOneMillionPlainClientCloakOnEmitsCanonicalClaudeIdentity locks the
// cloak=auto/always side of the F1 fix. After the relay-side identity injection was
// deleted from prepareClaudeOneMillionPlainClientShape, the canonical cloak-gated paths
// must still produce a genuine Claude shape for a plain (non-CLI) [1m] client:
//   - the outbound system is EXACTLY [billing header, agent identity], the agent
//     identity appearing once, right after the billing header. This proves the Anthropic
//     transformer refills the identity block the relay no longer injects. If this
//     assertion regresses, the relay-side deletion dropped identity and the fix must
//     fall back to a cloak gate rather than a deletion.
//   - metadata.user_id is the canonical compact form: 64-hex device_id, golden key order
//     device_id/account_uuid/session_id (NOT the alphabetical map-marshal tell), and a
//     body session_id equal to the X-Claude-Code-Session-Id header (one UUID in both
//     places, like real Claude Code).
func TestAnthropicOneMillionPlainClientCloakOnEmitsCanonicalClaudeIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	const claudeAgentIdentity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	const billingHeaderPrefix = "x-anthropic-billing-header:"

	var sawSystem []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var sawMetadataUserID string
	var sawClaudeSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClaudeSessionID = r.Header.Get("X-Claude-Code-Session-Id")
		var payload struct {
			System   json.RawMessage `json:"system"`
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		sawMetadataUserID = payload.Metadata.UserID
		if len(payload.System) > 0 {
			if err := json.Unmarshal(payload.System, &sawSystem); err != nil {
				t.Errorf("decode upstream system: %v (raw %s)", err, string(payload.System))
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_plain_1m_cloakon","type":"message","role":"assistant","model":"claude-opus-4-8[1m]","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "the relay Claude 1M CloakOn",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		Model:    "claude-opus-4-8[1m]",
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-4-8[1m]",
		"max_tokens":128000,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	// Force a deterministic client session so the X-Claude-Code-Session-Id header and the
	// body metadata.user_id.session_id derive from one seed (real Claude Code uses one
	// UUID in both places), making the body==header assertion below stable.
	req.Header.Set("X-Session-Id", "f1-cloak-on-session")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected cloak-on [1m] stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}

	// System must be exactly [billing header, agent identity], identity once after billing.
	if len(sawSystem) != 2 {
		t.Fatalf("outbound system must be [billing, identity], got %d parts: %#v", len(sawSystem), sawSystem)
	}
	if !strings.HasPrefix(strings.TrimSpace(sawSystem[0].Text), billingHeaderPrefix) {
		t.Fatalf("system[0] must be the billing header (prefix %q), got %q", billingHeaderPrefix, sawSystem[0].Text)
	}
	if sawSystem[1].Text != claudeAgentIdentity {
		t.Fatalf("system[1] must be the claude agent identity, got %q", sawSystem[1].Text)
	}
	identityCount := 0
	for _, part := range sawSystem {
		if strings.Contains(part.Text, "built on Anthropic's Claude Agent SDK") {
			identityCount++
		}
	}
	if identityCount != 1 {
		t.Fatalf("claude agent identity must appear exactly once, got %d in %#v", identityCount, sawSystem)
	}

	// metadata.user_id must be the canonical compact form (not the alphabetical tell).
	if sawMetadataUserID == "" {
		t.Fatalf("cloak-on [1m] must inject metadata.user_id")
	}
	di := strings.Index(sawMetadataUserID, `"device_id"`)
	ai := strings.Index(sawMetadataUserID, `"account_uuid"`)
	si := strings.Index(sawMetadataUserID, `"session_id"`)
	if !(di >= 0 && di < ai && ai < si) {
		t.Fatalf("metadata.user_id key order must be device_id,account_uuid,session_id (canonical), got %q", sawMetadataUserID)
	}
	var meta struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(sawMetadataUserID), &meta); err != nil {
		t.Fatalf("metadata.user_id is not JSON: %q (%v)", sawMetadataUserID, err)
	}
	if len(meta.DeviceID) != 64 {
		t.Fatalf("device_id must be 64-hex like real Claude Code, got %d chars: %q", len(meta.DeviceID), meta.DeviceID)
	}
	if meta.SessionID != sawClaudeSessionID {
		t.Fatalf("body session_id %q must equal header X-Claude-Code-Session-Id %q", meta.SessionID, sawClaudeSessionID)
	}
}

func TestAnthropicNativeClaudeOneMillionShapeIsNotSynthesized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var sawModel string
	var sawBeta string
	var sawThinking string
	var sawOutputConfig string
	var sawContextManagement string
	var sawToolsPresent bool
	var sawToolsLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBeta = r.Header.Get("Anthropic-Beta")
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if modelName, _ := raw["model"].(string); modelName != "" {
			sawModel = modelName
		}
		if data, err := json.Marshal(raw["thinking"]); err == nil {
			sawThinking = string(data)
		}
		if data, err := json.Marshal(raw["output_config"]); err == nil {
			sawOutputConfig = string(data)
		}
		if data, err := json.Marshal(raw["context_management"]); err == nil {
			sawContextManagement = string(data)
		}
		if tools, ok := raw["tools"].([]any); ok {
			sawToolsPresent = true
			sawToolsLen = len(tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_native_1m","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:               "the relay Claude native 1M",
		Type:               outbound.OutboundTypeAnthropic,
		Enabled:            true,
		AnthropicContext1M: true,
		Model:              "claude-opus-4-8",
		BaseUrls:           []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:               []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
		SelectedModels:     []string{"claude-opus-4-8"},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-4-8",
		"max_tokens":64000,
		"stream":true,
		"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"\",\"session_id\":\"session\"}"},
		"system":[{"type":"text","text":"You are a Claude agent."}],
		"thinking":{"type":"disabled"},
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},
		"tools":[],
		"messages":[{"role":"user","content":[{"type":"text","text":"<session>Reply with exactly OK.</session>"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,context-1m-2025-08-07,structured-outputs-2025-12-15")
	req.Header.Set("Authorization", "Bearer client-should-not-leak")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected native Claude [1m] stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if sawModel != "claude-opus-4-8" {
		t.Fatalf("unexpected upstream model %q", sawModel)
	}
	for _, want := range []string{defaultClaudeOneMillionBeta, "structured-outputs-2025-12-15"} {
		if !strings.Contains(sawBeta, want) {
			t.Fatalf("expected beta %q in %q", want, sawBeta)
		}
	}
	if !strings.Contains(sawThinking, `"disabled"`) {
		t.Fatalf("native thinking should stay disabled, got %s", sawThinking)
	}
	if !strings.Contains(sawOutputConfig, "json_schema") || !strings.Contains(sawOutputConfig, "additionalProperties") {
		t.Fatalf("native output_config format was not preserved: %s", sawOutputConfig)
	}
	if sawContextManagement != "null" {
		t.Fatalf("native title request should not synthesize context_management, got %s", sawContextManagement)
	}
	if !sawToolsPresent || sawToolsLen != 0 {
		t.Fatalf("native empty tools array was not preserved, present=%t len=%d", sawToolsPresent, sawToolsLen)
	}
}

func TestAnthropicOpusOneMillionShortcutRoutesToClaude48(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var sawModel, sawBeta string
	var sawStream bool
	var sawMetadata map[string]any
	var sawSystem any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string         `json:"model"`
			Stream   bool           `json:"stream"`
			Metadata map[string]any `json:"metadata"`
			System   any            `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		sawModel = body.Model
		sawStream = body.Stream
		sawMetadata = body.Metadata
		sawSystem = body.System
		sawBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_alias","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}
`))
	}))
	defer upstream.Close()

	channel := dbmodel.Channel{
		Name:    "the relay Claude 4-8",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-opus-4-8",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"opus[1m]",
		"max_tokens":128000,
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected opus[1m] alias stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if sawModel != "claude-opus-4-8" || !sawStream {
		t.Fatalf("unexpected upstream shape: model=%q stream=%t", sawModel, sawStream)
	}
	if sawMetadata["user_id"] == nil || sawSystem == nil {
		t.Fatalf("expected synthesized Claude agent metadata/system, metadata=%#v system=%#v", sawMetadata, sawSystem)
	}
	if !strings.Contains(sawBeta, defaultClaudeOneMillionBeta) {
		t.Fatalf("expected 1m beta %q, got %q", defaultClaudeOneMillionBeta, sawBeta)
	}
}

func TestStreamKeepaliveEventDetection(t *testing.T) {
	if !isStreamKeepaliveEvent("ping", `{"type":"message_start"}`) {
		t.Fatalf("expected SSE event type ping to be recognized")
	}
	if !isStreamKeepaliveEvent("", `{"type":"ping"}`) {
		t.Fatalf("expected JSON type ping to be recognized")
	}
	if isStreamKeepaliveEvent("", `{"type":"content_block_stop"}`) {
		t.Fatalf("did not expect content block stop to be treated as keepalive")
	}
}

func TestCurrentStreamKeepaliveIntervalUsesSetting(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_STREAM_KEEPALIVE_INTERVAL_SECONDS", "")
	setupRelayErrorDB(t)

	if got := currentStreamKeepaliveInterval(); got != 15*time.Second {
		t.Fatalf("expected default keepalive interval 15s, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "0"); err != nil {
		t.Fatalf("disable keepalive setting: %v", err)
	}
	if got := currentStreamKeepaliveInterval(); got != 0 {
		t.Fatalf("expected disabled keepalive interval, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "30"); err != nil {
		t.Fatalf("set keepalive setting: %v", err)
	}
	if got := currentStreamKeepaliveInterval(); got != 30*time.Second {
		t.Fatalf("expected keepalive interval 30s, got %s", got)
	}
}

func TestCurrentStreamDataIntervalTimeoutUsesSetting(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_STREAM_DATA_INTERVAL_TIMEOUT_SECONDS", "")
	setupRelayErrorDB(t)

	if got := currentStreamDataIntervalTimeout(); got != 900*time.Second {
		t.Fatalf("expected default stream data interval timeout 900s, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "0"); err != nil {
		t.Fatalf("disable stream data timeout setting: %v", err)
	}
	if got := currentStreamDataIntervalTimeout(); got != 0 {
		t.Fatalf("expected disabled stream data interval timeout, got %s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "45"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}
	if got := currentStreamDataIntervalTimeout(); got != 45*time.Second {
		t.Fatalf("expected stream data interval timeout 45s, got %s", got)
	}
}

func TestStreamDataIntervalTimeoutStopsSilentUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "0"); err != nil {
		t.Fatalf("disable keepalive setting: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_timeout","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "silent-anthropic-upstream",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-upstream",
		ModelMapping: map[string]string{
			"claude-timeout-request": "claude-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-timeout-request",
		"max_tokens":16,
		"stream":true,
		"messages":[{"role":"user","content":"timeout"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	startedAt := time.Now()
	Handler(inbound.InboundTypeAnthropic, c)
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("expected silent upstream to be stopped quickly, elapsed %s", elapsed)
	}
	// Deferred commit buffers the message_start opener until real content arrives, so
	// a silent-after-opener upstream commits nothing downstream. With keepalive
	// disabled here, the data-interval timeout surfaces as a clean HTTP 504 (which is
	// retryable and would fail over to another channel) instead of a committed but
	// dead SSE stream. The buffered opener must NOT leak into the body.
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 gateway timeout for silent-after-opener upstream, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "octopus_upstream_stream_timeout") {
		t.Fatalf("expected stream-timeout error code, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("deferred-commit opener must not leak before content, got %s", rec.Body.String())
	}
}
