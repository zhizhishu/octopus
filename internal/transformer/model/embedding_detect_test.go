package model

import "testing"

// 🔴 普通 chat 请求带上 embedding_input 字段，绝不能被判成 embedding 请求。
//
// 事故面是真的、实测复现过：入站 chat 适配器把整个 body 直接 json.Unmarshal
// 进 InternalLLMRequest，而 EmbeddingInput.UnmarshalJSON 对任何非 null JSON
// 都照单全收（塞进 Raw，不校验形状）。于是客户端只要在 chat body 里多带一个
// 顶层 "embedding_input": 1，就会让 EmbeddingInput != nil，接着：
//
//	relay.go:419  所有 chat 渠道被跳过（"channel type not compatible with embedding request"）
//	model.go:364  Validate 报 "cannot specify both messages and input"
//
// 一条正常请求因此完全不可用，而且客户端能单方面触发——这是拒绝服务面。
func TestChatBodyWithEmbeddingInputIsNotEmbeddingRequest(t *testing.T) {
	req := &InternalLLMRequest{
		Model:          "gpt-4o",
		RawAPIFormat:   APIFormatOpenAIChatCompletion,
		Messages:       []Message{{Role: "user", Content: MessageContent{Content: strPtrDetect("hi")}}},
		EmbeddingInput: &EmbeddingInput{},
	}
	if req.IsEmbeddingRequest() {
		t.Error("带 messages 的 chat 端点请求不该被判成 embedding 请求")
	}
	if !req.IsChatRequest() {
		t.Error("它应该仍是 chat 请求")
	}
}

// 真正的 embedding 请求要靠端点标记识别，不靠字段。
func TestEmbeddingInboundMarkerWins(t *testing.T) {
	req := &InternalLLMRequest{
		Model:          "text-embedding-3-small",
		RawAPIFormat:   APIFormatOpenAIEmbedding,
		EmbeddingInput: &EmbeddingInput{},
	}
	if !req.IsEmbeddingRequest() {
		t.Error("embedding 端点请求应被判成 embedding 请求")
	}
}

// 没设端点标记时（内部构造、探针、测试）回落到字段判断，保持向后兼容。
func TestUnmarkedRequestFallsBackToField(t *testing.T) {
	embedding := &InternalLLMRequest{EmbeddingInput: &EmbeddingInput{}}
	if !embedding.IsEmbeddingRequest() {
		t.Error("未设标记但有 EmbeddingInput 时，应仍判为 embedding（兼容内部构造）")
	}

	// 但带了 messages 就一定不是——宁可让 chat 走通。
	both := &InternalLLMRequest{
		Messages:       []Message{{Role: "user"}},
		EmbeddingInput: &EmbeddingInput{},
	}
	if both.IsEmbeddingRequest() {
		t.Error("同时带 messages 时不该判为 embedding")
	}
}

func TestNilRequestIsNotEmbedding(t *testing.T) {
	var r *InternalLLMRequest
	if r.IsEmbeddingRequest() {
		t.Error("nil 请求不该判为 embedding")
	}
}

func strPtrDetect(s string) *string { return &s }
