package modeltest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func setupModelTestDB(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}

func TestRunUsesAccessRouteAndDoesNotExposeBillingModelAsUpstream(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		seenModel.Store(payload.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"claude-sonnet-4.5",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "route-test-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "claude-sonnet-4.5",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var vip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}

	if _, err := op.AccessPlanUpdateRouteTargets(vip.ID, []dbmodel.AccessRouteTarget{{
		RequestModel:         "glm-5.1",
		ChannelID:            channel.ID,
		UpstreamModel:        "claude-sonnet-4.5",
		Priority:             1,
		Weight:               1,
		Enabled:              true,
		BillingModelSource:   dbmodel.AccessBillingModelSourceRequest,
		FallbackMode:         dbmodel.AccessRouteFallbackGroup,
		PromptOverrideMode:   dbmodel.PromptOverrideModeAppendSystem,
		SystemPromptOverride: "",
	}}, ctx); err != nil {
		t.Fatalf("update route targets: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:          "glm-5.1",
		AccessPlanSlug: "vip",
		Endpoint:       "openai_responses",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Total != 1 || response.Summary.Success != 1 {
		t.Fatalf("unexpected summary: %#v", response.Summary)
	}
	result := response.Results[0]
	if !result.Success {
		t.Fatalf("expected success: %#v", result)
	}
	if result.RequestModel != "glm-5.1" || result.UpstreamModel != "claude-sonnet-4.5" {
		t.Fatalf("unexpected model mapping: %#v", result)
	}
	if result.RequestEndpoint != "openai_responses" || result.RequestPath != "/v1/responses" {
		t.Fatalf("unexpected endpoint metadata: endpoint=%q path=%q", result.RequestEndpoint, result.RequestPath)
	}
	if got, _ := seenModel.Load().(string); got != "claude-sonnet-4.5" {
		t.Fatalf("upstream received model %q", got)
	}
}

func TestRunCanTestSavedChannelDirectlyWithoutGroup(t *testing.T) {
	ctx := setupModelTestDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyCodexFastMode, "true"); err != nil {
		t.Fatalf("set codex fast mode: %v", err)
	}

	var seenPath atomic.Value
	var seenStream atomic.Value
	var seenBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath.Store(r.URL.Path)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		seenBody.Store(payload)
		seenStream.Store(payload["stream"] == true)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":123,\"model\":\"gpt-5.5\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":123,\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":0},\"total_tokens\":3}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "direct-responses-channel",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: false,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "gpt-5.5",
		ChannelID: channel.ID,
		Endpoint:  "openai_responses",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if !result.Success || result.ChannelID != channel.ID || result.UpstreamPath != "/v1/responses" || result.ResponsePreview != "OK" {
		t.Fatalf("unexpected direct-channel result: %#v", result)
	}
	if got, _ := seenPath.Load().(string); got != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got)
	}
	if got, _ := seenStream.Load().(bool); !got {
		t.Fatalf("expected model test to force stream=true for responses upstream")
	}
	body, _ := seenBody.Load().(map[string]any)
	if instructions, _ := body["instructions"].(string); instructions == "" {
		t.Fatalf("expected Codex instructions in model test body, got %#v", body["instructions"])
	}
	if include, _ := body["include"].([]any); len(include) == 0 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("expected Codex include in model test body, got %#v", body["include"])
	}
	if body["service_tier"] != "priority" {
		t.Fatalf("expected Codex fast service_tier=priority in model test body, got %#v", body["service_tier"])
	}
	if tools, _ := body["tools"].([]any); len(tools) < 4 {
		t.Fatalf("expected Codex tools in model test body, got %#v", body["tools"])
	}
	if choice := body["tool_choice"]; choice != "auto" {
		t.Fatalf("expected tool_choice auto, got %#v", choice)
	}
	if input, _ := body["input"].([]any); len(input) == 0 {
		t.Fatalf("expected Codex input array in model test body, got %#v", body["input"])
	}
}

