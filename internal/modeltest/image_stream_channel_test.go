package modeltest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// next-1129 第 10 件：流式出图检测类型的验收。
//
// 全部用 httptest 假上游推 SSE 事件序列——**没有任何真实上游请求**（真测是用户上线
// 后自己在线上渠道点最多 3 张的事，代码里不留任何真实渠道调用）。
//
// 本文件钉住三件事：
//   1. 端点注册：streamImageGenerationTestEndpointName 及别名归一化到同一端点、
//      同一个 /v1/images/generations 上游地址、同一个审计端点前缀；
//   2. 出站字节：请求体只比非流式多一个 "stream":true，其余字段与既有非流式图片
//      检测逐字节一致，Accept 头是 text/event-stream；
//   3. 判图口径：SSE 帧累积出图 → 成功且预览里有张数/首图/总耗时/尺寸；
//      只有"completed 没图" → 必须报红（不许把没图当成功）。

// TestNormalizeEndpointSupportsStreamImages 钉住端点注册与别名。
func TestNormalizeEndpointSupportsStreamImages(t *testing.T) {
	for _, raw := range []string{
		"openai_images_generations_stream",
		"images_stream",
		"images_generations_stream",
		"openai_images_stream",
		" OpenAI-Images-Generations-Stream ",
	} {
		endpoint, err := normalizeEndpoint(raw)
		if err != nil {
			t.Fatalf("normalizeEndpoint(%q): %v", raw, err)
		}
		if endpoint.name != streamImageGenerationTestEndpointName {
			t.Fatalf("normalizeEndpoint(%q).name = %q, want %q", raw, endpoint.name, streamImageGenerationTestEndpointName)
		}
		if got := endpoint.path("any-model"); got != "/v1/images/generations" {
			t.Fatalf("normalizeEndpoint(%q).path = %q, want the same upstream path as the non-stream image endpoint", raw, got)
		}
		if !isImageTestEndpoint(raw) {
			t.Fatalf("isImageTestEndpoint(%q) = false; 流式出图必须与图片端点共用转发与准入路径", raw)
		}
		if !isStreamImageTestEndpoint(raw) {
			t.Fatalf("isStreamImageTestEndpoint(%q) = false", raw)
		}
	}

	// 老的非流式图片端点不能被新判据吞掉：它必须仍然"是图片端点但不是流式出图"。
	if isStreamImageTestEndpoint(imageGenerationTestEndpointName) {
		t.Fatalf("non-stream image endpoint must not be treated as the stream endpoint")
	}
	if !isImageTestEndpoint(imageGenerationTestEndpointName) {
		t.Fatalf("non-stream image endpoint must stay an image endpoint")
	}
	// chat / 嵌入 / 未知端点都不许被认成流式出图。
	for _, raw := range []string{"openai_chat", embeddingTestEndpointName, "openai_images_generationz"} {
		if isStreamImageTestEndpoint(raw) {
			t.Fatalf("isStreamImageTestEndpoint(%q) = true, want false", raw)
		}
	}
}

// TestModelTestAuditEndpointMapsStreamImages 钉住审计端点名：日志页的筛选/标签
// 靠 model_test 前缀认连通性测试，流式出图要自己一格，不能并进非流式出图。
func TestModelTestAuditEndpointMapsStreamImages(t *testing.T) {
	if got := modelTestAuditEndpoint(streamImageGenerationTestEndpointName); got != "model_test_images_stream" {
		t.Fatalf("modelTestAuditEndpoint(%q) = %q, want model_test_images_stream", streamImageGenerationTestEndpointName, got)
	}
	if got := modelTestAuditEndpoint(imageGenerationTestEndpointName); got != "model_test_images" {
		t.Fatalf("non-stream image audit endpoint changed: %q", got)
	}
}

