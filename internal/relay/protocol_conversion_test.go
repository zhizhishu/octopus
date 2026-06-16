package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	geminiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestGeminiNativeRequestConvertsThroughOpenAIChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		gotPath  string
		gotAuth  string
		gotModel string
		gotText  string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body transformerModel.InternalLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotModel = body.Model
		if len(body.Messages) > 0 && body.Messages[0].Content.Content != nil {
			gotText = *body.Messages[0].Content.Content
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-gemini-conversion",
			"object":"chat.completion",
			"created":123,
			"model":"upstream-openai",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok from openai"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "openai-for-gemini-native",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "openai-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-gemini", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "upstream-openai",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/request-gemini:generateContent", strings.NewReader(`{
		"contents":[{"role":"user","parts":[{"text":"ping gemini"}]}],
		"generationConfig":{"maxOutputTokens":16}
	}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(geminiInbound.WithRequestOptions(req.Context(), "request-gemini", false))
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeGemini, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gemini native request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, auth, modelName, text := gotPath, gotAuth, gotModel, gotText
	mu.Unlock()
	if path != "/v1/chat/completions" {
		t.Fatalf("expected OpenAI chat path, got %q", path)
	}
	if auth != "Bearer openai-key" {
		t.Fatalf("unexpected auth: %q", auth)
	}
	if modelName != "upstream-openai" {
		t.Fatalf("expected upstream model rewrite, got %q", modelName)
	}
	if text != "ping gemini" {
		t.Fatalf("expected Gemini text converted to OpenAI message, got %q", text)
	}

	var geminiResp transformerModel.GeminiGenerateContentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &geminiResp); err != nil {
		t.Fatalf("unmarshal gemini response: %v", err)
	}
	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil || len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("unexpected gemini response candidates: %+v", geminiResp.Candidates)
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text != "ok from openai" {
		t.Fatalf("unexpected gemini response text: %q", geminiResp.Candidates[0].Content.Parts[0].Text)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].RequestModelName != "request-gemini" || logs[0].ActualModelName != "upstream-openai" {
		t.Fatalf("unexpected log models: request=%q actual=%q", logs[0].RequestModelName, logs[0].ActualModelName)
	}
	if logs[0].InputTokens != 4 || logs[0].OutputTokens != 3 {
		t.Fatalf("unexpected log usage: input=%d output=%d", logs[0].InputTokens, logs[0].OutputTokens)
	}
	if logs[0].RequestEndpoint != "gemini_generate_content" || logs[0].RequestPath != "/v1beta/models/request-gemini:generateContent" {
		t.Fatalf("unexpected log endpoint: endpoint=%q path=%q", logs[0].RequestEndpoint, logs[0].RequestPath)
	}
}

func TestCustomOpenAIChatChannelUsesConfiguredEndpointPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		gotPath  string
		gotAuth  string
		gotModel string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body transformerModel.InternalLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotModel = body.Model
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-custom",
			"object":"chat.completion",
			"created":123,
			"model":"glm-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:           "zhipu-custom-openai",
		Type:           outbound.OutboundTypeCustomOpenAIChat,
		Enabled:        true,
		OpenAIChatPath: "/chat/completions",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL + "/api/coding/paas/v4",
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "custom-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-glm", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "glm-upstream",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"request-glm",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected custom OpenAI chat request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, auth, modelName := gotPath, gotAuth, gotModel
	mu.Unlock()
	if path != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("unexpected upstream path: %q", path)
	}
	if auth != "Bearer custom-key" {
		t.Fatalf("unexpected auth: %q", auth)
	}
	if modelName != "glm-upstream" {
		t.Fatalf("expected upstream model rewrite, got %q", modelName)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 || len(logs[0].Attempts) == 0 {
		t.Fatalf("expected relay log attempt")
	}
	if logs[0].Attempts[0].UpstreamPath != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("expected audited custom upstream path, got %+v", logs[0].Attempts)
	}
}

func TestOpenAIResponsesFallsBackToChatForCompatibleUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu        sync.Mutex
		paths     []string
		gotAuth   string
		gotModel  string
		gotPrompt string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"responses endpoint is not supported by this compatible upstream","code":"invalid_request_error"}}`))
			return
		case "/v1/chat/completions":
			var body transformerModel.InternalLLMRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode chat fallback body: %v", err)
			}
			mu.Lock()
			gotAuth = r.Header.Get("Authorization")
			gotModel = body.Model
			if len(body.Messages) > 0 && body.Messages[0].Content.Content != nil {
				gotPrompt = *body.Messages[0].Content.Content
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-cpa-fallback",
				"object":"chat.completion",
				"created":123,
				"model":"gpt-5.5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok from cpa chat fallback"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}
			}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected path"}}`))
			return
		}
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "GPT-CPA",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "cpa-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-gpt", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"request-gpt",
		"input":"ping cpa"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected responses request to succeed via chat fallback, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	auth, modelName, prompt := gotAuth, gotModel, gotPrompt
	mu.Unlock()
	if strings.Join(gotPaths, ",") != "/v1/responses,/v1/chat/completions" {
		t.Fatalf("expected responses then chat fallback paths, got %#v", gotPaths)
	}
	if auth != "Bearer cpa-key" {
		t.Fatalf("unexpected auth: %q", auth)
	}
	if modelName != "gpt-5.5" {
		t.Fatalf("expected upstream model rewrite, got %q", modelName)
	}
	if prompt != "ping cpa" {
		t.Fatalf("expected prompt converted from Responses input, got %q", prompt)
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) || !strings.Contains(rec.Body.String(), "ok from cpa chat fallback") {
		t.Fatalf("expected responses-shaped body from chat fallback, got %s", rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].RequestEndpoint != "responses" || logs[0].RequestPath != "/v1/responses" {
		t.Fatalf("unexpected log endpoint: endpoint=%q path=%q", logs[0].RequestEndpoint, logs[0].RequestPath)
	}
	if logs[0].RequestModelName != "request-gpt" || logs[0].ActualModelName != "gpt-5.5" {
		t.Fatalf("unexpected log models: request=%q actual=%q", logs[0].RequestModelName, logs[0].ActualModelName)
	}
	if len(logs[0].Attempts) != 1 || logs[0].Attempts[0].UpstreamPath != "/v1/responses -> /v1/chat/completions" {
		t.Fatalf("expected audited upstream path to include responses rejection and chat fallback, attempts=%+v", logs[0].Attempts)
	}
}

