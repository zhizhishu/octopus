package modeltest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/volcengine"
)

func volcengineEmbeddingChannel() dbmodel.Channel {
	return dbmodel.Channel{
		Name:         "volc-embed",
		Type:         outbound.OutboundTypeVolcengine,
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: "https://ark.example.com/api/v3"}},
		Keys:         []dbmodel.ChannelKey{{ID: 1, Enabled: true, ChannelKey: "sk-test"}},
		Capabilities: []string{dbmodel.CapabilityEmbeddings},
	}
}

// 契约第四节之二：渠道检测里同一个坑 —— 嵌入端点必须换 adapter，否则一个勾了
// embeddings 的火山渠道会拿嵌入请求去序列化成 responses body。
// 存量之所以没暴雷，是因为嵌入渠道必然是 type 5，outbound.Get(5) 正好是
// EmbeddingOutbound；能力位放开后这个巧合就没了。
func TestModelTestOutboundAdapterSwapsEmbeddingEndpoint(t *testing.T) {
	adapter := modelTestOutboundAdapter(outbound.OutboundTypeVolcengine, embeddingTestEndpointName)
	if _, ok := adapter.(*openaiOutbound.EmbeddingOutbound); !ok {
		t.Fatalf("嵌入端点在火山渠道上选出 %T, want *openai.EmbeddingOutbound", adapter)
	}
	if _, wrong := adapter.(*volcengine.ResponseOutbound); wrong {
		t.Fatalf("嵌入端点绝不能落到 volcengine.ResponseOutbound")
	}

	// 别名端点同样要换（normalizeEndpoint 认 "embeddings"）。
	if _, ok := modelTestOutboundAdapter(outbound.OutboundTypeVolcengine, "embeddings").(*openaiOutbound.EmbeddingOutbound); !ok {
		t.Fatalf("别名端点 embeddings 也必须换到 EmbeddingOutbound")
	}

	// 存量行为零变化：chat 端点仍然按渠道类型取出厂适配器。
	chatAdapter := modelTestOutboundAdapter(outbound.OutboundTypeVolcengine, "openai_chat")
	if got, want := fmt.Sprintf("%T", chatAdapter), "*volcengine.ResponseOutbound"; got != want {
		t.Fatalf("chat 端点 + 火山渠道选出 %s, want %s", got, want)
	}
	// 空端点按默认端点（openai_chat）处理，与 NewProbeSender 的默认语义一致。
	if got, want := fmt.Sprintf("%T", modelTestOutboundAdapter(outbound.OutboundTypeVolcengine, "")), "*volcengine.ResponseOutbound"; got != want {
		t.Fatalf("空端点默认不是嵌入端点: got %s, want %s", got, want)
	}

	// 没有出厂适配器的类型仍然返回 nil（既有 "unsupported channel type" 跳过逻辑不变）。
	if adapter := modelTestOutboundAdapter(outbound.OutboundType(99), embeddingTestEndpointName); adapter != nil {
		t.Fatalf("未知渠道类型返回 %T, want nil", adapter)
	}
}

// 探针侧：勾了 embeddings 能力位的火山渠道 + 嵌入端点，适配器必须是 EmbeddingOutbound。
func TestNewProbeSenderSelectsEmbeddingOutboundForCapabilityChannel(t *testing.T) {
	sender, err := NewProbeSender(volcengineEmbeddingChannel(), "doubao-embedding", embeddingTestEndpointName)
	if err != nil {
		t.Fatalf("NewProbeSender: %v", err)
	}
	if _, ok := sender.adapter.(*openaiOutbound.EmbeddingOutbound); !ok {
		t.Fatalf("探针适配器 = %T, want *openai.EmbeddingOutbound", sender.adapter)
	}
}

