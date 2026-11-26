package modeltest

import (
	"encoding/json"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// RelaySender 的输出上限与请求形状（用户 2026-11-26 拍板的口径）：
//   - Anthropic 入站必须带 max_tokens=32000（协议必填 + 检测要完整输出）；
//   - OpenAI/Gemini 类不强夹 max_tokens（Ask.MaxTokens 显式给了才带）；
//   - 绝不回落 defaultProbeMaxTokens=256。
// 本文件静态编写，未在 Windows 本机运行（环境约束禁止 go test）。

func relayShapeSender(inboundType inbound.InboundType) *RelaySender {
	return &RelaySender{
		channel:      dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
		requestModel: "zai-glm-5-3",
		inboundType:  inboundType,
	}
}

func TestRelayRequestAnthropicCarries32000(t *testing.T) {
	s := relayShapeSender(inbound.InboundTypeAnthropic)
	spec, err := s.buildRelayRequest(modelverify.Ask{Prompt: "画一只鹈鹕骑着自行车"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if spec.path != "/v1/messages" {
		t.Fatalf("path = %s", spec.path)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(spec.body, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	var maxTokens int64
	if err := json.Unmarshal(body["max_tokens"], &maxTokens); err != nil {
		t.Fatalf("max_tokens: %v", err)
	}
	if maxTokens != relayAnthropicMaxTokens || relayAnthropicMaxTokens != 32000 {
		t.Fatalf("max_tokens = %d, want 32000（不能夹到 256）", maxTokens)
	}
	var stream bool
	if err := json.Unmarshal(body["stream"], &stream); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if stream {
		t.Fatal("inbound request must be non-stream（relay 聚合后一次性返回）")
	}
}

func TestRelayRequestOpenAIOmitsMaxTokens(t *testing.T) {
	s := relayShapeSender(inbound.InboundTypeOpenAIChat)
	spec, err := s.buildRelayRequest(modelverify.Ask{Prompt: "hi"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(spec.body, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatal("openai chat request must not clamp max_tokens")
	}
	// 显式预算才透传（glitch/numberdist 探针靠它控制复述长度）。
	budget := 1536
	spec, err = s.buildRelayRequest(modelverify.Ask{Prompt: "hi", MaxTokens: &budget})
	if err != nil {
		t.Fatalf("build with budget: %v", err)
	}
	var with map[string]json.RawMessage
	if err := json.Unmarshal(spec.body, &with); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	var got int64
	if err := json.Unmarshal(with["max_tokens"], &got); err != nil || got != 1536 {
		t.Fatalf("max_tokens = %d err=%v, want 1536", got, err)
	}
}

func TestRelayRequestTurnsReplayThinkingBlocks(t *testing.T) {
	s := relayShapeSender(inbound.InboundTypeAnthropic)
	spec, err := s.buildRelayRequest(modelverify.Ask{
		Turns: []modelverify.Turn{
			{Role: "user", Content: "为什么"},
			{Role: "assistant", Thinking: "推理", Signature: "sig=="},
			{Role: "user", Content: "复述推理"},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(spec.body, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(body.Messages))
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(body.Messages[1].Content, &blocks); err != nil {
		t.Fatalf("assistant content must be thinking blocks: %v", err)
	}
	if string(blocks[0]["type"]) != `"thinking"` || string(blocks[0]["signature"]) != `"sig=="` {
		t.Fatalf("thinking block = %v", blocks[0])
	}
}

func TestRelaySenderAdmissionErrors(t *testing.T) {
	// 缺 base url / 缺模型的渠道在发送前就该被拦下（错误口径与 NewProbeSender
	// 一致，供 worker 区分「确定性失败」与「可重试」）。
	chat := dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat}
	if _, err := NewRelaySender(chat, "gpt-6.2", "openai_chat"); err == nil {
		t.Fatal("channel without base url must be rejected")
	}
	anthropic := dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}
	if _, err := NewRelaySender(anthropic, "", "anthropic_messages"); err == nil {
		t.Fatal("empty model must be rejected")
	}
	if _, err := NewRelaySender(anthropic, "claude-x", "systemone"); err == nil {
		t.Fatal("systemone endpoint must be refused by the relay sender")
	}
}
