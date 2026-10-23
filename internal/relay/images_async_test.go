package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 任务行上的 Endpoint 必须能换算回正确的上游子路径。
//
// 空值 = generations 是**存量兼容**语义（见 model.ImageTask.Endpoint）：
// 取任何一个非空零值都会让一次重启把在途任务的查询路径改错。
func TestImageTaskEndpointPathDefaultsToGenerations(t *testing.T) {
	cases := map[string]string{
		"":                                 "/images/generations",
		model.ImageTaskEndpointGenerations: "/images/generations",
		model.ImageTaskEndpointEdits:       "/images/edits",
		"  edits  ":                        "/images/edits",
		"something-else":                   "/images/generations",
	}
	for input, want := range cases {
		if got := imageTaskEndpointPath(input); got != want {
			t.Fatalf("imageTaskEndpointPath(%q) = %q, want %q", input, got, want)
		}
	}
}

// 开关读不到（settingCache 里没有这一行）= 关闭。这个功能扣钱，任何不确定都要落在
// "不对外存在"这一侧。
func TestImageTaskAsyncEnabledDefaultsToFalse(t *testing.T) {
	if ImageTaskAsyncEnabled() {
		t.Fatal("the async image switch must be off when the setting is unreadable")
	}
}

// b64_json 必须被剔除（几 MB 的 base64 会撑爆 text 列与日志页），其余字段保留，
// 结果仍是合法 JSON —— 查询接口直接把它当 json.RawMessage 编码。
func TestImageTaskResultPayloadStripsBase64(t *testing.T) {
	big := strings.Repeat("A", 4096)
	body := []byte(`{"created":1700000000,"data":[{"b64_json":"` + big + `","revised_prompt":"a fox"}]}`)

	resultJSON, imageURL := ImageTaskResultPayload(body)
	if imageURL != "" {
		t.Fatalf("image_url = %q, want empty for a b64-only response", imageURL)
	}
	if !json.Valid([]byte(resultJSON)) {
		t.Fatalf("result_json is not valid JSON: %q", resultJSON)
	}
	if strings.Contains(resultJSON, big) {
		t.Fatalf("result_json still carries the base64 payload (%d bytes)", len(resultJSON))
	}
	if !strings.Contains(resultJSON, "omitted") || !strings.Contains(resultJSON, "a fox") {
		t.Fatalf("result_json lost its diagnostic value: %q", resultJSON)
	}

	// 有 url 的正常形状：url 要能取出来，并且原样进 result。
	withURL := []byte(`{"data":[{"url":"https://images.upstream.example/x.png"}]}`)
	resultJSON, imageURL = ImageTaskResultPayload(withURL)
	if imageURL != "https://images.upstream.example/x.png" {
		t.Fatalf("image_url = %q", imageURL)
	}
	if !strings.Contains(resultJSON, "images.upstream.example") {
		t.Fatalf("result_json = %q, want the upstream payload", resultJSON)
	}

	// 读不懂的响应：不落原始字节，也不炸。
	if resultJSON, imageURL := ImageTaskResultPayload([]byte("<html>nope</html>")); resultJSON != "" || imageURL != "" {
		t.Fatalf("unparseable body produced (%q, %q), want empty", resultJSON, imageURL)
	}
}

// ImageTaskRunUpstream 是后台执行器唯一的出口：路径、鉴权头、映射后的模型名
// 都必须与同步图片路径一致，否则"异步"就是另一条会悄悄画错图的链路。
func TestImageTaskRunUpstreamReplaysToTheRecordedEndpoint(t *testing.T) {
	type captured struct {
		path   string
		auth   string
		accept string
		ua     string
		model  string
		prompt string
	}
	got := make(chan captured, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		modelName, _ := payload["model"].(string)
		prompt, _ := payload["prompt"].(string)
		got <- captured{
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			accept: r.Header.Get("Accept"),
			ua:     r.Header.Get("User-Agent"),
			model:  modelName,
			prompt: prompt,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://images.upstream.example/y.png"}]}`))
	}))
	t.Cleanup(upstream.Close)

	channel := &model.Channel{
		ID:       9,
		Name:     "image-async-unit-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{ID: 3, Enabled: true, ChannelKey: "image-async-unit-key"}},
		CustomHeader: []model.CustomHeader{{
			HeaderKey:   "X-Image-Route",
			HeaderValue: "async",
		}},
	}

	statusCode, body, failReason, err := ImageTaskRunUpstream(
		context.Background(),
		channel,
		"image-async-unit-key",
		model.ImageTaskEndpointEdits,
		"mapped-upstream-model",
		[]byte(`{"model":"client-alias","prompt":"a fox in snow"}`),
	)
	if err != nil || failReason != "" {
		t.Fatalf("run upstream: err=%v failReason=%q", err, failReason)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusCode)
	}
	if !strings.Contains(string(body), "images.upstream.example") {
		t.Fatalf("body = %q", string(body))
	}

	select {
	case seen := <-got:
		if seen.path != "/v1/images/edits" {
			t.Fatalf("upstream path = %q, want /v1/images/edits", seen.path)
		}
		if seen.auth != "Bearer image-async-unit-key" {
			t.Fatalf("Authorization = %q", seen.auth)
		}
		if seen.accept != "application/json" {
			t.Fatalf("Accept = %q, want application/json", seen.accept)
		}
		if seen.ua == "" || strings.Contains(seen.ua, "Go-http-client") {
			t.Fatalf("User-Agent = %q, want a non-Go default UA", seen.ua)
		}
		// 与同步路径一致：上游拿到的是映射后的模型名，不是客户端别名。
		if seen.model != "mapped-upstream-model" {
			t.Fatalf("upstream model = %q, want mapped-upstream-model", seen.model)
		}
		if seen.prompt != "a fox in snow" {
			t.Fatalf("prompt = %q; the stored request body must be replayed verbatim", seen.prompt)
		}
	default:
		t.Fatal("the upstream was never called")
	}
}

// 上游非 2xx：状态码原样返回，失败原因是一段**脱敏**的摘要（进 error_message，
// 是给前端看的），不是整段上游响应。
func TestImageTaskRunUpstreamReportsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited, key sk-live-abcdef0123456789"}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := &model.Channel{
		ID:       10,
		Name:     "image-async-unit-fail-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
	}

	statusCode, _, failReason, err := ImageTaskRunUpstream(
		context.Background(), channel, "unit-key", model.ImageTaskEndpointGenerations, "m", []byte(`{"prompt":"x"}`),
	)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if statusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", statusCode)
	}
	if failReason == "" || !strings.Contains(failReason, "429") {
		t.Fatalf("failReason = %q, want it to mention the status", failReason)
	}
	if strings.Contains(failReason, "sk-live-abcdef0123456789") {
		t.Fatalf("failReason leaked an upstream credential: %q", failReason)
	}
}