// 契约第三节：探针准入门的嵌入那半改走能力位。
// 存量兼容分支由 model 层的 SupportsCapability 保证（type 5 仍准入）；这里是另一半：
// 一个「不是 chat 类型」但声明了 embeddings 能力位的渠道必须能进到适配器解析这一步。
func TestNewProbeSenderGateUsesCapabilityBit(t *testing.T) {
	channel := dbmodel.Channel{
		Name:         "capability-only",
		Type:         outbound.OutboundType(99),
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: "https://embed.example.test"}},
		Keys:         []dbmodel.ChannelKey{{ID: 1, Enabled: true, ChannelKey: "sk-test"}},
		Capabilities: []string{dbmodel.CapabilityEmbeddings},
	}
	_, err := NewProbeSender(channel, "embed-model", embeddingTestEndpointName)
	if err == nil {
		t.Fatalf("type 99 没有出厂适配器，必须报错")
	}
	if strings.Contains(err.Error(), "不支持探针校验") {
		t.Fatalf("声明了 embeddings 能力位的渠道不该被准入门拒掉，got %v", err)
	}

	// 没有能力位、也不是 chat 类型的 channel type 仍然被准入门拒掉。
	// 断言"被拒"本身，不断言用哪句话拒 —— 理由文案会随能力位语义演进
	// （现在是"未声明 embedding 能力"），但"不许进"这个契约不变。
	plain := channel
	plain.Capabilities = nil
	if _, err := NewProbeSender(plain, "embed-model", embeddingTestEndpointName); err == nil {
		t.Fatalf("无能力位的未知类型必须仍被准入门拒掉，got nil error")
	}
}

// 端到端形态证据：火山渠道 + 嵌入端点，选出来的适配器构建出的 body 只有嵌入字段。
func TestEmbeddingEndpointOnVolcengineChannelBuildsEmbeddingBody(t *testing.T) {
	channel := volcengineEmbeddingChannel()
	runner := &modelRunner{request: dbmodel.ModelTestRequest{
		Endpoint: embeddingTestEndpointName,
		Prompt:   "请只回答算式结果，不要解释：1 + 1 = ?",
	}}
	internalRequest := runner.internalRequest("doubao-embedding")
	adapter := modelTestOutboundAdapter(channel.Type, runner.request.Endpoint)

	outboundRequest, err := adapter.TransformRequest(context.Background(), internalRequest, modelTestOutboundBaseURL(&channel, internalRequest), "sk-test")
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
	for _, forbidden := range []string{"messages", "instructions", "max_output_tokens"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("嵌入 body 里出现了 %q —— 这是 responses 的字段: %v", forbidden, payload)
		}
	}
}

