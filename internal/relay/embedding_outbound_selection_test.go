package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

func newEmbeddingInternalRequest(modelName string) *model.InternalLLMRequest {
	input := "向量测试输入"
	return &model.InternalLLMRequest{
		Model:          modelName,
		EmbeddingInput: &model.EmbeddingInput{Single: &input},
		RawAPIFormat:   model.APIFormatOpenAIEmbedding,
	}
}

// 整条 relay 链路：POST /v1/embeddings → 选路（能力位准入 + 出站纠正）→ 上游。
// 一个 chat 型渠道勾上 embeddings 后，上游必须收到**嵌入报文**打在渠道配置的嵌入路径上，
// 而不是被它自带的 ResponseOutbound 序列化成 responses body、也不是被类型兼容检查挡掉。
func TestRelayRoutesEmbeddingToCapabilityChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu          sync.Mutex
		sawPath     string
		sawInput    string
		sawMessages bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		sawPath = r.URL.Path
		if input, ok := payload["input"].(string); ok {
			sawInput = input
		}
		_, sawMessages = payload["messages"]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"emb-relay",
			"object":"list",
			"created":123,
			"model":"doubao-embedding",
			"data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]}],
			"usage":{"prompt_tokens":3,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:                 "capability-embedding-relay",
		Type:                 outbound.OutboundTypeOpenAIResponse,
		Enabled:              true,
		BaseUrls:             []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:                 []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-capability"}},
		Model:                "doubao-embedding",
		Capabilities:         []string{dbmodel.CapabilityEmbeddings},
		OpenAIEmbeddingsPath: "/embeddings",
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"doubao-embedding","input":"hello world"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIEmbedding, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("嵌入请求应当成功, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, input, hasMessages := sawPath, sawInput, sawMessages
	mu.Unlock()
	if path != "/embeddings" {
		t.Fatalf("上游收到 path %q, want /embeddings（渠道配置的嵌入路径）", path)
	}
	if input != "hello world" {
		t.Fatalf("上游收到 input %q, want hello world", input)
	}
	if hasMessages {
		t.Fatalf("嵌入报文里出现了 messages —— 说明走的是 responses/chat 序列化")
	}
	var response struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response %q: %v", rec.Body.String(), err)
	}
	if len(response.Data) != 1 || len(response.Data[0].Embedding) != 2 {
		t.Fatalf("客户端拿到的嵌入数据不对: %s", rec.Body.String())
	}
}

// 契约第三节：race 分组的渠道兼容判定同样改走能力位。
// 空 Capabilities 的存量渠道行为不变（嵌入渠道仍进组，chat 渠道仍不进组）。
func TestGroupRaceChannelCompatibleUsesCapabilityBit(t *testing.T) {
	raceChannel := func(channelType outbound.OutboundType, capabilities ...string) *dbmodel.Channel {
		return &dbmodel.Channel{ID: 1, Name: "c", Enabled: true, RaceMode: true, Type: channelType, Capabilities: capabilities}
	}

	if !groupRaceChannelCompatible(newEmbeddingInternalRequest("m"), raceChannel(outbound.OutboundTypeOpenAIEmbedding)) {
		t.Fatalf("存量嵌入渠道(type 5)必须仍能进入 race 分组")
	}
	if groupRaceChannelCompatible(newEmbeddingInternalRequest("m"), raceChannel(outbound.OutboundTypeOpenAIResponse)) {
		t.Fatalf("没填能力位的 chat 型渠道不得凭空获得嵌入流量")
	}
	if !groupRaceChannelCompatible(newEmbeddingInternalRequest("m"), raceChannel(outbound.OutboundTypeOpenAIResponse, dbmodel.CapabilityEmbeddings)) {
		t.Fatalf("填了 embeddings 能力位的 chat 型渠道必须能进入嵌入请求的 race 分组")
	}
	if !groupRaceChannelCompatible(newChatInternalRequest("m"), raceChannel(outbound.OutboundTypeOpenAIResponse)) {
		t.Fatalf("存量 chat 渠道的 race 兼容判定不得变化")
	}
}

