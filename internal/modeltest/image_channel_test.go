package modeltest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// A1：端点解析。图片端点是 channel test 的第 6 个端点，归一化后必须带上图片的
// apiFormat 与 /v1/images/generations 路径。
func TestNormalizeEndpointSupportsImages(t *testing.T) {
	for _, raw := range []string{"openai_images_generations", "images", "OpenAI-Images-Generations", " openai/images/generations "} {
		endpoint, err := normalizeEndpoint(raw)
		if err != nil {
			t.Fatalf("normalizeEndpoint(%q): %v", raw, err)
		}
		if endpoint.name != imageGenerationTestEndpointName {
			t.Fatalf("normalizeEndpoint(%q).name = %q, want %q", raw, endpoint.name, imageGenerationTestEndpointName)
		}
		if endpoint.apiFormat != transformermodel.APIFormatOpenAIImageGeneration {
			t.Fatalf("normalizeEndpoint(%q).apiFormat = %q, want %q", raw, endpoint.apiFormat, transformermodel.APIFormatOpenAIImageGeneration)
		}
		if got := endpoint.path("any-model"); got != "/v1/images/generations" {
			t.Fatalf("normalizeEndpoint(%q).path = %q, want /v1/images/generations", raw, got)
		}
		if !isImageTestEndpoint(raw) {
			t.Fatalf("isImageTestEndpoint(%q) = false, want true", raw)
		}
	}
	if isImageTestEndpoint("openai_chat") || isImageTestEndpoint(embeddingTestEndpointName) {
		t.Fatalf("chat / 嵌入端点不许被判成图片端点")
	}
	if _, err := normalizeEndpoint("openai_images_generationz"); err == nil {
		t.Fatalf("unknown endpoint must still be rejected")
	}
}

// A1：审计端点映射。探针日志要落在 model_test_images，否则日志页筛选对不上。
func TestModelTestAuditEndpointMapsImages(t *testing.T) {
	if got := modelTestAuditEndpoint(imageGenerationTestEndpointName); got != "model_test_images" {
		t.Fatalf("modelTestAuditEndpoint(%q) = %q, want model_test_images", imageGenerationTestEndpointName, got)
	}
}

// recordedUpstreamRequest 记录上游真正收到了什么。
type recordedUpstreamRequest struct {
	method        string
	path          string
	body          map[string]any
	authorization string
	userAgent     string
	contentType   string
}

