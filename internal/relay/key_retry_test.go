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