func newChatInternalRequest(modelName string) *model.InternalLLMRequest {
	content := "你好"
	return &model.InternalLLMRequest{
		Model: modelName,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
		RawAPIFormat: model.APIFormatOpenAIChatCompletion,
	}
}

// 契约第四节：渠道类型决定"能接什么"，不决定"嵌入报文长什么样"。
// 一个 chat 型渠道勾上 embeddings 能力位后，嵌入请求必须交给通用
// EmbeddingOutbound，而不是该渠道的出厂适配器 ResponseOutbound —— 后者会把嵌入请求
// 序列化成 responses body，发出去的东西是错的。
func TestOutboundAdapterForEmbeddingRequestUsesEmbeddingOutbound(t *testing.T) {
	adapter := outboundAdapterFor(newEmbeddingInternalRequest("doubao-embedding"), outbound.OutboundTypeOpenAIResponse)
	if _, ok := adapter.(*openaiOutbound.EmbeddingOutbound); !ok {
		t.Fatalf("嵌入请求 + chat 型渠道选出的适配器 = %T, want *openai.EmbeddingOutbound", adapter)
	}
	if _, wrong := adapter.(*openaiOutbound.ResponseOutbound); wrong {
		t.Fatalf("嵌入请求绝不能落到 ResponseOutbound")
	}
}

// 存量行为零变化：type 5 的嵌入渠道本来就该拿到 EmbeddingOutbound。
func TestOutboundAdapterForEmbeddingChannelTypeUnchanged(t *testing.T) {
	adapter := outboundAdapterFor(newEmbeddingInternalRequest("text-embedding-3-small"), outbound.OutboundTypeOpenAIEmbedding)
	if _, ok := adapter.(*openaiOutbound.EmbeddingOutbound); !ok {
		t.Fatalf("type 5 嵌入渠道选出的适配器 = %T, want *openai.EmbeddingOutbound", adapter)
	}
}

// 存量行为零变化：chat 请求仍然按渠道类型取出厂适配器，一个字节都不改。
func TestOutboundAdapterForChatRequestKeepsChannelTypeAdapter(t *testing.T) {
	cases := []struct {
		channelType outbound.OutboundType
		want        any
	}{
		{outbound.OutboundTypeOpenAIResponse, &openaiOutbound.ResponseOutbound{}},
		{outbound.OutboundTypeOpenAIChat, &openaiOutbound.ChatOutbound{}},
		{outbound.OutboundTypeOpenAIEmbedding, &openaiOutbound.EmbeddingOutbound{}},
	}
	for _, tt := range cases {
		adapter := outboundAdapterFor(newChatInternalRequest("glm-4.6"), tt.channelType)
		if got, want := fmt.Sprintf("%T", adapter), fmt.Sprintf("%T", tt.want); got != want {
			t.Fatalf("chat 请求 + 渠道类型 %d 选出 %s, want %s", tt.channelType, got, want)
		}
	}
}

// 没有出厂适配器的类型仍然返回 nil，让调用点既有的 "unsupported channel type" 跳过
// 逻辑继续生效（嵌入请求也不许把它"救"成一个可用的适配器）。
func TestOutboundAdapterForUnknownChannelTypeStaysNil(t *testing.T) {
	unknownType := outbound.OutboundType(99)
	if adapter := outboundAdapterFor(newEmbeddingInternalRequest("m"), unknownType); adapter != nil {
		t.Fatalf("未知渠道类型的嵌入请求返回了 %T, want nil", adapter)
	}
	if adapter := outboundAdapterFor(nil, outbound.OutboundTypeOpenAIChat); adapter == nil {
		t.Fatalf("nil 请求必须原样返回渠道类型对应的适配器")
	}
}

