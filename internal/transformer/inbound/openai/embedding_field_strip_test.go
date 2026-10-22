package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 🔴 端到端：chat 端点收到夹带 embedding 字段的 body，必须能正常走通。
//
// 审计抓到我只改了 IsEmbeddingRequest 就收工，但真实失败面在别处：
// Validate 自己重算了一遍旧判据（`EmbeddingInput != nil`），于是请求在选渠前
// 就被 400 拒掉——修的是一半。这一组测试走完整入站路径，专门盯住那个缺口。
func TestChatInboundDropsEmbeddingFields(t *testing.T) {
	// 客户端能单方面发出来的最坏情况：一条合法的 chat 请求 + 一个多余的
	// embedding 字段，值随便给（UnmarshalJSON 对任何非 null JSON 都收）。
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` +
		`"embedding_input":1,"dimensions":1024,"encoding_format":"float"}`)

	req, err := (&ChatInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("入站解析不该失败: %v", err)
	}

	if req.EmbeddingInput != nil {
		t.Error("chat 入站必须丢掉 EmbeddingInput")
	}
	if req.EmbeddingDimensions != nil {
		t.Error("chat 入站必须丢掉 EmbeddingDimensions")
	}
	if req.EmbeddingEncodingFormat != nil {
		t.Error("chat 入站必须丢掉 EmbeddingEncodingFormat")
	}
	if req.IsEmbeddingRequest() {
		t.Error("清理后不该再被判为 embedding 请求")
	}

	// 真正要紧的一步：Validate 必须放行。旧代码在这里报
	// "cannot specify both messages and input"，整条链路 400。
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate 不该拒绝这条请求，实际: %v", err)
	}

	// 出站会把整个结构体 json.Marshal 给上游，字段必须真的不在报文里。
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}
	for _, field := range []string{"embedding_input", "dimensions", "encoding_format"} {
		if v, ok := wire[field]; ok && string(v) != "null" {
			t.Errorf("出站报文不该含 %q，实际: %s", field, v)
		}
	}
}

// 真正的 embedding 请求必须原样保留，别被上面的清理误伤。
func TestEmbeddingInboundKeepsItsFields(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello",` +
		`"dimensions":256,"encoding_format":"float"}`)

	req, err := (&EmbeddingInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("入站解析失败: %v", err)
	}
	if !req.IsEmbeddingRequest() {
		t.Fatal("真正的 embedding 请求必须仍被判为 embedding")
	}
	if req.EmbeddingDimensions == nil || *req.EmbeddingDimensions != 256 {
		t.Error("dimensions 必须原样保留")
	}
	if req.EmbeddingEncodingFormat == nil || *req.EmbeddingEncodingFormat != "float" {
		t.Error("encoding_format 必须原样保留")
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("合法 embedding 请求不该被 Validate 拒: %v", err)
	}
}

// Validate 与 IsEmbeddingRequest 必须是同一套判据，不能各自推导。
func TestValidateAgreesWithIsEmbeddingRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *model.InternalLLMRequest
	}{
		{
			name: "chat 带 messages 无 embedding 字段",
			req: &model.InternalLLMRequest{
				Model:        "gpt-4o",
				RawAPIFormat: model.APIFormatOpenAIChatCompletion,
				Messages:     []model.Message{{Role: "user"}},
			},
		},
		{
			name: "正牌 embedding",
			req: &model.InternalLLMRequest{
				Model:        "text-embedding-3-small",
				RawAPIFormat: model.APIFormatOpenAIEmbedding,
				// 必须有实际输入：空的 EmbeddingInput 本来就该被
				// "input cannot be empty" 拒掉，那是正确行为。
				EmbeddingInput: &model.EmbeddingInput{Single: strPtrStrip("hello")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEmbedding := tc.req.IsEmbeddingRequest()
			// Validate 里对 embedding 的判定若与 IsEmbeddingRequest 不一致，
			// 两者组合出来的行为必然有一边是错的。这里用「同时有 messages」
			// 这个只有旧判据才会报错的组合来探。
			if gotEmbedding && len(tc.req.Messages) > 0 {
				t.Fatalf("IsEmbeddingRequest 与 messages 同时为真，判据分裂")
			}
			if err := tc.req.Validate(); err != nil {
				t.Fatalf("Validate 拒绝了一条自洽的请求: %v", err)
			}
		})
	}
}

func strPtrStrip(s string) *string { return &s }