func TestOpenAIResponsesFallsBackToChatForCompatibleUpstream503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		paths    []string
		gotTools int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream responses proxy unavailable","code":"service_unavailable"}}`))
			return
		case "/v1/chat/completions":
			var body transformerModel.InternalLLMRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode chat fallback body: %v", err)
			}
			mu.Lock()
			gotTools = len(body.Tools)
			mu.Unlock()
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-cpa-fallback-503",
				"object":"chat.completion",
				"created":123,
				"model":"gpt-5.5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok from 503 chat fallback"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}
			}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected path"}}`))
			return
		}
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "muyuan-like-response",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "cpa-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-gpt-503", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"request-gpt-503",
		"input":"ping cpa",
		"tools":[{"type":"function","name":"exec_command","description":"run command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
		"tool_choice":"auto"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected responses request to succeed via chat fallback after 503, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	tools := gotTools
	mu.Unlock()
	if strings.Join(gotPaths, ",") != "/v1/responses,/v1/chat/completions" {
		t.Fatalf("expected responses then chat fallback paths, got %#v", gotPaths)
	}
	if tools != 1 {
		t.Fatalf("expected function tools preserved in chat fallback, got %d", tools)
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) || !strings.Contains(rec.Body.String(), "ok from 503 chat fallback") {
		t.Fatalf("expected responses-shaped body from 503 chat fallback, got %s", rec.Body.String())
	}
}

func TestOpenAIResponsesChatFallbackSynthesizesDoneOnUpstreamEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient account balance","type":"bad_response_status_code","code":"bad_response_status_code"}}`))
			return
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_eof\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_eof\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "muyuan-like-eof",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "cpa-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-gpt-eof", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"request-gpt-eof",
		"input":"ping cpa",
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected streamed fallback request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if created := strings.Index(got, `"type":"response.created"`); created < 0 {
		t.Fatalf("expected prelude response.created before slow chat fallback chunks, got %s", got)
	} else if delta := strings.Index(got, `"type":"response.output_text.delta"`); delta < 0 || created > delta {
		t.Fatalf("expected response.created before output delta, got %s", got)
	}
	if !strings.Contains(got, `"type":"response.completed"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("expected synthesized response.completed and DONE after upstream EOF, got %s", got)
	}
	if !strings.Contains(got, `"input_tokens_details":{"cached_tokens":0`) {
		t.Fatalf("expected synthesized usage to include cached_tokens=0 for Codex parser, got %s", got)
	}
	if !strings.Contains(got, `"output_tokens_details":{"reasoning_tokens":0`) {
		t.Fatalf("expected synthesized usage to include reasoning_tokens=0 for Codex parser, got %s", got)
	}
}

func TestAnthropicMessagesViaOpenAIChatSynthesizesStopOnUpstreamEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_anthropic_eof\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_anthropic_eof\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "openai-chat-for-anthropic-eof",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "openai-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-claude-eof", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"request-claude-eof",
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
		t.Fatalf("expected anthropic converted stream to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{`"text":"ok"`, "event:message_delta", `"stop_reason":"end_turn"`, "event:message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected synthesized anthropic stream to contain %q, got %s", want, got)
		}
	}
	if strings.Contains(got, "data: [DONE]") {
		t.Fatalf("anthropic messages stream should finish with message_stop, not OpenAI [DONE], got %s", got)
	}
}

func TestOpenAIResponsesStreamLogsChatStyleUsageAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_alias\",\"object\":\"response\",\"model\":\"glm-upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_alias\",\"object\":\"response\",\"model\":\"glm-upstream\",\"status\":\"completed\",\"output\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14,\"prompt_tokens_details\":{\"cached_tokens\":4}}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "responses-compatible-alias-usage",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "request-glm-responses", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "glm-upstream",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"request-glm-responses",
		"input":"ping",
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected streamed responses request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"response.completed"`) || !strings.Contains(rec.Body.String(), `"cached_tokens":4`) {
		t.Fatalf("expected response.completed with cache usage, got %s", rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].InputTokens != 11 || logs[0].OutputTokens != 3 || logs[0].CacheHitTokens != 4 || logs[0].CacheInputTokens != 11 {
		t.Fatalf("unexpected logged usage: input=%d output=%d cache_hit=%d cache_input=%d", logs[0].InputTokens, logs[0].OutputTokens, logs[0].CacheHitTokens, logs[0].CacheInputTokens)
	}
}

func TestOpenAIResponsesFallbackRequiresEndpointCompatibilitySignal(t *testing.T) {
	ra := newResponsesFallbackAttempt(&transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: strPtr("ping")},
		}},
	})

	if ra.shouldFallbackOpenAIResponsesToChat(http.StatusBadRequest, newUpstreamError(http.StatusBadRequest, []byte(`{
		"error":{"message":"Invalid schema for text.format","code":"invalid_request_error"}
	}`))) {
		t.Fatalf("native responses client errors must not fallback to chat")
	}
	if ra.shouldFallbackOpenAIResponsesToChat(http.StatusNotFound, newUpstreamError(http.StatusNotFound, []byte(`{
		"error":{"message":"model gpt-5.5 was not found","code":"model_not_found"}
	}`))) {
		t.Fatalf("native responses model-not-found errors must not fallback to chat")
	}
	if !ra.shouldFallbackOpenAIResponsesToChat(http.StatusBadRequest, newUpstreamError(http.StatusBadRequest, []byte(`{
		"error":{"message":"responses endpoint is not supported by this compatible upstream","code":"invalid_request_error"}
	}`))) {
		t.Fatalf("explicit responses endpoint unsupported errors should fallback to chat")
	}
	if !ra.shouldFallbackOpenAIResponsesToChat(http.StatusNotFound, newUpstreamError(http.StatusNotFound, []byte(`404 page not found`))) {
		t.Fatalf("plain route 404 from compatible proxies should fallback to chat")
	}
	if !ra.shouldFallbackOpenAIResponsesToChat(http.StatusServiceUnavailable, newUpstreamError(http.StatusServiceUnavailable, []byte(`{
		"error":{"message":"upstream responses proxy unavailable","code":"service_unavailable"}
	}`))) {
		t.Fatalf("compatible responses proxy 503 should fallback to chat")
	}
	if !ra.shouldFallbackOpenAIResponsesToChat(http.StatusForbidden, newUpstreamError(http.StatusForbidden, []byte(`{
		"error":{"message":"Insufficient account balance","type":"bad_response_status_code","code":"bad_response_status_code"}
	}`))) {
		t.Fatalf("compatible responses proxy 403 bad_response_status_code should fallback to chat")
	}
	if ra.shouldFallbackOpenAIResponsesToChat(http.StatusForbidden, newUpstreamError(http.StatusForbidden, []byte(`{
		"error":{"message":"Invalid API key","code":"invalid_api_key"}
	}`))) {
		t.Fatalf("auth 403 must not fallback to chat")
	}
}