// A codex (Responses) channel test must force a streaming upstream even when the
// caller explicitly asks for stream=false — real codex always streams and the relay
// force-streams codex upstreams, so the test must too (shape lock). Guards the fix for
// the "真开真关" toggle that could otherwise send an off-shape non-stream request.
func TestModelTestForcesStreamForCodexEvenWhenClientAsksNonStream(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenStream atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		seenStream.Store(payload["stream"] == true)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":123,\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "codex-shape-lock",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	noStream := false
	if _, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "gpt-5.6-sol",
		ChannelID: channel.ID,
		Endpoint:  "openai_responses",
		Stream:    &noStream,
	}); err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if got, _ := seenStream.Load().(bool); !got {
		t.Fatal("codex channel test must force stream=true upstream even when client asks stream=false (shape lock)")
	}
}

func TestRunAppliesCodexDefaultsWhenResponsesUseChatChannel(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenUserAgent atomic.Value
	var seenBetaFeatures atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUserAgent.Store(r.Header.Get("User-Agent"))
		seenBetaFeatures.Store(r.Header.Get("X-Codex-Beta-Features"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-5.5",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "responses-through-chat-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: false,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "gpt-5.5",
		ChannelID: channel.ID,
		Endpoint:  "openai_responses",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	if got, _ := seenUserAgent.Load().(string); got != defaultCodexUserAgent {
		t.Fatalf("user-agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if got, _ := seenBetaFeatures.Load().(string); got != defaultCodexBetaFeatures {
		t.Fatalf("codex beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}
}

func TestPrepareCodexModelTestRequestUsesStableDeviceAcrossSessions(t *testing.T) {
	first := &transformermodel.InternalLLMRequest{Model: "gpt-5.5"}
	second := &transformermodel.InternalLLMRequest{Model: "gpt-5.5"}

	// Zero-value fingerprint == global default (no profile selected), which is what
	// this test asserts: one stable per-instance device id across sessions.
	prepareCodexModelTestRequest(first, outbound.OutboundTypeOpenAIResponse, resolvedFingerprint{})
	prepareCodexModelTestRequest(second, outbound.OutboundTypeOpenAIResponse, resolvedFingerprint{})

	var firstMetadata map[string]string
	var secondMetadata map[string]string
	if err := json.Unmarshal(first.ClientMetadata, &firstMetadata); err != nil {
		t.Fatalf("decode first client metadata: %v", err)
	}
	if err := json.Unmarshal(second.ClientMetadata, &secondMetadata); err != nil {
		t.Fatalf("decode second client metadata: %v", err)
	}
	if firstMetadata["x-codex-installation-id"] == "" {
		t.Fatalf("expected stable installation id in metadata: %#v", firstMetadata)
	}
	if firstMetadata["x-codex-installation-id"] != secondMetadata["x-codex-installation-id"] {
		t.Fatalf("installation id should be stable for one device identity: first=%q second=%q", firstMetadata["x-codex-installation-id"], secondMetadata["x-codex-installation-id"])
	}
	if first.PromptCacheKey == nil || second.PromptCacheKey == nil || *first.PromptCacheKey == *second.PromptCacheKey {
		t.Fatalf("expected independent sessions under the stable device identity: first=%#v second=%#v", first.PromptCacheKey, second.PromptCacheKey)
	}
	if !strings.HasPrefix(firstMetadata["x-codex-window-id"], *first.PromptCacheKey+":") {
		t.Fatalf("first window id should be scoped to first session: %#v", firstMetadata)
	}
	if !strings.HasPrefix(secondMetadata["x-codex-window-id"], *second.PromptCacheKey+":") {
		t.Fatalf("second window id should be scoped to second session: %#v", secondMetadata)
	}
}

func TestRunHonorsExplicitStreamMode(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenStream atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		seenStream.Store(payload["stream"] == true)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "stream-test-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: false,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}

	stream := true
	response, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model:    "gpt-test",
		Endpoint: "openai_chat",
		Stream:   &stream,
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 || response.Results[0].ResponsePreview != "OK" {
		t.Fatalf("unexpected stream model-test response: %#v", response)
	}
	if got, _ := seenStream.Load().(bool); !got {
		t.Fatalf("expected upstream request to include stream=true")
	}
}

func TestRunOpenAIChatStreamsByDefault(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenStream atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		seenStream.Store(payload["stream"] == true)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	// No Stream field set: the openai_chat probe must default to streaming so a thinking
	// model (glm/deepseek) that emits a long reasoning preamble streams its first content
	// instead of buffering the whole blob server-side and blowing the connectivity timeout.
	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:    "default-stream-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: false,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}, dbmodel.ModelTestRequest{
		Model:    "glm-5.2",
		Endpoint: "openai_chat",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 || response.Results[0].ResponsePreview != "OK" {
		t.Fatalf("unexpected default-stream response: %#v", response)
	}
	if got, _ := seenStream.Load().(bool); !got {
		t.Fatalf("expected openai_chat model test to default to stream=true")
	}
}

func TestRunChannelUsesUnsavedConfig(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	t.Cleanup(upstream.Close)

	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:    "draft-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: false,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "draft-key"}},
	}, dbmodel.ModelTestRequest{
		Model: "gpt-4o",
	})
	if err != nil {
		t.Fatalf("run channel test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if !result.Success || result.ChannelName != "draft-channel" || result.ResponsePreview != "OK" {
		t.Fatalf("unexpected draft channel result: %#v", result)
	}
}

func TestRunChannelRecordsHTTPProxyConnectivity(t *testing.T) {
	ctx := setupModelTestDB(t)

	var headHits int32
	var postHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			atomic.AddInt32(&headHits, 1)
			if got := r.URL.String(); got != "http://upstream.test/" {
				t.Fatalf("proxy HEAD target = %q, want http://upstream.test/", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			atomic.AddInt32(&postHits, 1)
			if got := r.URL.String(); got != "http://upstream.test/v1/chat/completions" {
				t.Fatalf("proxy POST target = %q, want chat completions target", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-proxy",
				"object":"chat.completion",
				"created":123,
				"model":"gpt-proxy",
				"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
			}`))
		default:
			t.Fatalf("unexpected proxy method %s", r.Method)
		}
	}))
	t.Cleanup(proxy.Close)

	channelProxy := proxy.URL
	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:    "proxy-test-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: "http://upstream.test",
		}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
		Proxy:        true,
		ChannelProxy: &channelProxy,
	}, dbmodel.ModelTestRequest{
		Model:    "gpt-proxy",
		Endpoint: "openai_chat",
	})
	if err != nil {
		t.Fatalf("run channel: %v", err)
	}
	if atomic.LoadInt32(&headHits) != 1 || atomic.LoadInt32(&postHits) != 1 {
		t.Fatalf("expected one proxy HEAD and POST, got head=%d post=%d", headHits, postHits)
	}
	result := response.Results[0]
	if !result.Success || result.ResponsePreview != "OK" {
		t.Fatalf("unexpected proxy result: %#v", result)
	}
	if !result.ProxyUsed || result.ProxySource != "channel" || result.ProxyScheme != "http" || result.ProxyStatus != http.StatusNoContent {
		t.Fatalf("unexpected proxy metadata: %#v", result)
	}
	if !strings.Contains(result.ProxyTarget, "channel http proxy") || !strings.Contains(result.ProxyTarget, "http://upstream.test/") {
		t.Fatalf("unexpected proxy target: %q", result.ProxyTarget)
	}
	if len(result.Attempts) != 1 || !result.Attempts[0].ProxyUsed || !strings.Contains(result.Attempts[0].Msg, "proxy connectivity ok") {
		t.Fatalf("expected proxy metadata on success attempt, got %#v", result.Attempts)
	}
}

func TestRunChannelReportsProxyConnectivityFailure(t *testing.T) {
	ctx := setupModelTestDB(t)

	channelProxy := "ftp://proxy.invalid:21"
	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:    "bad-proxy-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: "http://upstream.test",
		}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
		Proxy:        true,
		ChannelProxy: &channelProxy,
	}, dbmodel.ModelTestRequest{
		Model:    "gpt-proxy",
		Endpoint: "openai_chat",
	})
	if err != nil {
		t.Fatalf("run channel: %v", err)
	}
	result := response.Results[0]
	if result.Success || result.ErrorCode != "proxy_connectivity_failed" {
		t.Fatalf("expected proxy failure, got %#v", result)
	}
	if !result.ProxyUsed || result.ProxySource != "channel" || result.ProxyScheme != "ftp" {
		t.Fatalf("unexpected proxy metadata: %#v", result)
	}
	if len(result.Attempts) != 1 || !strings.Contains(result.Attempts[0].Msg, "proxy connectivity failed") {
		t.Fatalf("expected proxy failure attempt, got %#v", result.Attempts)
	}
}

func TestRunUsesAPIKeyAccessPlanAndWritesAuditLog(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"selected-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "key-plan-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "selected-upstream",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var svip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "svip" {
			svip = plan
			break
		}
	}
	if svip.ID == 0 {
		t.Fatalf("svip plan not found")
	}

	if _, err := op.AccessPlanUpdateRouteTargets(svip.ID, []dbmodel.AccessRouteTarget{{
		RequestModel:  "selected-request",
		ChannelID:     channel.ID,
		UpstreamModel: "selected-upstream",
		Priority:      1,
		Weight:        1,
		Enabled:       true,
		FallbackMode:  dbmodel.AccessRouteFallbackGroup,
	}}, ctx); err != nil {
		t.Fatalf("update route targets: %v", err)
	}

	admin, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "model-test-admin",
		Password: "secret",
		Role:     dbmodel.UserRoleAdmin,
		Status:   dbmodel.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	apiKey := dbmodel.APIKey{
		UserID:  admin.ID,
		Name:    "selected test key",
		APIKey:  "sk-model-test",
		Enabled: true,
	}
	if err := op.APIKeyCreate(&apiKey, ctx); err != nil {
		t.Fatalf("create API key: %v", err)
	}
	if err := op.UserAccessPlanSet(admin.ID, []int{svip.ID}, svip.ID, ctx); err != nil {
		t.Fatalf("set user access plan: %v", err)
	}
	if err := op.APIKeyAccessPlanSet(apiKey.ID, []int{svip.ID}, svip.ID, ctx); err != nil {
		t.Fatalf("set API key access plan: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "selected-request",
		APIKeyID: apiKey.ID,
		AuditLog: true,
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if !result.RouteUsed || result.AccessPlanSlug != "svip" || result.UpstreamModel != "selected-upstream" {
		t.Fatalf("expected API key scoped route, got %#v", result)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, &dbmodel.RelayLogScope{
		APIKeyID: apiKey.ID,
		Endpoint: "model_test_chat",
	})
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected one audit log, got %d", len(logs))
	}
	audit := logs[0]
	if audit.UserID != admin.ID || audit.APIKeyID != apiKey.ID || audit.RequestAPIKeyName != apiKey.Name {
		t.Fatalf("unexpected audit identity: %#v", audit)
	}
	if audit.RequestEndpoint != "model_test_chat" || audit.AccessPlanSlug != "svip" || audit.Cost != 0 {
		t.Fatalf("unexpected audit metadata: %#v", audit)
	}
	if audit.RequestModelName != "selected-request" || audit.ActualModelName != "selected-upstream" || audit.TotalAttempts != 1 {
		t.Fatalf("unexpected audit route metadata: %#v", audit)
	}
}

func TestRunRespectsAPIKeySupportedModels(t *testing.T) {
	ctx := setupModelTestDB(t)
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	upstreamHits := int32(0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"blocked-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Model:    "blocked-model",
		Priority: 1,
		Name:     "supported-model-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "supported-model-user",
		Password: "secret",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apiKey := dbmodel.APIKey{
		UserID:          user.ID,
		Name:            "restricted key",
		APIKey:          "sk-restricted",
		Enabled:         true,
		SupportedModels: "allowed-model, another-model",
	}
	if err := op.APIKeyCreate(&apiKey, ctx); err != nil {
		t.Fatalf("create API key: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "blocked-model",
		APIKeyID: apiKey.ID,
		AuditLog: true,
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Total != 1 || response.Summary.Success != 0 || response.Summary.Failed != 1 {
		t.Fatalf("unexpected summary: %#v", response.Summary)
	}
	result := response.Results[0]
	if result.Success || result.ErrorCode != "model_not_supported" || result.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected supported-model failure, got %#v", result)
	}
	if hits := atomic.LoadInt32(&upstreamHits); hits != 0 {
		t.Fatalf("unsupported model should not be sent upstream, hits=%d", hits)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ErrorCode != "model_not_supported" || logs[0].TotalAttempts != 0 {
		t.Fatalf("expected audit log for local model-test rejection, got %#v", logs)
	}
}

func TestRunRejectsUnsupportedEndpoint(t *testing.T) {
	ctx := setupModelTestDB(t)

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "gpt-4o",
		Endpoint: "bad-endpoint",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Total != 1 || response.Summary.Failed != 1 {
		t.Fatalf("unexpected summary: %#v", response.Summary)
	}
	result := response.Results[0]
	if result.Success || result.Error == "" || result.RequestEndpoint != "bad-endpoint" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestResolveTimeoutSecondsUsesEndpointDefaults(t *testing.T) {
	openAIEndpoint, err := normalizeEndpoint("openai_chat")
	if err != nil {
		t.Fatalf("normalize openai endpoint: %v", err)
	}
	anthropicEndpoint, err := normalizeEndpoint("anthropic_messages")
	if err != nil {
		t.Fatalf("normalize anthropic endpoint: %v", err)
	}

	if got := resolveTimeoutSeconds(dbmodel.ModelTestRequest{}, openAIEndpoint); got != defaultTimeoutSeconds {
		t.Fatalf("openai default timeout = %d, want %d", got, defaultTimeoutSeconds)
	}
	if got := resolveTimeoutSeconds(dbmodel.ModelTestRequest{}, anthropicEndpoint); got != defaultAnthropicMessagesTimeoutSeconds {
		t.Fatalf("anthropic default timeout = %d, want %d", got, defaultAnthropicMessagesTimeoutSeconds)
	}
	if got := resolveTimeoutSeconds(dbmodel.ModelTestRequest{TimeoutSeconds: 90}, anthropicEndpoint); got != 90 {
		t.Fatalf("custom timeout = %d, want 90", got)
	}
	if got := resolveTimeoutSeconds(dbmodel.ModelTestRequest{TimeoutSeconds: maxTimeoutSeconds + 60}, anthropicEndpoint); got != maxTimeoutSeconds {
		t.Fatalf("clamped timeout = %d, want %d", got, maxTimeoutSeconds)
	}
}

func TestRunAnthropicMessagesUsesStreamConnectivityTest(t *testing.T) {
	ctx := setupModelTestDB(t)
	// This test asserts the Claude CLI-like 1M body shape, which is now opt-in.
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeCLIAutoCompact, "true"); err != nil {
		t.Fatalf("set auto compact: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeCLIReasoningEffort, "high"); err != nil {
		t.Fatalf("set reasoning effort: %v", err)
	}

	var sawStream bool
	var sawBeta string
	var sawClientRequestID string
	var sawUserAgent string
	var sawBetaQuery string
	var sawSystem string
	var sawMetadata string
	var sawContextManagement string
	var sawThinking string
	var sawMaxTokens float64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		sawStream, _ = payload["stream"].(bool)
		sawBeta = r.Header.Get("Anthropic-Beta")
		sawClientRequestID = r.Header.Get("X-Client-Request-Id")
		sawUserAgent = r.Header.Get("User-Agent")
		sawBetaQuery = r.URL.Query().Get("beta")
		sawMaxTokens, _ = payload["max_tokens"].(float64)
		if systemData, err := json.Marshal(payload["system"]); err == nil {
			sawSystem = string(systemData)
		}
		if metadataData, err := json.Marshal(payload["metadata"]); err == nil {
			sawMetadata = string(metadataData)
		}
		if contextData, err := json.Marshal(payload["context_management"]); err == nil {
			sawContextManagement = string(contextData)
		}
		if thinkingData, err := json.Marshal(payload["thinking"]); err == nil {
			sawThinking = string(thinkingData)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_model_test","type":"message","role":"assistant","model":"claude-opus-4-7[1m]","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

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
		Model:    "claude-opus-4-7[1m]",
		Priority: 1,
		Name:     "Claude-CPA",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "claude-opus-4-7[1m]",
		Endpoint: "anthropic_messages",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if !sawStream {
		t.Fatalf("expected anthropic model test to use stream upstream")
	}
	if !strings.Contains(sawBeta, defaultClaudeOneMillionBeta) {
		t.Fatalf("expected [1m] beta header to contain %q, got %q", defaultClaudeOneMillionBeta, sawBeta)
	}
	for _, want := range []string{"claude-code-20250219", "context-management-2025-06-27", "effort-2025-11-24"} {
		if !strings.Contains(sawBeta, want) {
			t.Fatalf("expected Claude CLI beta %q, got %q", want, sawBeta)
		}
	}
	if sawBetaQuery != "true" || sawUserAgent != defaultClaudeUserAgent {
		t.Fatalf("expected Claude CLI-like query/ua, beta=%q ua=%q", sawBetaQuery, sawUserAgent)
	}
	if sawMaxTokens != 64000 || !strings.Contains(sawThinking, `"adaptive"`) || !strings.Contains(sawContextManagement, "clear_thinking_20251015") {
		t.Fatalf("expected Claude CLI-like 1M body, max=%v thinking=%s context=%s", sawMaxTokens, sawThinking, sawContextManagement)
	}
	// Genuine claude-cli (2.1.168/2.1.178, captured on the wire) does NOT send
	// X-Client-Request-Id, and the relay forward path strips it; the channel/model
	// test must match — sending it was a non-CLI tell and a test-vs-relay drift.
	if sawClientRequestID != "" {
		t.Fatalf("test must NOT send X-Client-Request-Id (real claude-cli omits it; relay strips it), got %q", sawClientRequestID)
	}
	if !strings.Contains(sawSystem, "Claude Agent SDK") || !strings.Contains(sawMetadata, "session_id") {
		t.Fatalf("expected Claude 1M model-test shape to include agent system and metadata, system=%s metadata=%s", sawSystem, sawMetadata)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if !result.Success || result.ResponsePreview != "OK" || result.InputTokens != 3 || result.OutputTokens != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

// TestRunChannelFingerprintProfileSelectsDistinctIdentity proves the multi-profile
// feature: a channel whose ChannelCloak.ProfileID points at a fingerprint profile
// emits THAT profile's device identity (UA / X-Stainless-OS) on the wire instead of
// the global default, so two channels behind different egress IPs present two distinct
// real devices. (ProfileID=0 keeping the byte-for-byte global default is covered by
// every other test here, which all run with no profile selected.)
func TestRunChannelFingerprintProfileSelectsDistinctIdentity(t *testing.T) {
	ctx := setupModelTestDB(t)

	stabilize := true
	profile := dbmodel.FingerprintProfile{
		Name:            "linux-real-test",
		Seed:            "profile2-distinct-seed-deadbeef0000000000000000000000000000",
		ClaudeUserAgent: "claude-cli/2.1.186 (external, sdk-cli)",
		ClaudeOS:        "Linux",
		ClaudeArch:      "x64",
		ClaudeStabilize: &stabilize,
	}
	if err := op.FingerprintProfileCreate(&profile, ctx); err != nil {
		t.Fatalf("create fingerprint profile: %v", err)
	}

	var sawUserAgent, sawStainlessOS string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUserAgent = r.Header.Get("User-Agent")
		sawStainlessOS = r.Header.Get("X-Stainless-OS")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Model:    "claude-opus-4-8",
		Priority: 1,
		Name:     "Claude-Profile2",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		Cloak:    dbmodel.ChannelCloak{ProfileID: profile.ID},
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	if _, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "claude-opus-4-8",
		Endpoint: "anthropic_messages",
	}); err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if sawUserAgent != "claude-cli/2.1.186 (external, sdk-cli)" {
		t.Fatalf("expected profile2 UA on the wire, got %q", sawUserAgent)
	}
	if sawStainlessOS != "Linux" {
		t.Fatalf("expected profile2 X-Stainless-OS=Linux, got %q", sawStainlessOS)
	}
	if sawUserAgent == defaultClaudeUserAgent {
		t.Fatalf("profile2 UA must differ from the global default %q", defaultClaudeUserAgent)
	}
}

// TestRunClaudeChannelKeepsCLIBodyShapeOnNonAnthropicEndpoint pins the fix for the
// "渠道测试报错非Claude" bug: an Anthropic channel's outbound is ALWAYS a /v1/messages
// claude request, so a connectivity test must send the genuine claude-cli body shape
// (64000 max_tokens, streamed, explicit thinking, body metadata.user_id) even when the
// inbound test endpoint is the model-test page default openai_responses — NOT a degraded
// max_tokens=8 / no-thinking body that strict Claude-Code-gating upstreams
// reject as non-Claude while the relay forward path (always cli-shaped) passes. Uses a
// plain (non-1M) claude model, the worst case, where the body would otherwise degrade.
func TestRunClaudeChannelKeepsCLIBodyShapeOnNonAnthropicEndpoint(t *testing.T) {
	ctx := setupModelTestDB(t)

	var sawStream bool
	var sawMaxTokens float64
	var sawThinking string
	var sawMetadata string
	var sawSystem string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("expected Anthropic outbound /v1/messages, got %q", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		sawStream, _ = payload["stream"].(bool)
		sawMaxTokens, _ = payload["max_tokens"].(float64)
		if b, err := json.Marshal(payload["thinking"]); err == nil {
			sawThinking = string(b)
		}
		if b, err := json.Marshal(payload["metadata"]); err == nil {
			sawMetadata = string(b)
		}
		if b, err := json.Marshal(payload["system"]); err == nil {
			sawSystem = string(b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_model_test","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Model:    "claude-haiku-4-5",
		Priority: 1,
		Name:     "Claude-Plain",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Inbound endpoint = openai_responses (the model-test page default), NOT anthropic_messages.
	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "claude-haiku-4-5",
		Endpoint: "openai_responses",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if !sawStream {
		t.Fatalf("claude channel test must stream regardless of inbound endpoint")
	}
	if sawMaxTokens != 64000 {
		t.Fatalf("expected cli max_tokens=64000 (not the degraded 8) on openai_responses endpoint, got %v", sawMaxTokens)
	}
	if !strings.Contains(sawThinking, "disabled") {
		t.Fatalf("expected explicit thinking object on plain claude test, got %q", sawThinking)
	}
	if !strings.Contains(sawMetadata, "session_id") || !strings.Contains(sawMetadata, "device_id") {
		t.Fatalf("expected body metadata.user_id (device_id+session_id) like the relay, got %q", sawMetadata)
	}
	if !strings.Contains(sawSystem, "Claude") {
		t.Fatalf("expected claude identity system block (endpoint-independent), got %q", sawSystem)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestRunAnthropicMessagesShortcutUsesCleanCapabilityModelGroup(t *testing.T) {
	ctx := setupModelTestDB(t)

	var sawModel string
	var sawBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		sawModel, _ = payload["model"].(string)
		sawBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_model_test_alias","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

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
		Model:    "claude-opus-4-8[1m]",
		Priority: 1,
		Name:     "Claude-Full-1M",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:    "opus[1m]",
		Endpoint: "anthropic_messages",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if result.GroupName != "claude-opus-4-8" || result.UpstreamModel != "claude-opus-4-8" {
		t.Fatalf("expected shortcut to use clean 1m-capable group, got %#v", result)
	}
	if sawModel != "claude-opus-4-8" {
		t.Fatalf("upstream model = %q, want claude-opus-4-8", sawModel)
	}
	if !strings.Contains(sawBeta, defaultClaudeOneMillionBeta) {
		t.Fatalf("expected [1m] beta header to contain %q, got %q", defaultClaudeOneMillionBeta, sawBeta)
	}
}

func TestRunAnthropicMessagesStreamStopsAfterMessageStopWithoutEOF(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_model_test_stop","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

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
		Model:    "claude-opus-4-8[1m]",
		Priority: 1,
		Name:     "Claude-CPA-Terminal",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "anthropic-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	started := time.Now()
	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:          "claude-opus-4-8[1m]",
		Endpoint:       "anthropic_messages",
		TimeoutSeconds: 3,
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("model test should stop after message_stop without waiting for post-stop pings, elapsed=%s response=%#v", elapsed, response)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	if !result.Success || result.ResponsePreview != "OK" || result.InputTokens != 3 || result.OutputTokens != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestUpstreamErrorSummarySanitizesHTML(t *testing.T) {
	_, message := upstreamErrorSummary([]byte(`<html>
<head><title>504 Gateway Time-out</title></head>
<body><center><h1>504 Gateway Time-out</h1></center><hr><center>openresty</center></body>
</html>`))
	if message != "upstream returned HTML error page: 504 Gateway Time-out (openresty)" {
		t.Fatalf("unexpected html summary: %q", message)
	}
}

func TestRunRedactsSecretsFromUpstreamErrorSummary(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"Authorization: Bearer sk-leaked-provider and x-api-key: sk-second-leaked"}}`))
	}))
	t.Cleanup(upstream.Close)

	stream := false
	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:    "redacted-error-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-provider-request"}},
	}, dbmodel.ModelTestRequest{
		Model:    "gpt-error",
		Endpoint: "openai_chat",
		Stream:   &stream,
	})
	if err != nil {
		t.Fatalf("run channel test: %v", err)
	}
	if response.Summary.Success != 0 || len(response.Results) != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	result := response.Results[0]
	combined := result.Error
	if len(result.Attempts) > 0 {
		combined += " " + result.Attempts[0].Msg
	}
	if strings.Contains(combined, "sk-") || strings.Contains(combined, "sk_leaked") || strings.Contains(combined, "Bearer sk") {
		t.Fatalf("model-test error leaked secret material: %#v", result)
	}
	if !strings.Contains(result.Error, "[redacted]") || result.ErrorCode != "invalid_api_key" {
		t.Fatalf("expected redacted provider error with code, got %#v", result)
	}
}

func TestRunBatchHonorsConcurrency(t *testing.T) {
	ctx := setupModelTestDB(t)

	var current int64
	var maxSeen int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := atomic.AddInt64(&current, 1)
		for {
			max := atomic.LoadInt64(&maxSeen)
			if now <= max || atomic.CompareAndSwapInt64(&maxSeen, max, now) {
				break
			}
		}
		// Hold each probe in-flight long enough that the fan-out actually overlaps,
		// mirroring a real (slow) upstream rather than an instant one.
		time.Sleep(1200 * time.Millisecond)
		atomic.AddInt64(&current, -1)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":123,
			"model":"ok",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Model:    "model-a,model-b,model-c",
		Priority: 1,
		Name:     "batch-test-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	models := []string{"model-a", "model-b", "model-c"}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Models:      models,
		Concurrency: 3,
	})
	if err != nil {
		t.Fatalf("run model tests: %v", err)
	}
	if response.Summary.Success != len(models) {
		t.Fatalf("unexpected response: %#v", response)
	}
	if atomic.LoadInt64(&maxSeen) < 2 {
		t.Fatalf("expected concurrent upstream calls, max in-flight was %d", maxSeen)
	}
}
