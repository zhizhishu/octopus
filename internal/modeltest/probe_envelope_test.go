package modeltest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 一组 Anthropic SSE：message_start 带上游响应 id，content_block_start 带 tool_use id，
// 另有正文与一个 Vertex 侧的 claude# 签名。
func anthropicEnvelopeSSE(t *testing.T) func(w http.ResponseWriter) {
	t.Helper()
	payloads := []string{
		`{"type":"message_start","message":{"id":"msg_01ABCdef","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":7,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tooluse_01xyz","name":"shell_command"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"claude#opaque"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	}
	blocks := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		blocks = append(blocks, anthropicSSE(t, payload))
	}
	body := strings.Join(blocks, "\n\n") + "\n\n"
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}
}

// Anthropic 出站：id / tool id / 签名形态都是上游原文，必须进信封。
func TestProbeAskFillsEnvelopeOnAnthropicChannel(t *testing.T) {
	ctx := setupModelTestDB(t)
	reply := anthropicEnvelopeSSE(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 头名足以判家族（x-amz* → aws），值不留。
		w.Header().Set("X-Amzn-Requestid", "abc-123")
		reply(w)
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "envelope-anthropic",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	stored, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}

	sender, err := NewProbeSender(*stored, "claude-opus-4-8", "anthropic_messages")
	if err != nil {
		t.Fatalf("probe sender: %v", err)
	}
	resp, err := sender.Ask(ctx, modelverify.Ask{Prompt: "hi"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	if resp.ResponseID != "msg_01ABCdef" {
		t.Fatalf("ResponseID = %q, want the upstream id", resp.ResponseID)
	}
	if resp.FirstToolCallID != "tooluse_01xyz" {
		t.Fatalf("FirstToolCallID = %q, want the upstream tool id", resp.FirstToolCallID)
	}
	if !resp.SignatureClaudeHash {
		t.Fatal("SignatureClaudeHash = false, want true for a claude# signature")
	}
	if resp.DeclaredModel != "claude-opus-4-8" {
		t.Fatalf("DeclaredModel = %q, want the upstream self-report", resp.DeclaredModel)
	}
	if !reflect.DeepEqual(resp.HeaderFamilies, []string{"aws"}) {
		t.Fatalf("HeaderFamilies = %v, want [aws]", resp.HeaderFamilies)
	}
	if !strings.Contains(resp.Content, "ok") {
		t.Fatalf("content = %q", resp.Content)
	}
}

// 非 Anthropic 出站：编号可能被适配器重写，一律不填；自报模型名与头家族照收
// （它们对所有协议都有意义）。
func TestProbeAskLeavesIDsEmptyOnNonAnthropicChannel(t *testing.T) {
	ctx := setupModelTestDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Amz-Request-Id", "aws-1")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "envelope-chat",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	stored, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}

	sender, err := NewProbeSender(*stored, "gpt-5.6", "openai_chat")
	if err != nil {
		t.Fatalf("probe sender: %v", err)
	}
	resp, err := sender.Ask(ctx, modelverify.Ask{Prompt: "hi"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	if resp.ResponseID != "" || resp.FirstToolCallID != "" || resp.SignatureClaudeHash {
		t.Fatalf("non-anthropic must not capture ids/signature: %+v", resp)
	}
	if resp.DeclaredModel != "gpt-5.6" {
		t.Fatalf("DeclaredModel = %q, want the chat completion model field", resp.DeclaredModel)
	}
	if !reflect.DeepEqual(resp.HeaderFamilies, []string{"aws"}) {
		t.Fatalf("HeaderFamilies = %v, want [aws] (family judgement is protocol-agnostic)", resp.HeaderFamilies)
	}
}

// 响应头家族只在 2xx 时记：错误页的头来自网关/前置层，不是模型后端的证据。
func TestHeaderFamiliesRecordedOnlyOn2xx(t *testing.T) {
	ctx := setupModelTestDB(t)
	adapter := outbound.Get(outbound.OutboundTypeOpenAIChat)
	if adapter == nil {
		t.Fatal("openai chat outbound adapter unavailable")
	}

	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{
			name:   "2xx",
			status: http.StatusOK,
			body:   "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			want:   []string{"aws"},
		},
		{
			name:   "5xx",
			status: http.StatusInternalServerError,
			body:   `{"error":{"message":"boom"}}`,
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Amz-Request-Id", "aws-1")
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(upstream.Close)

			channel := &dbmodel.Channel{
				Name:     "envelope-" + tc.name,
				Type:     outbound.OutboundTypeOpenAIChat,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
			}
			r := &modelRunner{request: dbmodel.ModelTestRequest{Endpoint: "openai_chat", Prompt: "hi"}}
			status, _, err := r.testChannelKey(ctx, adapter, channel, channel.Keys[0], "gpt-5.6")
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if tc.status == http.StatusOK && err != nil {
				t.Fatalf("2xx path failed: %v", err)
			}
			if tc.status != http.StatusOK && err == nil {
				t.Fatal("non-2xx path must return an error")
			}
			if !reflect.DeepEqual(r.lastHeaderFamilies, tc.want) {
				t.Fatalf("lastHeaderFamilies = %v, want %v", r.lastHeaderFamilies, tc.want)
			}
		})
	}
}

// 注入家族随伪装开关与形状变化：它决定复述探针要扣掉哪一家的文字。
func TestProbeSenderInjectedFamilies(t *testing.T) {
	cases := []struct {
		name    string
		channel dbmodel.Channel
		want    []string
	}{
		{
			name:    "anthropic channel (cloak default)",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
			want:    []string{"anthropic"},
		},
		{
			name:    "responses channel (cloak default)",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
			want:    []string{"openai"},
		},
		{
			name:    "chat channel shape claude",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}},
			want:    []string{"anthropic"},
		},
		{
			name:    "chat channel shape codex",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "codex"}},
			want:    []string{"openai"},
		},
		{
			name:    "chat channel without shape injects nothing",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
			want:    nil,
		},
		{
			name:    "cloak off injects nothing",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeAnthropic, Cloak: dbmodel.ChannelCloak{Mode: "never"}},
			want:    nil,
		},
		{
			name:    "cloak off ignores the shape",
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Mode: "never", Shape: "claude"}},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &ProbeSender{channel: tc.channel}
			if got := sender.InjectedFamilies(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("InjectedFamilies = %v, want %v", got, tc.want)
			}
		})
	}
}

// model-test 路径不会把渠道/接入方案的系统提示词覆盖加进探针请求：
// 那些层只在 relay 正向转发里应用（internal/relay/prompt_override.go 的
// applyPromptOverrides），所以 InjectedFamilies 不需要为它并家族。
// 这条用例钉的是「runner.go / testChannelKey 没有调用那套覆盖」。
func TestModelTestPathNeverAppliesPromptOverride(t *testing.T) {
	for _, banned := range []string{"applyPromptOverrides", "promptOverrideLayers"} {
		if strings.Contains(modelTestRunnerSource(t), banned) {
			t.Fatalf("runner.go 里出现了 %q：系统提示词覆盖会改变出站字节，必须由主管复核", banned)
		}
	}
}

// modelTestRunnerSource 读本包的 runner.go 源码（只看文本，不解析）。
func modelTestRunnerSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("read runner.go: %v", err)
	}
	return string(raw)
}
