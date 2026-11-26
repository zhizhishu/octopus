package modeltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestChannelTestSelectedUpstreamEndpoint(t *testing.T) {
	ctx := setupModelTestDB(t)
	for _, direction := range []struct {
		name             string
		stored, selected outbound.OutboundType
		endpoint, model  string
	}{
		{"responses-to-claude", outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeAnthropic, "anthropic_messages", "claude-opus-5-5"},
		{"claude-to-responses", outbound.OutboundTypeAnthropic, outbound.OutboundTypeOpenAIResponse, "openai_responses", "gpt-6-astra"},
	} {
		for _, cloak := range []string{"auto", "never"} {
			for _, selectUpstream := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/selected=%t", direction.name, cloak, selectUpstream)
				t.Run(name, func(t *testing.T) {
					wantType := direction.stored
					if selectUpstream {
						wantType = direction.selected
					}
					wantPath := "/v1/responses"
					if wantType == outbound.OutboundTypeAnthropic {
						wantPath = "/v1/messages"
					}
					wantStream := wantType == outbound.OutboundTypeOpenAIResponse || cloak == "auto"
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							http.Error(w, "invalid JSON", 400)
							return
						}
						if r.URL.Path != wantPath {
							t.Errorf("path=%s want %s", r.URL.Path, wantPath)
							http.Error(w, "wrong endpoint", 400)
							return
						}
						if body["model"] != direction.model {
							t.Errorf("model changed: %v", body["model"])
						}
						if got := body["stream"] == true; got != wantStream {
							t.Errorf("stream=%t want %t", got, wantStream)
						}
						if wantType == outbound.OutboundTypeAnthropic {
							if lenValue(body["messages"]) == 0 || body["input"] != nil {
								t.Error("wrong Anthropic body")
								http.Error(w, "invalid messages", 400)
								return
							}
							if r.Header.Get("Authorization") != "Bearer test-key" {
								t.Error("Anthropic proxy auth header missing")
							}
							if cloak == "auto" && r.Header.Get("X-Api-Key") != "" {
								t.Error("cloaked Claude must not send duplicate auth headers")
							}
							if selectUpstream && direction.model == "claude-opus-5-5" {
								thinking, _ := body["thinking"].(map[string]any)
								if thinking["type"] == "disabled" {
									t.Error("adaptive-only Claude received disabled thinking")
								}
							}
							if wantStream {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", direction.model)
							} else {
								w.Header().Set("Content-Type", "application/json")
								fmt.Fprintf(w, `{"id":"msg_test","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`, direction.model)
							}
						} else {
							if (lenValue(body["input"]) == 0 && body["input"] == "") || body["input"] == nil || body["messages"] != nil || body["thinking"] != nil {
								t.Error("wrong Responses body")
								http.Error(w, "invalid input", 400)
								return
							}
							if r.Header.Get("Authorization") != "Bearer test-key" {
								t.Error("Responses auth header missing")
							}
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n", direction.model, direction.model)
						}
					}))
					defer upstream.Close()
					channel := dbmodel.Channel{Name: name, Type: direction.stored, Enabled: true, Model: direction.model, BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}}, Cloak: dbmodel.ChannelCloak{Mode: cloak}}
					if err := op.ChannelCreate(&channel, ctx); err != nil {
						t.Fatal(err)
					}
					stream := false
					req := dbmodel.ModelTestRequest{Model: direction.model, ChannelID: channel.ID, Endpoint: direction.endpoint, TestUpstreamEndpoint: selectUpstream, Prompt: "Reply OK.", Stream: &stream}
					for _, draft := range []bool{false, true} {
						var result dbmodel.ModelTestResponse
						var err error
						if draft {
							result, err = RunChannel(ctx, channel, req)
						} else {
							result, err = Run(ctx, req)
						}
						if err != nil {
							t.Fatal(err)
						}
						// relay 链路不回填 result.UpstreamPath（探针自建出站的元数据）；
						// 上游真正命中的路径由 mock 里的 r.URL.Path != wantPath 断言兜底。
						if len(result.Results) != 1 || !result.Results[0].Success || result.Results[0].ResponsePreview != "OK" {
							t.Fatalf("draft=%t result=%#v", draft, result)
						}
					}
					if calls.Load() != 2 {
						t.Fatalf("actual upstream requests=%d want2", calls.Load())
					}
					stored, err := op.ChannelGet(channel.ID, ctx)
					if err != nil {
						t.Fatal(err)
					}
					if channel.Type != direction.stored || stored.Type != direction.stored {
						t.Fatal("test modified saved channel type")
					}
				})
			}
		}
	}
}