// streamImageUpstream 起一个假上游：先按 SSE 推完给定帧再关流，并记录收到的请求。
func streamImageUpstream(t *testing.T, frames []string) (*httptest.Server, func() []recordedUpstreamRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		recorded []recordedUpstreamRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		recorded = append(recorded, recordedUpstreamRequest{
			method:        r.Method,
			path:          r.URL.Path,
			body:          body,
			rawBody:       rawBody,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 真上游开头也可能是心跳注释帧；判定不该把它当事件。
		_, _ = w.Write([]byte(":\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte(frame))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedUpstreamRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedUpstreamRequest(nil), recorded...)
	}
}

// TestRunChannelStreamImageEndpointGoesThroughRealImagesPath 是第 10 件的主证据：
// 流式出图必须走 relay.ImagesHandler 这条真链路，请求体只多一个 stream:true，
// 并且按 SSE 帧累积判定出图。
func TestRunChannelStreamImageEndpointGoesThroughRealImagesPath(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream, hits := streamImageUpstream(t, []string{
		`data: {"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"aW1hZ2Utb25l","size":"1024x1024"}` + "\n\n",
		// 中间夹一条心跳注释与一条 event: 行，判定都不该受影响。
		":keepalive\n\n",
		"event: image_generation.completed\n" +
			`data: {"type":"image_generation.completed","b64_json":"aW1hZ2UtdHdv","size":"1024x1024","usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}` + "\n\n",
	})

	channel := dbmodel.Channel{
		Name:     "stream-image-draft-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "stream-image-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model:    "gpt-image-1",
		Endpoint: streamImageGenerationTestEndpointName,
	})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if response.Summary.Total != 1 || response.Summary.Success != 1 {
		t.Fatalf("流式出图渠道测试必须成功, got summary %#v / results %#v", response.Summary, response.Results)
	}
	result := response.Results[0]
	if !result.Success {
		t.Fatalf("unexpected stream image result: %#v", result)
	}
	if result.RequestEndpoint != streamImageGenerationTestEndpointName {
		t.Fatalf("request_endpoint = %q, want %q", result.RequestEndpoint, streamImageGenerationTestEndpointName)
	}
	if result.RequestPath != "/v1/images/generations" || result.UpstreamPath != "/v1/images/generations" {
		t.Fatalf("request path = %q / upstream path = %q, want /v1/images/generations on both", result.RequestPath, result.UpstreamPath)
	}
	if !result.IsStream {
		t.Fatalf("is_stream = false, want true（流式出图端点必须如实标记流式）")
	}
	// 预览：张数 + 首图耗时 + 总耗时 + 尺寸。耗时是实测值，只钉形状不钉数字。
	wantPreview := regexp.MustCompile(`^2 image\(s\) \(stream, first \d+ms / total \d+ms, 1024x1024\)$`)
	if !wantPreview.MatchString(result.ResponsePreview) {
		t.Fatalf("response preview = %q, want %s", result.ResponsePreview, wantPreview)
	}
	if result.InputTokens != 11 || result.OutputTokens != 22 {
		t.Fatalf("token usage = %d/%d, want 11/22（SSE completed 帧的 usage 要用上）", result.InputTokens, result.OutputTokens)
	}

	recorded := hits()
	if len(recorded) != 1 {
		t.Fatalf("上游必须恰好收到 1 条请求, got %d", len(recorded))
	}
	hit := recorded[0]
	// 与非流式图片检测逐字节对齐，只多一个 stream:true。
	const wantBody = `{"model":"gpt-image-1","n":1,"prompt":"a cute cat","size":"1024x1024","stream":true}`
	if string(hit.rawBody) != wantBody {
		t.Fatalf("stream image request bytes = %s, want %s", hit.rawBody, wantBody)
	}
	if hit.method != http.MethodPost {
		t.Fatalf("上游方法 = %q, want POST", hit.method)
	}
	if hit.path != "/v1/images/generations" {
		t.Fatalf("上游路径 = %q, want /v1/images/generations", hit.path)
	}
	if hit.authorization != "Bearer stream-image-key" {
		t.Fatalf("上游 Authorization = %q, want the channel's own key", hit.authorization)
	}
	if hit.body["stream"] != true {
		t.Fatalf("上游 stream = %#v, want true", hit.body["stream"])
	}
}

// TestRunChannelStreamImageEndpointRejectsImageLessStream 钉住反向判据：上游推了
// completed 事件却没有任何图片字节，必须报红。把"完成"当"出图"是假绿灯。
func TestRunChannelStreamImageEndpointRejectsImageLessStream(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream, _ := streamImageUpstream(t, []string{
		`data: {"type":"image_generation.completed","size":"1024x1024","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}` + "\n\n",
	})

	channel := dbmodel.Channel{
		Name:     "stream-image-empty-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "stream-image-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model:    "gpt-image-1",
		Endpoint: streamImageGenerationTestEndpointName,
	})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if response.Summary.Success != 0 || len(response.Results) != 1 {
		t.Fatalf("无图的流式响应必须判红, got %#v", response.Summary)
	}
	result := response.Results[0]
	if result.Success {
		t.Fatalf("无图的流式响应被判成功了: %#v", result)
	}
	if !strings.Contains(result.Error, "no image event") {
		t.Fatalf("错误文案必须说清是没图: %q", result.Error)
	}
}

// TestStreamImageRecorderCountsFramesAcrossChunks 直接喂 recorder：SSE 帧边界与
// 网络块边界无关（data 行可能被切在两块里），并且要认 Gemini/Vertex 家族的
// inlineData 形状。这一条是纯单测，不需要 DB / 网络。
func TestStreamImageRecorderCountsFramesAcrossChunks(t *testing.T) {
	recorder := newImageStreamTestRecorder()
	first := `data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"cG5nLW9uZQ=="}}]}}]}` + "\n\n" + `data: {"cand`
	second := `idates":[{"content":{"parts":[{"inline_data":{"data":"cG5nLXR3bw=="}}]}}]}` + "\n\n"
	if _, err := recorder.Write([]byte(first)); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if recorder.summary.images != 1 {
		t.Fatalf("images after first chunk = %d, want 1", recorder.summary.images)
	}
	if _, err := recorder.Write([]byte(second)); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if recorder.summary.images != 2 {
		t.Fatalf("images after second chunk = %d, want 2（跨块的 data 行必须拼起来再解析）", recorder.summary.images)
	}
	if recorder.summary.size != "" {
		t.Fatalf("size = %q, want empty（事件没给 size 时留空，由调用方回落到请求 size）", recorder.summary.size)
	}
	if recorder.FirstImageLatency() < 0 {
		t.Fatalf("first image latency must not be negative")
	}
}