func TestOpenAIResponsesFallbackRejectsNonTextInputs(t *testing.T) {
	unsupported := newUpstreamError(http.StatusBadRequest, []byte(`{
		"error":{"message":"responses endpoint is not supported by this compatible upstream"}
	}`))

	imageAttempt := newResponsesFallbackAttempt(&transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{MultipleContent: []transformerModel.MessageContentPart{{
				Type:     "image_url",
				ImageURL: &transformerModel.ImageURL{URL: "https://example.test/image.png"},
			}}},
		}},
	})
	if imageAttempt.shouldFallbackOpenAIResponsesToChat(http.StatusBadRequest, unsupported) {
		t.Fatalf("responses requests with image input must not fallback to chat")
	}

	audioAttempt := newResponsesFallbackAttempt(&transformerModel.InternalLLMRequest{
		Modalities: []string{"audio"},
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: strPtr("ping")},
		}},
	})
	if audioAttempt.shouldFallbackOpenAIResponsesToChat(http.StatusBadRequest, unsupported) {
		t.Fatalf("responses requests with audio modality must not fallback to chat")
	}

	toolAttempt := newResponsesFallbackAttempt(&transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: strPtr("draw")},
		}},
		Tools: []transformerModel.Tool{{Type: "image_generation", ImageGeneration: &transformerModel.ImageGeneration{}}},
	})
	if toolAttempt.shouldFallbackOpenAIResponsesToChat(http.StatusBadRequest, unsupported) {
		t.Fatalf("responses requests with non-function tools must not fallback to chat")
	}
}

func newResponsesFallbackAttempt(req *transformerModel.InternalLLMRequest) *relayAttempt {
	return &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}
}

func strPtr(value string) *string {
	return &value
}