// 整条检测链路（RunChannel → tryChannel → testChannelKey → 出站）在「能力位渠道 +
// 嵌入端点」上必须成功，且上游收到的必须是嵌入报文：这正是能力位放开后最容易暴雷的
// 那条路 —— adapter 不纠正就会把嵌入请求序列化成 responses body 发出去。
func TestRunChannelEmbeddingProbeOnCapabilityChannel(t *testing.T) {
	ctx := setupModelTestDB(t)

	var sawPath, sawInput string
	var sawMessages, sawInstructions, sawContentType bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawContentType = r.Header.Get("Content-Type") == "application/json"
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if input, ok := payload["input"].(string); ok {
			sawInput = input
		}
		_, sawMessages = payload["messages"]
		_, sawInstructions = payload["instructions"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"emb-volc",
			"object":"list",
			"created":123,
			"model":"doubao-embedding",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3,0.4]}],
			"usage":{"prompt_tokens":5,"total_tokens":5}
		}`))
	}))
	t.Cleanup(upstream.Close)

	response, err := RunChannel(ctx, dbmodel.Channel{
		Name:                 "volc-embedding-capability",
		Type:                 outbound.OutboundTypeVolcengine,
		Enabled:              false,
		BaseUrls:             []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:                 []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "embed-key"}},
		Capabilities:         []string{dbmodel.CapabilityEmbeddings},
		OpenAIEmbeddingsPath: "/embeddings",
	}, dbmodel.ModelTestRequest{
		Model:    "doubao-embedding",
		Endpoint: embeddingTestEndpointName,
	})
	if err != nil {
		t.Fatalf("run channel test: %v", err)
	}
	if response.Summary.Success != 1 {
		t.Fatalf("能力位渠道的嵌入检测必须成功, got %#v", response)
	}
	result := response.Results[0]
	if !result.Success {
		t.Fatalf("unexpected result: %#v", result)
	}
	// 自定义路径 /embeddings 直接挂在 base url 后面（火山 /api/v3/embeddings 同形），
	// 不能被拼成 /v1/embeddings。
	if sawPath != "/embeddings" {
		t.Fatalf("upstream saw path %q, want /embeddings", sawPath)
	}
	if result.UpstreamPath != "/embeddings" {
		t.Fatalf("upstream path = %q, want /embeddings", result.UpstreamPath)
	}
	if !sawContentType {
		t.Fatalf("嵌入请求必须带 Content-Type: application/json")
	}
	if sawInput == "" {
		t.Fatalf("上游没有收到非空 input")
	}
	if sawMessages || sawInstructions {
		t.Fatalf("嵌入报文里出现了 chat/responses 字段: messages=%v instructions=%v", sawMessages, sawInstructions)
	}
	if result.ResponsePreview != "1 embedding(s) x 4 dims" {
		t.Fatalf("response preview = %q, want the vector summary", result.ResponsePreview)
	}
}

// 契约第三节：检测准入门的嵌入那半改走能力位（runner 侧）。
// 一个「不是 chat 类型」但声明了 embeddings 能力位的渠道必须越过准入门，
// 停在下游的适配器解析上，而不是被 "does not support model connectivity tests" 挡掉。
func TestRunChannelGateUsesCapabilityBit(t *testing.T) {
	ctx := setupModelTestDB(t)

	blocked, err := RunChannel(ctx, dbmodel.Channel{
		Name:     "plain-unknown-type",
		Type:     outbound.OutboundType(99),
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: "https://embed.example.test"}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
	}, dbmodel.ModelTestRequest{Model: "m", Endpoint: embeddingTestEndpointName})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if len(blocked.Results[0].Attempts) == 0 || blocked.Results[0].Attempts[0].Status != dbmodel.AttemptSkipped {
		t.Fatalf("无能力位的未知类型必须被准入门挡掉, got %#v", blocked.Results[0].Attempts)
	}

	allowed, err := RunChannel(ctx, dbmodel.Channel{
		Name:         "capability-only",
		Type:         outbound.OutboundType(99),
		Enabled:      false,
		BaseUrls:     []dbmodel.BaseUrl{{URL: "https://embed.example.test"}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		Capabilities: []string{dbmodel.CapabilityEmbeddings},
	}, dbmodel.ModelTestRequest{Model: "m", Endpoint: embeddingTestEndpointName})
	if err != nil {
		t.Fatalf("RunChannel: %v", err)
	}
	if len(allowed.Results[0].Attempts) == 0 {
		t.Fatalf("能力位渠道必须产生一次尝试记录, got %#v", allowed.Results[0])
	}
	if msg := allowed.Results[0].Attempts[0].Msg; strings.Contains(msg, "does not support model connectivity tests") {
		t.Fatalf("声明了 embeddings 能力位的渠道不该被准入门挡掉, got %q", msg)
	}
}

// 检测与真实转发同一条链路：渠道配了 openai_embeddings_path 时，检测的 base url
// 必须与 relay 的 outboundBaseURL 一致。
func TestModelTestOutboundBaseURLUsesEmbeddingsPath(t *testing.T) {
	channel := volcengineEmbeddingChannel()
	channel.OpenAIEmbeddingsPath = "/embeddings"
	embeddingRequest := &transformermodel.InternalLLMRequest{
		Model:          "doubao-embedding",
		EmbeddingInput: &transformermodel.EmbeddingInput{},
		RawAPIFormat:   transformermodel.APIFormatOpenAIEmbedding,
	}
	if got, want := modelTestOutboundBaseURL(&channel, embeddingRequest), "https://ark.example.com/api/v3/embeddings"; got != want {
		t.Fatalf("嵌入请求的检测 base url = %q, want %q", got, want)
	}

	prompt := "hi"
	chatRequest := &transformermodel.InternalLLMRequest{Model: "doubao-pro"}
	chatRequest.Messages = []transformermodel.Message{{
		Role:    "user",
		Content: transformermodel.MessageContent{Content: &prompt},
	}}
	chatRequest.RawAPIFormat = transformermodel.APIFormatOpenAIChatCompletion
	if got, want := modelTestOutboundBaseURL(&channel, chatRequest), "https://ark.example.com/api/v3"; got != want {
		t.Fatalf("chat 请求的检测 base url = %q, want %q", got, want)
	}
}