func lenValue(value any) int {
	if items, ok := value.([]any); ok {
		return len(items)
	}
	return 0
}

// customDerivedRequest 记录派生端点测试里上游真正收到的请求。
// query 用 Query() 而不是 RawQuery 比较：Gemini 适配器会往 query 里加 key，
// 顺序与编码都不再由我们自己控制。
type customDerivedRequest struct {
	path          string
	query         map[string][]string
	authorization string
	body          map[string]any
}

func customDerivedChatFixture(model string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, model)
}

func customDerivedResponsesSSE(model string) string {
	return fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n", model, model)
}

func customDerivedAnthropicFixture(model string) string {
	return fmt.Sprintf(`{"id":"msg_test","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`, model)
}

func customDerivedGeminiFixture(model string) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"modelVersion":%q,"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`, model)
}

func customDerivedEmbeddingFixture(string) string {
	return `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
}

func customDerivedDecisionFixture(string) string { return `{"state":"pong"}` }

// 定制渠道（type 6）存的是一条完整请求 URL。只有显式端点 "custom" 原样使用它；
// 其余选择都是“显式协议探测”，一律派生成同源的**标准**路径。旧契约
// （自定义渠道永远不能被问另一个协议）按需求反转；不受影响的那半仍然钉死：
// 不带 test_upstream_endpoint 的普通调用一个字节也不许离开存的那条 URL。
func TestChannelTestCustomChannelEndpointStaysVerbatim(t *testing.T) {
	ctx := setupModelTestDB(t)

	var (
		mu     sync.Mutex
		paths  []string
		auths  []string
		bodies []map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(customDerivedChatFixture("gpt-6-astra")))
	}))
	defer upstream.Close()

	fullURL := upstream.URL + "/gateway/proxy/chat/completions"
	channel := dbmodel.Channel{
		Name:     "custom-verbatim",
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []dbmodel.BaseUrl{{URL: fullURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	snapshot := func() ([]string, []string, []map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]string(nil), auths...), append([]map[string]any(nil), bodies...)
	}

	// 1. "custom" 离开显式渠道测试就被拒绝，且绝不发出请求。
	refused, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{Model: "gpt-6-astra", Endpoint: customTestEndpointName})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := snapshot(); len(got) != 0 {
		t.Fatalf("被拒绝的 custom 端点不许打到上游, got %d 条", len(got))
	}
	if len(refused.Results) != 1 || refused.Results[0].Success || !strings.Contains(refused.Results[0].Error, "only available in an explicit channel test") {
		t.Fatalf("unexpected refusal %#v", refused.Results)
	}

	// 2. 带显式开关时，"custom" 原样打存储的完整地址。
	noStream := false
	verbatim, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model: "gpt-6-astra", Endpoint: customTestEndpointName, TestUpstreamEndpoint: true,
		Stream: &noStream, Prompt: "Reply OK.",
	})
	if err != nil {
		t.Fatal(err)
	}
	// relay 链路不回填 result.UpstreamPath；上游命中路径以 mock 实收的为准（下面 snapshot 断言）。
	if len(verbatim.Results) != 1 || !verbatim.Results[0].Success {
		t.Fatalf("verbatim result %#v", verbatim.Results)
	}
	gotPaths, gotAuths, gotBodies := snapshot()
	if len(gotPaths) != 1 || gotPaths[0] != "/gateway/proxy/chat/completions" {
		t.Fatalf("upstream paths = %v, want the stored full URL", gotPaths)
	}
	if gotAuths[0] != "Bearer test-key" {
		t.Fatalf("authorization = %q, want the channel key", gotAuths[0])
	}
	if gotBodies[0]["messages"] == nil {
		t.Fatalf("custom 端点必须仍是 chat 报文, got %#v", gotBodies[0])
	}

	// 3. 显式协议选择派生成同源标准路径，而不是存的那条路径。
	derived, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model: "gpt-6-astra", Endpoint: "openai_chat", TestUpstreamEndpoint: true,
		Stream: &noStream, Prompt: "Reply OK.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(derived.Results) != 1 || !derived.Results[0].Success {
		t.Fatalf("derived chat result %#v", derived.Results)
	}
	gotPaths, _, _ = snapshot()
	if len(gotPaths) != 2 || gotPaths[1] != "/v1/chat/completions" {
		t.Fatalf("upstream paths = %v, want /v1/chat/completions on the second call", gotPaths)
	}

	// 4. 不带显式开关的普通调用（草稿 / 预览）仍然原样打存储的 URL：派生只发生在
	//    显式渠道测试里，type 6 的常规流量字节不变。
	plain, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model: "gpt-6-astra", Endpoint: "openai_chat", Stream: &noStream, Prompt: "Reply OK.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Results) != 1 {
		t.Fatalf("a non-explicit run must produce exactly one result, got %#v", plain.Results)
	}
	gotPaths, _, _ = snapshot()
	if len(gotPaths) != 3 || gotPaths[2] != "/gateway/proxy/chat/completions" {
		t.Fatalf("upstream paths = %v, want the verbatim URL on the third call", gotPaths)
	}

	// 5. 存的渠道与它的 BaseUrls 切片一个字段都没变。
	if channel.Type != outbound.OutboundTypeCustomOpenAIChat || len(channel.BaseUrls) != 1 || channel.BaseUrls[0].URL != fullURL {
		t.Fatalf("stored channel mutated: %#v", channel)
	}
	if channel.OpenAIChatPath != "" || channel.OpenAIImagesPath != "" || channel.OpenAIEmbeddingsPath != "" {
		t.Fatalf("stored path overrides mutated: %#v", channel)
	}
}

// 每个显式协议选择都必须打到同源的**标准**路径，带上渠道 key，原来的 query 保留，
// 且报文形状由所选协议的适配器决定（chat/responses/messages/contents/embeddings/决策）。
func TestChannelTestCustomChannelDerivesSameOriginStandardEndpoints(t *testing.T) {
	ctx := setupModelTestDB(t)
	noStream := false

	cases := []struct {
		name        string
		endpoint    string
		stream      *bool
		contentType string
		reply       func(model string) string
		wantPath    string
		wantBody    []string
		absentBody  []string
	}{
		{
			name: "chat", endpoint: "openai_chat", stream: &noStream,
			contentType: "application/json", reply: customDerivedChatFixture,
			wantPath: "/v1/chat/completions", wantBody: []string{"messages", "model"}, absentBody: []string{"input"},
		},
		{
			// Responses 渠道会强制流式（与真 codex 流量一致），所以这里不钉 stream。
			name: "responses", endpoint: "openai_responses",
			contentType: "text/event-stream", reply: customDerivedResponsesSSE,
			wantPath: "/v1/responses", wantBody: []string{"input"}, absentBody: []string{"messages"},
		},
		{
			name: "anthropic", endpoint: "anthropic_messages", stream: &noStream,
			contentType: "application/json", reply: customDerivedAnthropicFixture,
			wantPath: "/v1/messages", wantBody: []string{"messages", "max_tokens"},
		},
		{
			name: "gemini", endpoint: "gemini_generate_content", stream: &noStream,
			contentType: "application/json", reply: customDerivedGeminiFixture,
			wantPath: "/v1beta/models/gpt-6-astra:generateContent", wantBody: []string{"contents"}, absentBody: []string{"messages"},
		},
		{
			name: "embeddings", endpoint: embeddingTestEndpointName, stream: &noStream,
			contentType: "application/json", reply: customDerivedEmbeddingFixture,
			wantPath: "/v1/embeddings", wantBody: []string{"input", "model"}, absentBody: []string{"messages"},
		},
		{
			name: "decision", endpoint: "systemone", stream: &noStream,
			contentType: "application/json", reply: customDerivedDecisionFixture,
			wantPath: "/v1/systemone", wantBody: []string{"state", "model"}, absentBody: []string{"messages"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				seen []customDerivedRequest
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, customDerivedRequest{
					path:          r.URL.Path,
					query:         r.URL.Query(),
					authorization: r.Header.Get("Authorization"),
					body:          body,
				})
				mu.Unlock()
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.reply("gpt-6-astra")))
			}))
			defer upstream.Close()

			storedPath := upstream.URL + "/gateway/proxy/chat/completions?api-version=v9"
			channel := dbmodel.Channel{
				Name:     "custom-derived-" + tc.name,
				Type:     outbound.OutboundTypeCustomOpenAIChat,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL + "/other/stored/base"}},
				// 绝对 chat 路径覆盖优先于 BaseUrls（与真实转发一致），派生的同源根
				// 必须来自它，而覆盖本身不许泄漏进派生端点。
				OpenAIChatPath:   storedPath,
				OpenAIImagesPath: "https://cdn.example/openai/images",
				Keys:             []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
				Cloak:            dbmodel.ChannelCloak{Mode: "never"},
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			result, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
				Model: "gpt-6-astra", Endpoint: tc.endpoint, TestUpstreamEndpoint: true,
				Stream: tc.stream, Prompt: "Reply OK.",
			})
			if err != nil {
				t.Fatalf("RunChannel: %v", err)
			}

			mu.Lock()
			recorded := append([]customDerivedRequest(nil), seen...)
			mu.Unlock()
			if len(recorded) != 1 {
				t.Fatalf("upstream requests = %d, want 1 (%#v)", len(recorded), result.Results)
			}
			got := recorded[0]
			if got.path != tc.wantPath {
				t.Fatalf("upstream path = %q, want %q", got.path, tc.wantPath)
			}
			if values := got.query["api-version"]; len(values) != 1 || values[0] != "v9" {
				t.Fatalf("base query lost: api-version = %v", got.query)
			}
			if tc.endpoint == "gemini_generate_content" {
				// Gemini 适配器把 key 放进 query，不发 Authorization 头。
				if values := got.query["key"]; len(values) != 1 || values[0] != "test-key" {
					t.Fatalf("gemini query key = %v, want exactly one test-key", values)
				}
				if got.authorization != "" {
					t.Fatalf("gemini authorization = %q, want empty", got.authorization)
				}
			} else if got.authorization != "Bearer test-key" {
				t.Fatalf("authorization = %q, want the channel key", got.authorization)
			}
			for _, key := range tc.wantBody {
				if _, ok := got.body[key]; !ok {
					t.Fatalf("body is missing %q: %#v", key, got.body)
				}
			}
			for _, key := range tc.absentBody {
				if _, ok := got.body[key]; ok {
					t.Fatalf("body must not carry %q: %#v", key, got.body)
				}
			}

			// relay 链路不回填 result.UpstreamPath；上游命中路径以 mock 实收的 got.path 为准。
			if len(result.Results) != 1 || !result.Results[0].Success {
				t.Fatalf("result %#v, want success on %s", result.Results, tc.wantPath)
			}

			// 存的渠道一个字段都没变（Type/BaseUrls/路径覆盖）。
			if channel.Type != outbound.OutboundTypeCustomOpenAIChat || channel.BaseUrls[0].URL != upstream.URL+"/other/stored/base" {
				t.Fatalf("stored channel mutated: %#v", channel)
			}
			if channel.OpenAIChatPath != storedPath || channel.OpenAIImagesPath != "https://cdn.example/openai/images" {
				t.Fatalf("stored path overrides mutated: %#v", channel)
			}
			if len(channel.Capabilities) != 0 {
				t.Fatalf("stored capabilities mutated: %#v", channel.Capabilities)
			}
		})
	}
}

// 回归：普通渠道（0/1/2/3）的端点切换必须与改动前逐字一致 —— 只有四个 chat 协议
// 会换渠道类型；嵌入/图片/决策/自定义一律原样返回，不改类型、不复制、不派生。
func TestOrdinaryChannelEndpointSwitchKeepsBaseline(t *testing.T) {
	for _, channelType := range []outbound.OutboundType{
		outbound.OutboundTypeOpenAIChat,
		outbound.OutboundTypeOpenAIResponse,
		outbound.OutboundTypeAnthropic,
		outbound.OutboundTypeGemini,
	} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			// 非 chat 协议端点：必须原样返回同一个渠道（指针不变、类型不变）。
			for _, endpoint := range []string{embeddingTestEndpointName, "systemone", imageGenerationTestEndpointName} {
				original := &dbmodel.Channel{Type: channelType, BaseUrls: []dbmodel.BaseUrl{{URL: "https://upstream.example/v1"}}}
				selected, err := channelForTestEndpoint(original, endpoint)
				if err != nil {
					t.Fatalf("%s on type %d: %v", endpoint, channelType, err)
				}
				if selected != original || selected.Type != channelType {
					t.Fatalf("%s on type %d must keep the stored channel, got type %d", endpoint, channelType, selected.Type)
				}
			}
			// "custom" 只属于自定义渠道。
			if _, err := channelForTestEndpoint(&dbmodel.Channel{Type: channelType}, customTestEndpointName); err == nil {
				t.Fatalf("custom endpoint must be refused on channel type %d", channelType)
			}
			// 四个 chat 协议仍然按协议改类型（改动前的行为）。
			for endpoint, wantType := range map[string]outbound.OutboundType{
				"openai_chat":             outbound.OutboundTypeOpenAIChat,
				"openai_responses":        outbound.OutboundTypeOpenAIResponse,
				"anthropic_messages":      outbound.OutboundTypeAnthropic,
				"gemini_generate_content": outbound.OutboundTypeGemini,
			} {
				selected, err := channelForTestEndpoint(&dbmodel.Channel{Type: channelType}, endpoint)
				if err != nil {
					t.Fatalf("%s on type %d: %v", endpoint, channelType, err)
				}
				if selected.Type != wantType {
					t.Fatalf("%s on type %d: type = %d, want %d", endpoint, channelType, selected.Type, wantType)
				}
			}
		})
	}

	// nil 渠道不许 panic。
	if _, err := channelForTestEndpoint(nil, "openai_chat"); err == nil {
		t.Fatal("nil channel must be refused")
	}
}

// 派生副本的形状是这一改动的核心契约：类型按所选协议切换，BaseUrls 只剩同源根
// （带原 query），所有协议路径覆盖都被清空（包括绝对地址），原渠道与它的切片不动。
func TestCustomChannelDerivedEndpointCopyShape(t *testing.T) {
	const chatURL = "https://upstream.example/api/paas/v4/chat/completions?api-version=v9"
	const storedBase = "https://base.example/raw/chat"

	makeChannel := func() *dbmodel.Channel {
		return &dbmodel.Channel{
			ID:                   42,
			Name:                 "custom-shape",
			Type:                 outbound.OutboundTypeCustomOpenAIChat,
			BaseUrls:             []dbmodel.BaseUrl{{URL: storedBase}},
			OpenAIChatPath:       chatURL,
			OpenAIModelsPath:     "https://models.example/list",
			OpenAIEmbeddingsPath: "/relative/embeddings",
			OpenAIImagesPath:     "https://cdn.example/openai/images",
			OpenAIVideosPath:     "/openai/videos",
			OpenAITasksPath:      "/openai/tasks",
			Capabilities:         []string{"other"},
			Keys:                 []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		}
	}

	protocolTypes := map[string]outbound.OutboundType{
		"openai_chat":             outbound.OutboundTypeOpenAIChat,
		"openai_responses":        outbound.OutboundTypeOpenAIResponse,
		"anthropic_messages":      outbound.OutboundTypeAnthropic,
		"gemini_generate_content": outbound.OutboundTypeGemini,
		embeddingTestEndpointName: outbound.OutboundTypeOpenAIEmbedding,
		"systemone":               outbound.OutboundTypeSystemOne,
	}
	for name, wantType := range protocolTypes {
		original := makeChannel()
		selected, err := channelForTestEndpoint(original, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if selected == original {
			t.Fatalf("%s: a protocol test must use a copy, not the stored channel", name)
		}
		if selected.Type != wantType {
			t.Fatalf("%s: copy type = %d, want %d", name, selected.Type, wantType)
		}
		if len(selected.BaseUrls) != 1 || selected.BaseUrls[0].URL != "https://upstream.example?api-version=v9" {
			t.Fatalf("%s: copy BaseUrls = %#v, want the same-origin base with the stored query", name, selected.BaseUrls)
		}
		if selected.OpenAIChatPath != "" || selected.OpenAIModelsPath != "" || selected.OpenAIEmbeddingsPath != "" ||
			selected.OpenAIImagesPath != "" || selected.OpenAIVideosPath != "" || selected.OpenAITasksPath != "" {
			t.Fatalf("%s: stored path overrides leaked into the copy: %#v", name, selected)
		}

		// 原渠道：类型、BaseUrls 底层数组、路径覆盖、能力位切片全都不动。
		if original.Type != outbound.OutboundTypeCustomOpenAIChat || original.BaseUrls[0].URL != storedBase {
			t.Fatalf("%s: stored channel mutated: %#v", name, original)
		}
		if original.OpenAIChatPath != chatURL || original.OpenAIImagesPath != "https://cdn.example/openai/images" ||
			original.OpenAIEmbeddingsPath != "/relative/embeddings" || original.OpenAIModelsPath != "https://models.example/list" ||
			original.OpenAIVideosPath != "/openai/videos" || original.OpenAITasksPath != "/openai/tasks" {
			t.Fatalf("%s: stored path overrides mutated: %#v", name, original)
		}
		if len(original.Capabilities) != 1 || original.Capabilities[0] != "other" {
			t.Fatalf("%s: stored capabilities mutated: %#v", name, original.Capabilities)
		}
	}

	// 嵌入：类型改成 Embedding（它本身就声明了嵌入能力，不需要改能力位切片），
	// 存储的能力位不变。
	embeddingChannel := makeChannel()
	embeddingCopy, err := channelForTestEndpoint(embeddingChannel, embeddingTestEndpointName)
	if err != nil {
		t.Fatal(err)
	}
	if embeddingCopy.Type != outbound.OutboundTypeOpenAIEmbedding {
		t.Fatalf("embedding copy type = %d, want the embedding type", embeddingCopy.Type)
	}
	if !embeddingCopy.SupportsCapability(dbmodel.CapabilityEmbeddings) {
		t.Fatalf("embedding copy must support embeddings by type: %#v", embeddingCopy.Capabilities)
	}
	if len(embeddingChannel.Capabilities) != 1 || embeddingChannel.Capabilities[0] != "other" {
		t.Fatalf("stored capabilities mutated: %#v", embeddingChannel.Capabilities)
	}

	// 图片：临时副本走 chat 兼容类型，不填任何图片路径覆盖：清空覆盖后 relay 会用
	// 同源根的默认 /v1/images/generations（query 仍在 base 上）。
	imageChannel := makeChannel()
	imageCopy, err := channelForTestEndpoint(imageChannel, imageGenerationTestEndpointName)
	if err != nil {
		t.Fatal(err)
	}
	if imageCopy.Type != outbound.OutboundTypeOpenAIChat {
		t.Fatalf("images copy type = %d, want the chat-compatible type", imageCopy.Type)
	}
	if imageCopy.OpenAIImagesPath != "" {
		t.Fatalf("images copy must not carry a path override, got %q", imageCopy.OpenAIImagesPath)
	}
	if len(imageCopy.BaseUrls) != 1 || imageCopy.BaseUrls[0].URL != "https://upstream.example?api-version=v9" {
		t.Fatalf("images copy BaseUrls = %#v, want the same-origin base with the stored query", imageCopy.BaseUrls)
	}
	if imageChannel.Type != outbound.OutboundTypeCustomOpenAIChat || imageChannel.OpenAIImagesPath != "https://cdn.example/openai/images" {
		t.Fatalf("stored channel mutated: %#v", imageChannel)
	}

	// "custom"：原样返回存储的渠道，一个字都不改。
	verbatimChannel := makeChannel()
	verbatim, err := channelForTestEndpoint(verbatimChannel, customTestEndpointName)
	if err != nil {
		t.Fatal(err)
	}
	if verbatim != verbatimChannel || verbatim.OpenAIChatPath != chatURL {
		t.Fatalf("custom endpoint must keep the stored channel verbatim: %#v", verbatim)
	}

	// "custom" 只在自定义渠道上成立。
	if _, err := channelForTestEndpoint(&dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat}, customTestEndpointName); err == nil {
		t.Fatal("custom endpoint must be refused on a non-custom channel")
	}

	// 不可用的存储 URL：给清晰错误，且错误里不回显原值。
	if _, err := channelForTestEndpoint(&dbmodel.Channel{
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []dbmodel.BaseUrl{{URL: "not-a-url"}},
	}, "openai_responses"); err == nil {
		t.Fatal("an unusable stored URL must not derive an endpoint")
	} else if strings.Contains(err.Error(), "not-a-url") {
		t.Fatalf("derivation error echoed the stored URL: %v", err)
	}
}

// 图片端点也走真链路（relay images）。派生副本是 chat 兼容类型 + 清空的路径覆盖，
// 所以上游必须收到同源根的默认 /v1/images/generations，base 上的 query 原样带上；
// 原渠道里填的绝对图片路径（别的 origin）一律不许 hijack 这次测试。
func TestChannelTestCustomChannelDerivesSameOriginStandardImagesPath(t *testing.T) {
	ctx := setupModelTestDB(t)

	var (
		mu   sync.Mutex
		seen []customDerivedRequest
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, customDerivedRequest{
			path:          r.URL.Path,
			query:         r.URL.Query(),
			authorization: r.Header.Get("Authorization"),
			body:          body,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1730000000,"data":[{"url":"https://upstream.example/one.png"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`))
	}))
	defer upstream.Close()

	storedURL := upstream.URL + "/gateway/proxy/chat/completions?api-version=v9"
	channel := dbmodel.Channel{
		Name:     "custom-images",
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []dbmodel.BaseUrl{{URL: storedURL}},
		// 旧配置里可能留着别的 origin 的绝对图片路径：它不许决定本次派生测试的地址。
		OpenAIImagesPath: "https://cdn.example/openai/images",
		Keys:             []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "image-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	result, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{
		Model: "gpt-image-1", Endpoint: imageGenerationTestEndpointName, TestUpstreamEndpoint: true, Prompt: "draw an octopus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || !result.Results[0].Success {
		t.Fatalf("image test must succeed, got %#v", result.Results)
	}

	mu.Lock()
	recorded := append([]customDerivedRequest(nil), seen...)
	mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(recorded))
	}
	got := recorded[0]
	if got.path != "/v1/images/generations" {
		t.Fatalf("upstream path = %q, want the standard images path", got.path)
	}
	if values := got.query["api-version"]; len(values) != 1 || values[0] != "v9" {
		t.Fatalf("base query lost on the images endpoint: %v", got.query)
	}
	if got.authorization != "Bearer image-key" {
		t.Fatalf("authorization = %q, want the channel key", got.authorization)
	}
	if got.body["prompt"] != "draw an octopus" {
		t.Fatalf("upstream prompt = %#v", got.body["prompt"])
	}
	if _, ok := got.body["messages"]; ok {
		t.Fatalf("image request must not carry messages: %#v", got.body)
	}
	if result.Results[0].UpstreamPath != "/v1/images/generations" {
		t.Fatalf("result upstream path = %q", result.Results[0].UpstreamPath)
	}
	if channel.Type != outbound.OutboundTypeCustomOpenAIChat || channel.BaseUrls[0].URL != storedURL || channel.OpenAIImagesPath != "https://cdn.example/openai/images" {
		t.Fatalf("stored channel mutated: %#v", channel)
	}
}