// 端到端形态证据：用选出来的适配器真正构建一次出站请求，body 里必须是嵌入字段
// （model + input），不能有 responses/chat 的字段。
func TestEmbeddingOutboundSelectionProducesEmbeddingBody(t *testing.T) {
	internalRequest := newEmbeddingInternalRequest("doubao-embedding-text-240715")
	adapter := outboundAdapterFor(internalRequest, outbound.OutboundTypeOpenAIResponse)

	outboundRequest, err := adapter.TransformRequest(context.Background(), internalRequest, "https://ark.example.com/api/v3", "sk-test")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(outboundRequest.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body %q: %v", body, err)
	}
	if _, ok := payload["input"]; !ok {
		t.Fatalf("嵌入 body 必须带 input, got %v", payload)
	}
	for _, forbidden := range []string{"messages", "instructions", "max_output_tokens", "store"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("嵌入 body 里出现了 %q —— 这是 responses/chat 的字段: %v", forbidden, payload)
		}
	}
	if got, want := outboundRequest.URL.Path, "/api/v3/v1/embeddings"; got != want {
		t.Fatalf("未配自定义嵌入路径时 URL path = %q, want %q", got, want)
	}
}

// 渠道配了 openai_embeddings_path 时，relay 的 base url 必须换成嵌入专属路径，
// 出站打到上游真实端点（/api/v3/embeddings，而不是 /api/v3/v1/embeddings）。
func TestOutboundBaseURLUsesEmbeddingsPathForEmbeddingRequests(t *testing.T) {
	channel := &dbmodel.Channel{
		Type:                 outbound.OutboundTypeOpenAIResponse,
		BaseUrls:             []dbmodel.BaseUrl{{URL: "https://ark.example.com/api/v3"}},
		OpenAIEmbeddingsPath: "/embeddings",
	}
	embeddingAttempt := &relayAttempt{
		relayRequest: &relayRequest{internalRequest: newEmbeddingInternalRequest("doubao-embedding")},
		channel:      channel,
	}
	if got, want := embeddingAttempt.outboundBaseURL(), "https://ark.example.com/api/v3/embeddings"; got != want {
		t.Fatalf("嵌入请求的 base url = %q, want %q", got, want)
	}

	// 一路走到报文：配了自定义路径时，出站必须打在上游真实端点上，
	// 不能是 JoinOpenAIPath 拼出来的 /api/v3/v1/embeddings（那是个不存在的地址）。
	adapter := outboundAdapterFor(embeddingAttempt.internalRequest, channel.Type)
	outboundRequest, err := adapter.TransformRequest(context.Background(), embeddingAttempt.internalRequest, embeddingAttempt.outboundBaseURL(), "sk-test")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if got, want := outboundRequest.URL.String(), "https://ark.example.com/api/v3/embeddings"; got != want {
		t.Fatalf("自定义嵌入路径的最终出站 URL = %q, want %q", got, want)
	}

	// chat 请求仍走 GetBaseUrl()，与改动前一致。
	chatAttempt := &relayAttempt{
		relayRequest: &relayRequest{internalRequest: newChatInternalRequest("doubao-pro")},
		channel:      channel,
	}
	if got, want := chatAttempt.outboundBaseURL(), "https://ark.example.com/api/v3"; got != want {
		t.Fatalf("chat 请求的 base url = %q, want %q", got, want)
	}

	// 没配嵌入路径的存量渠道：嵌入与 chat 都取 GetBaseUrl()。
	legacyChannel := &dbmodel.Channel{
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		BaseUrls: []dbmodel.BaseUrl{{URL: "https://api.openai.com/v1"}},
	}
	legacyAttempt := &relayAttempt{
		relayRequest: &relayRequest{internalRequest: newEmbeddingInternalRequest("text-embedding-3-small")},
		channel:      legacyChannel,
	}
	if got, want := legacyAttempt.outboundBaseURL(), "https://api.openai.com/v1"; got != want {
		t.Fatalf("存量嵌入渠道的 base url = %q, want %q", got, want)
	}
}
