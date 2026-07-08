package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestHandlerRetriesNextChannelKeyAfter429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)

	var (
		mu          sync.Mutex
		seenAuth    []string
		firstKeyHit bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seenAuth = append(seenAuth, auth)
		if auth == "Bearer key-one" {
			firstKeyHit = true
		}
		mu.Unlock()

		if auth == "Bearer key-one" {
			http.Error(w, `{"error":{"message":"rate limit"}}`, http.StatusTooManyRequests)
			return
		}
		if auth != "Bearer key-two" {
			http.Error(w, `{"error":{"message":"unexpected key"}}`, http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"upstream-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "multi-key-429",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "key-one", TotalCost: 0},
			{Enabled: true, ChannelKey: "key-two", TotalCost: 1},
		},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{
		Name: "request-model",
		Mode: dbmodel.GroupModeRoundRobin,
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "upstream-model",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"request-model",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry with second key to succeed, got status %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotFirstKeyHit := firstKeyHit
	gotAuth := append([]string(nil), seenAuth...)
	mu.Unlock()
	if !gotFirstKeyHit {
		t.Fatalf("expected first key to receive the 429 response")
	}

	wantAuth := []string{"Bearer key-one", "Bearer key-two"}
	if len(gotAuth) != len(wantAuth) {
		t.Fatalf("expected auth attempts %v, got %v", wantAuth, gotAuth)
	}
	for i := range wantAuth {
		if gotAuth[i] != wantAuth[i] {
			t.Fatalf("auth attempt %d: got %q, want %q", i, gotAuth[i], wantAuth[i])
		}
	}

	stored, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	statusByKey := map[string]int{}
	for _, key := range stored.Keys {
		statusByKey[key.ChannelKey] = key.StatusCode
	}
	if statusByKey["key-one"] != http.StatusTooManyRequests {
		t.Fatalf("expected key-one status 429, got %d", statusByKey["key-one"])
	}
	if statusByKey["key-two"] != http.StatusOK {
		t.Fatalf("expected key-two status 200, got %d", statusByKey["key-two"])
	}
}

func TestHandlerRetriesNextChannelAfterResponsesPreludeOnlyStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)

	firstHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHit = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_empty","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n"))
	}))
	t.Cleanup(first.Close)

	secondHit := false
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_ok","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_ok","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(second.Close)

	firstChannel := dbmodel.Channel{
		Name:    "prelude-only",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: first.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "first-key"}},
	}
	if err := op.ChannelCreate(&firstChannel, ctx); err != nil {
		t.Fatalf("create first channel: %v", err)
	}
	secondChannel := dbmodel.Channel{
		Name:    "responses-ok",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: second.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "second-key"}},
	}
	if err := op.ChannelCreate(&secondChannel, ctx); err != nil {
		t.Fatalf("create second channel: %v", err)
	}
	group := dbmodel.Group{Name: "gpt-5.5", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, item := range []dbmodel.GroupItem{
		{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
		{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "gpt-5.5", Priority: 2, Weight: 1},
	} {
		if err := op.GroupItemAdd(&item, ctx); err != nil {
			t.Fatalf("create group item: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.5",
		"input":"Say OK only",
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected second channel to succeed, got status %d body %s", rec.Code, rec.Body.String())
	}
	if !firstHit || !secondHit {
		t.Fatalf("expected both channels to be attempted, first=%t second=%t", firstHit, secondHit)
	}
	body := rec.Body.String()
	if strings.Contains(body, "resp_empty") {
		t.Fatalf("prelude-only first channel leaked into downstream body: %s", body)
	}
	if !strings.Contains(body, `"delta":"OK"`) || !strings.Contains(body, `"response.completed"`) {
		t.Fatalf("expected second channel OK stream, got %s", body)
	}
}

// TestHandlerFailsOverAfterAnthropicMessageStartOnlyStream reproduces the exact
// deepseek网页桥 symptom from the capture: an upstream opens the SSE stream, emits
// only the non-meaningful opener (message_start + ping), then dies before any
// content. Deferred commit keeps that opener buffered (client sees only ignorable
// comment heartbeats), so the relay must fail over to a healthy channel instead of
// committing the empty opener and death-gripping the broken upstream. Before the
// fix the message_start was flushed immediately (Writer.Written()==true), which
// disabled both transient retry and cross-channel failover.
func TestHandlerFailsOverAfterAnthropicMessageStartOnlyStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)

	deadHits := 0
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits++
		w.Header().Set("Content-Type", "text/event-stream")
		// Opener only, then the connection ends — no content_block_delta, no message_stop.
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_shell_dead","type":"message","role":"assistant","model":"claude-up","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}

event: ping
data: {"type":"ping"}

`))
	}))
	t.Cleanup(dead.Close)

	healthyHit := false
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHit = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_healthy","type":"message","role":"assistant","model":"claude-up","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(healthy.Close)

	deadChannel := dbmodel.Channel{
		Name:     "message-start-then-dead",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: dead.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "dead-key"}},
	}
	if err := op.ChannelCreate(&deadChannel, ctx); err != nil {
		t.Fatalf("create dead channel: %v", err)
	}
	healthyChannel := dbmodel.Channel{
		Name:     "anthropic-healthy",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: healthy.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "healthy-key"}},
	}
	if err := op.ChannelCreate(&healthyChannel, ctx); err != nil {
		t.Fatalf("create healthy channel: %v", err)
	}
	group := dbmodel.Group{Name: "claude-req", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, item := range []dbmodel.GroupItem{
		{GroupID: group.ID, ChannelID: deadChannel.ID, ModelName: "claude-up", Priority: 1, Weight: 1},
		{GroupID: group.ID, ChannelID: healthyChannel.ID, ModelName: "claude-up", Priority: 2, Weight: 1},
	} {
		if err := op.GroupItemAdd(&item, ctx); err != nil {
			t.Fatalf("create group item: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-req",
		"max_tokens":16,
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected failover to healthy channel to succeed, got status %d body %s", rec.Code, rec.Body.String())
	}
	if deadHits == 0 || !healthyHit {
		t.Fatalf("expected both channels attempted, deadHits=%d healthyHit=%t", deadHits, healthyHit)
	}
	body := rec.Body.String()
	if strings.Contains(body, "msg_shell_dead") {
		t.Fatalf("dead channel's buffered opener leaked into downstream body: %s", body)
	}
	if !strings.Contains(body, `"text":"ok"`) && !strings.Contains(body, `"text_delta","text":"ok"`) {
		t.Fatalf("expected healthy channel content in downstream body, got %s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("expected healthy channel message_stop, got %s", body)
	}
}

func setupRelayKeyRetryDB(t *testing.T) context.Context {
	t.Helper()

	balancer.ResetRuntimeTelemetry()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	return context.Background()
}