// imageTestUpstream 起一个假的图片上游，返回它自己收到的请求记录。
func imageTestUpstream(t *testing.T) (*httptest.Server, func() []recordedUpstreamRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		recorded []recordedUpstreamRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		recorded = append(recorded, recordedUpstreamRequest{
			method:        r.Method,
			path:          r.URL.Path,
			body:          body,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":1730000000,
			"data":[{"url":"https://image.example.test/one.png"}],
			"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}
		}`))
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedUpstreamRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedUpstreamRequest(nil), recorded...)
	}
}

// A3/A5 的主证据：图片渠道测试必须**走真链路** —— 上游真的收到一条
// POST /v1/images/generations，body 里有 prompt / n / size / model。
//
// 同时证明"钉住渠道"：库里另有一个同模型、可被分组选中的诱饵渠道，测试请求
// 必须一条都不打给它。
func TestRunChannelImageEndpointGoesThroughRealImagesPath(t *testing.T) {
	ctx := setupModelTestDB(t)

	target, targetRequests := imageTestUpstream(t)
	decoy, decoyRequests := imageTestUpstream(t)

	// 诱饵渠道先入库：op.ChannelCreate 会把它挂进该模型的默认分组，于是
	// 分组选路**能**选中它。如果图片测试走到分组选路上去，请求就会打在它身上。
	decoyChannel := dbmodel.Channel{
		Name:     "image-decoy-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gpt-image-1",
		BaseUrls: []dbmodel.BaseUrl{{URL: decoy.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "decoy-key"}},
	}
	if err := op.ChannelCreate(&decoyChannel, ctx); err != nil {
		t.Fatalf("create decoy channel: %v", err)
	}

	// 被测渠道是**表单里的草稿**：不入库、禁用、base_url 指向另一个上游。
	draft := dbmodel.Channel{
		Name:     "image-draft-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: target.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "image-key"}},
	}

	response, err := RunChannel(ctx, draft, dbmodel.ModelTestRequest{
		Model:    "gpt-image-1",
		Endpoint: imageGenerationTestEndpointName,
	})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if response.Summary.Total != 1 || response.Summary.Success != 1 {
		t.Fatalf("图片渠道测试必须成功, got summary %#v / results %#v", response.Summary, response.Results)
	}
	result := response.Results[0]
	if !result.Success {
		t.Fatalf("unexpected image channel result: %#v", result)
	}
	if result.RequestEndpoint != imageGenerationTestEndpointName {
		t.Fatalf("request_endpoint = %q, want %q", result.RequestEndpoint, imageGenerationTestEndpointName)
	}
	if result.RequestPath != "/v1/images/generations" || result.UpstreamPath != "/v1/images/generations" {
		t.Fatalf("request path = %q / upstream path = %q, want /v1/images/generations on both", result.RequestPath, result.UpstreamPath)
	}
	if result.ResponsePreview != "1 image(s) (url)" {
		t.Fatalf("response preview = %q, want 1 image(s) (url)", result.ResponsePreview)
	}
	if result.InputTokens != 11 || result.OutputTokens != 22 {
		t.Fatalf("token usage = %d/%d, want 11/22（上游 usage 要用上）", result.InputTokens, result.OutputTokens)
	}
	if result.ChannelKeyID != 0 {
		// 草稿渠道的 key 没入库，ID 只能是 0；这条断言防的是"报告里编了个 key id"。
		t.Fatalf("channel key id = %d, want 0 for an unsaved draft key", result.ChannelKeyID)
	}

	hits := targetRequests()
	if len(hits) != 1 {
		t.Fatalf("目标上游必须恰好收到 1 条请求, got %d", len(hits))
	}
	hit := hits[0]
	if hit.method != http.MethodPost {
		t.Fatalf("上游方法 = %q, want POST", hit.method)
	}
	if hit.path != "/v1/images/generations" {
		t.Fatalf("上游路径 = %q, want /v1/images/generations", hit.path)
	}
	if hit.authorization != "Bearer image-key" {
		t.Fatalf("上游 Authorization = %q, want 目标渠道自己的 key", hit.authorization)
	}
	if !strings.Contains(strings.ToLower(hit.contentType), "application/json") {
		t.Fatalf("上游 Content-Type = %q, want application/json", hit.contentType)
	}
	if hit.body["prompt"] != "a cute cat" {
		t.Fatalf("上游 prompt = %#v, want a cute cat（照 new-api）", hit.body["prompt"])
	}
	if hit.body["n"] != float64(1) {
		t.Fatalf("上游 n = %#v, want 1", hit.body["n"])
	}
	if hit.body["size"] != "1024x1024" {
		t.Fatalf("上游 size = %#v, want 1024x1024", hit.body["size"])
	}
	if hit.body["model"] != "gpt-image-1" {
		t.Fatalf("上游 model = %#v, want gpt-image-1", hit.body["model"])
	}
	if _, ok := hit.body["messages"]; ok {
		t.Fatalf("图片请求不许带 messages: %#v", hit.body)
	}

	if decoyHits := decoyRequests(); len(decoyHits) != 0 {
		t.Fatalf("测试请求打到非目标渠道了（%d 条）：图片测试必须钉死在被测渠道上", len(decoyHits))
	}
}

// A4：不支持图片的渠道类型必须**明确跳过并记录原因**，不许硬塞一个假端点。
// 判据是 relay 自己的调度判据：Anthropic / Volcengine / OpenAIEmbedding 三类在
// ImagesHandler 里永远被 "unsupported channel type" 跳过，测它们等于测一条
// 生产走不通的路。
func TestRunChannelImageEndpointSkipsUnsupportedChannelTypes(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream, hits := imageTestUpstream(t)
	for _, channelType := range []outbound.OutboundType{
		outbound.OutboundTypeAnthropic,
		outbound.OutboundTypeVolcengine,
		outbound.OutboundTypeOpenAIEmbedding,
	} {
		response, err := RunChannel(ctx, dbmodel.Channel{
			Name:     "not-an-image-channel",
			Type:     channelType,
			Enabled:  false,
			BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
			Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		}, dbmodel.ModelTestRequest{
			Model:    "gpt-image-1",
			Endpoint: imageGenerationTestEndpointName,
		})
		if err != nil {
			t.Fatalf("RunChannel(type=%d): %v", channelType, err)
		}
		result := response.Results[0]
		if result.Success {
			t.Fatalf("channel type %d 不支持图片，不许测出成功: %#v", channelType, result)
		}
		if len(result.Attempts) == 0 {
			t.Fatalf("channel type %d 应产生一条跳过记录: %#v", channelType, result)
		}
		msg := result.Attempts[0].Msg
		if !strings.Contains(msg, "does not support the images endpoint") {
			t.Fatalf("channel type %d 的跳过原因不对: %q", channelType, msg)
		}
	}
	if got := hits(); len(got) != 0 {
		t.Fatalf("不支持图片的渠道类型不许发出任何上游请求, got %d", len(got))
	}
}

// A5：上游报错要如实回到结果里（状态码 + 失败记录），不能被吞成"成功"，
// 也不能重试成一串骚扰请求。
func TestRunChannelImageEndpointReportsUpstreamFailure(t *testing.T) {
	ctx := setupModelTestDB(t)

	var (
		mu    sync.Mutex
		count int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model does not support size","type":"invalid_request_error","code":"invalid_size"}}`))
	}))
	t.Cleanup(upstream.Close)

	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:     "image-failing-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "image-key"}},
	}, dbmodel.ModelTestRequest{
		Model:    "gpt-image-1",
		Endpoint: imageGenerationTestEndpointName,
	})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	result := response.Results[0]
	if result.Success {
		t.Fatalf("上游 400 不许算成功: %#v", result)
	}
	if result.StatusCode != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", result.StatusCode)
	}
	if strings.TrimSpace(result.Error) == "" {
		t.Fatalf("失败必须带错误说明: %#v", result)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Status != dbmodel.AttemptFailed {
		t.Fatalf("应有一条失败尝试记录, got %#v", result.Attempts)
	}
	mu.Lock()
	rounds := count
	mu.Unlock()
	if rounds != 1 {
		t.Fatalf("400 不重试，上游应只收到 1 条请求, got %d", rounds)
	}
}

// A5：显式 prompt 要透到上游（渠道测试页允许自定义 prompt）。
func TestRunChannelImageEndpointForwardsExplicitPrompt(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream, hits := imageTestUpstream(t)
	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:     "image-prompt-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "image-key"}},
	}, dbmodel.ModelTestRequest{
		Model:    "gpt-image-1",
		Endpoint: imageGenerationTestEndpointName,
		Prompt:   "draw an octopus",
	})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if !response.Results[0].Success {
		t.Fatalf("unexpected result: %#v", response.Results[0])
	}
	got := hits()
	if len(got) != 1 {
		t.Fatalf("上游应收到 1 条请求, got %d", len(got))
	}
	if got[0].body["prompt"] != "draw an octopus" {
		t.Fatalf("上游 prompt = %#v, want draw an octopus", got[0].body["prompt"])
	}
}
