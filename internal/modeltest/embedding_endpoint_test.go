package modeltest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// B1: 端点解析。嵌入端点是 channel test 的第 5 个端点，归一化后必须带上
// 嵌入的 inbound / apiFormat / path，否则出站适配器（EmbeddingOutbound）
// 会因为 IsEmbeddingRequest() 为假直接拒收。
func TestNormalizeEndpointSupportsEmbeddings(t *testing.T) {
	for _, raw := range []string{"openai_embeddings", "embeddings", "OpenAI-Embeddings", " openai/embeddings "} {
		endpoint, err := normalizeEndpoint(raw)
		if err != nil {
			t.Fatalf("normalizeEndpoint(%q): %v", raw, err)
		}
		if endpoint.name != embeddingTestEndpointName {
			t.Fatalf("normalizeEndpoint(%q).name = %q, want %q", raw, endpoint.name, embeddingTestEndpointName)
		}
		if endpoint.inbound != inbound.InboundTypeOpenAIEmbedding {
			t.Fatalf("normalizeEndpoint(%q).inbound = %d, want the OpenAI embedding inbound", raw, endpoint.inbound)
		}
		if endpoint.apiFormat != transformermodel.APIFormatOpenAIEmbedding {
			t.Fatalf("normalizeEndpoint(%q).apiFormat = %q, want %q", raw, endpoint.apiFormat, transformermodel.APIFormatOpenAIEmbedding)
		}
		if got := endpoint.path("any-model"); got != "/v1/embeddings" {
			t.Fatalf("normalizeEndpoint(%q).path = %q, want /v1/embeddings", raw, got)
		}
	}
	// 未知端点仍要报错——放宽不能变成"什么都收"。
	if _, err := normalizeEndpoint("openai_embeddingz"); err == nil {
		t.Fatalf("unknown endpoint must still be rejected")
	}
}

// B1: 审计端点映射：探针日志要落在 model_test_embeddings 而不是 model_test_<unknown>，
// 否则日志页的"连通性测试（向量）"筛选和 humanize 标签都对不上。
func TestModelTestAuditEndpointMapsEmbeddings(t *testing.T) {
	if got := modelTestAuditEndpoint(embeddingTestEndpointName); got != "model_test_embeddings" {
		t.Fatalf("modelTestAuditEndpoint(%q) = %q, want model_test_embeddings", embeddingTestEndpointName, got)
	}
	if got := modelTestAuditEndpoint(""); got != "model_test" {
		t.Fatalf("modelTestAuditEndpoint(\"\") = %q, want model_test", got)
	}
}

// B1: 探针准入门的放宽只对嵌入渠道生效；没有出站能力的类型仍被拒。
func TestNewProbeSenderAcceptsEmbeddingChannel(t *testing.T) {
	embeddingChannel := dbmodel.Channel{
		Name:     "embedding-channel",
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		BaseUrls: []dbmodel.BaseUrl{{URL: "http://embedding.example.test"}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "embed-key"}},
	}
	sender, err := NewProbeSender(embeddingChannel, "test-embed-model", embeddingTestEndpointName)
	if err != nil {
		t.Fatalf("embedding channel must be accepted by the probe gate: %v", err)
	}
	if sender.endpoint != embeddingTestEndpointName {
		t.Fatalf("sender endpoint = %q, want %q", sender.endpoint, embeddingTestEndpointName)
	}
	if sender.adapter == nil {
		t.Fatalf("embedding channel must resolve an outbound adapter")
	}

	// 未定义的类型（既非 chat 也非 embedding）仍必须被拒。
	unknown := embeddingChannel
	unknown.Type = outbound.OutboundType(99)
	if _, err := NewProbeSender(unknown, "m", ""); err == nil {
		t.Fatalf("a channel type without chat/embedding support must still be rejected")
	}
}

