package relay

import (
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 嵌入流量不许进入任何"CLI 形状"分支。
//
// 这三条测试守的是能力位改造引入的新风险面：改动前 EmbeddingChannelTypes 只有
// type 5，Anthropic / Responses / cloak 类渠道根本接不到嵌入请求，所以强制流式
// 与 CLI header 注入对它们无所谓。能力位放开后它们能接到了，若不在三条分支上
// 显式隔离，嵌入请求会：
//   ① 被强制 Stream=true → EmbeddingOutbound.TransformStream 直接报错
//      （embedding.go:122 "streaming is not supported for embedding API"）
//   ② 带上 claude/codex 的 CLI 指纹 header/TLS —— 嵌入没有 CLI 可言，且违反
//      "嵌入流量不碰指纹路径"的红线
//
// 删掉这些隔离，下面的测试必须变红；否则测试就是在测空气。

// ① Anthropic 渠道 + 开启 cloak：聊天请求会被强制流式，嵌入请求不许。
func TestEmbeddingRequestNeverForcedStreamByAnthropicCloak(t *testing.T) {
	channel := &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic, Cloak: dbmodel.ChannelCloak{Mode: "auto"}}

	// 前置条件：同样这个渠道，聊天请求必须被强制流式，否则下面的断言是空的。
	chatAttempt := newShapeAttempt(channel, &model.InternalLLMRequest{Model: "claude-sonnet-4-5", Stream: boolPtrFalse()})
	if !chatAttempt.shouldForceAnthropicStreamUpstream() {
		t.Fatalf("前置条件不成立：Anthropic+cloak 的聊天请求本应被强制流式")
	}

	embeddingAttempt := newShapeAttempt(channel, newEmbeddingInternalRequest("embedding-model"))
	if !embeddingAttempt.internalRequest.IsEmbeddingRequest() {
		t.Fatalf("测试夹具自身不自洽：构造出来的不是嵌入请求")
	}
	if embeddingAttempt.shouldForceAnthropicStreamUpstream() {
		t.Fatal("嵌入请求被 Anthropic cloak 强制流式：上游会返回 JSON，" +
			"handleStreamResponseAsNonStream 对非 SSE 直接判错")
	}
}

// ② Responses 渠道选出的 EmbeddingOutbound 不许触发 responses 强制流式。
func TestEmbeddingOutboundDoesNotForceResponsesStream(t *testing.T) {
	channel := &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse}
	adapter := outboundAdapterFor(newEmbeddingInternalRequest("embedding-model"), channel.Type)
	attempt := newShapeAttempt(channel, newEmbeddingInternalRequest("embedding-model"))
	if attempt.shouldForceOpenAIResponsesStreamUpstream(adapter) {
		t.Fatalf("嵌入流量被判定为需要 responses 强制流式：适配器=%T", adapter)
	}
}

// ③ 嵌入请求不许进 CLI header 注入分支：它必须落到通用默认，且不带任何指纹头。
func TestEmbeddingRequestSkipsCLIHeaderDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel *dbmodel.Channel
	}{
		{"anthropic", &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic, Cloak: dbmodel.ChannelCloak{Mode: "auto"}}},
		{"responses", &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse}},
		{"chat", &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Mode: "auto", Shape: "claude"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := tc.channel
			channel.BaseUrls = []dbmodel.BaseUrl{{URL: "https://upstream.example"}}
			attempt := newShapeAttempt(channel, newEmbeddingInternalRequest("embedding-model"))
			req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/embeddings", nil)
			if err != nil {
				t.Fatal(err)
			}
			attempt.applyHeaderDefaults(req)
			assertNoCLIIdentityHeaders(t, req.Header)
		})
	}
}

// newShapeAttempt 造一个只用于形状判定的 attempt。
// internalRequest 是 *relayRequest 的嵌入字段，Go 1.25 不允许在结构体字面量里直接写。
func newShapeAttempt(channel *dbmodel.Channel, req *model.InternalLLMRequest) *relayAttempt {
	return &relayAttempt{
		relayRequest: &relayRequest{internalRequest: req},
		channel:      channel,
	}
}

func boolPtrFalse() *bool { f := false; return &f }

// cliFingerprintHeaders 是 CLI 身份会留下的头。嵌入请求一个都不许带。
func assertNoCLIIdentityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	ua := h.Get("User-Agent")
	for _, needle := range []string{"claude-cli", "codex_cli_rs", "codex_exec", "codex-tui"} {
		if strings.Contains(ua, needle) {
			t.Fatalf("嵌入请求带上了 CLI 指纹 UA: %q", ua)
		}
	}
	for _, name := range []string{"originator", "x-codex-window-id", "anthropic-beta", "x-stainless-lang"} {
		if v := h.Get(name); v != "" {
			t.Fatalf("嵌入请求带上了 CLI 身份头 %s: %q", name, v)
		}
	}
}