// B2: 嵌入探针的**出站请求体**必须只有 model+input：没有 messages / stream /
// max_tokens / tools，且 URL 落在 /v1/embeddings。这是"嵌入探针绝不带 CLI 形状"
// 的第一道证据（第二道在 B3 的 header 断言）。
func TestEmbeddingProbeBuildsEmbeddingRequestBody(t *testing.T) {
	const prompt = "请只回答算式结果，不要解释：1 + 1 = ?"
	runner := &modelRunner{request: dbmodel.ModelTestRequest{
		Endpoint: embeddingTestEndpointName,
		Prompt:   prompt,
	}}

	internalRequest := runner.internalRequest("upstream-embed-model")
	if !internalRequest.IsEmbeddingRequest() {
		t.Fatalf("embedding endpoint must build an embedding request")
	}
	if internalRequest.Stream != nil {
		t.Fatalf("embedding probe must not set Stream (the outbound cannot stream embeddings): %v", *internalRequest.Stream)
	}
	if internalRequest.MaxTokens != nil {
		t.Fatalf("embedding probe must not set MaxTokens: %d", *internalRequest.MaxTokens)
	}
	if len(internalRequest.Messages) != 0 {
		t.Fatalf("embedding probe must not carry chat messages: %#v", internalRequest.Messages)
	}
	if internalRequest.RawAPIFormat != transformermodel.APIFormatOpenAIEmbedding {
		t.Fatalf("RawAPIFormat = %q, want %q", internalRequest.RawAPIFormat, transformermodel.APIFormatOpenAIEmbedding)
	}
	if internalRequest.EmbeddingInput == nil || internalRequest.EmbeddingInput.Single == nil {
		t.Fatalf("embedding probe must carry the probe question as a single-string input")
	}
	if got := *internalRequest.EmbeddingInput.Single; got != prompt {
		t.Fatalf("embedding input = %q, want the probe prompt %q", got, prompt)
	}

	adapter := outbound.Get(outbound.OutboundTypeOpenAIEmbedding)
	if adapter == nil {
		t.Fatalf("no outbound adapter registered for the embedding channel type")
	}
	outboundRequest, err := adapter.TransformRequest(context.Background(), internalRequest, "http://embedding.example.test", "sk-test-key")
	if err != nil {
		t.Fatalf("build embedding outbound request: %v", err)
	}
	if got := outboundRequest.URL.Path; got != "/v1/embeddings" {
		t.Fatalf("outbound path = %q, want /v1/embeddings", got)
	}

	body, err := io.ReadAll(outboundRequest.Body)
	if err != nil {
		t.Fatalf("read outbound body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal outbound body: %v", err)
	}
	if payload["model"] != "upstream-embed-model" {
		t.Fatalf("outbound model = %v, want upstream-embed-model", payload["model"])
	}
	if payload["input"] != prompt {
		t.Fatalf("outbound input = %v, want the probe prompt", payload["input"])
	}
	for _, forbidden := range []string{"messages", "stream", "max_tokens", "tools", "system", "thinking", "metadata"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("embedding outbound body must not carry %q, got %s", forbidden, string(body))
		}
	}
}

// B3: 整条渠道测试链路（RunChannel → testChannelKey → 出站）在嵌入渠道上必须成功，
// 且上游看到的**没有**任何 CLI 身份：没有 codex/claude UA、没有 codex Lite header、
// 没有 Anthropic-Beta / Originator / Stainless 头。嵌入属非 CLI 流量，指纹路径一字不碰。
func TestRunChannelEmbeddingProbeSucceedsWithoutCLIShape(t *testing.T) {
	ctx := setupModelTestDB(t)

	var sawPath, sawUA, sawInput string
	var sawHasStream, sawHasMessages bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawUA = r.Header.Get("User-Agent")
		for _, header := range []string{
			"X-Codex-Beta-Features",
			"X-Openai-Internal-Codex-Responses-Lite",
			"Originator",
			"Anthropic-Beta",
			"X-Stainless-OS",
		} {
			if value := r.Header.Get(header); value != "" {
				t.Errorf("embedding probe must not carry CLI identity header %s=%q", header, value)
			}
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if input, ok := payload["input"].(string); ok {
			sawInput = input
		}
		_, sawHasStream = payload["stream"]
		_, sawHasMessages = payload["messages"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"emb-test",
			"object":"list",
			"created":123,
			"model":"test-embed-model",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],
			"usage":{"prompt_tokens":5,"total_tokens":5}
		}`))
	}))
	t.Cleanup(upstream.Close)

	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:     "embedding-draft-channel",
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "embed-key"}},
	}, dbmodel.ModelTestRequest{
		Model:    "test-embed-model",
		Endpoint: embeddingTestEndpointName,
	})
	if err != nil {
		t.Fatalf("run channel test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("embedding channel test must succeed, got %#v", response)
	}
	result := response.Results[0]
	if !result.Success {
		t.Fatalf("unexpected embedding channel result: %#v", result)
	}
	if result.RequestEndpoint != embeddingTestEndpointName {
		t.Fatalf("request_endpoint = %q, want %q", result.RequestEndpoint, embeddingTestEndpointName)
	}
	if result.RequestPath != "/v1/embeddings" || result.UpstreamPath != "/v1/embeddings" {
		t.Fatalf("request path = %q / upstream path = %q, want /v1/embeddings on both", result.RequestPath, result.UpstreamPath)
	}
	if sawPath != "/v1/embeddings" {
		t.Fatalf("upstream saw path %q, want /v1/embeddings", sawPath)
	}
	if result.ResponsePreview != "1 embedding(s) x 3 dims" {
		t.Fatalf("response preview = %q, want the vector summary", result.ResponsePreview)
	}
	if result.InputTokens != 5 {
		t.Fatalf("input tokens = %d, want 5 (upstream usage)", result.InputTokens)
	}
	if sawInput == "" {
		t.Fatalf("upstream did not receive a non-empty input")
	}
	if sawHasStream {
		t.Fatalf("embedding probe must not send a stream field upstream")
	}
	if sawHasMessages {
		t.Fatalf("embedding probe must not send messages upstream")
	}
	if strings.TrimSpace(sawUA) == "" {
		t.Fatalf("embedding probe leaked a bare/empty User-Agent")
	}
	lowerUA := strings.ToLower(sawUA)
	if lowerUA == strings.ToLower(dbmodel.DefaultCodexHeaderUserAgent) || lowerUA == strings.ToLower(dbmodel.DefaultClaudeHeaderUserAgent) {
		t.Fatalf("embedding probe must not present a CLI User-Agent, got %q", sawUA)
	}
	if strings.Contains(lowerUA, "codex") || strings.Contains(lowerUA, "claude") {
		t.Fatalf("embedding probe must not present a CLI User-Agent, got %q", sawUA)
	}
}
