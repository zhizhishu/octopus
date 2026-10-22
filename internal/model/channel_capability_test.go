package model

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// 能力位的核心契约：**没填 Capabilities 的渠道行为必须与改动前完全一致**。
// 这一组用例就是那条安全带——它们红了意味着存量渠道的嵌入准入被改动了。
func TestSupportsCapabilityEmptyCapabilitiesFallsBackToChannelType(t *testing.T) {
	embeddingChannel := &Channel{Type: outbound.OutboundTypeOpenAIEmbedding}
	if !embeddingChannel.SupportsCapability(CapabilityEmbeddings) {
		t.Fatalf("存量嵌入渠道(type 5)必须继续承接 embeddings")
	}

	for _, channelType := range []outbound.OutboundType{
		outbound.OutboundTypeOpenAIChat,
		outbound.OutboundTypeOpenAIResponse,
		outbound.OutboundTypeAnthropic,
		outbound.OutboundTypeGemini,
		outbound.OutboundTypeVolcengine,
		outbound.OutboundTypeCustomOpenAIChat,
	} {
		channel := &Channel{Type: channelType}
		if channel.SupportsCapability(CapabilityEmbeddings) {
			t.Fatalf("没填 Capabilities 的 chat 渠道(type %d)不得凭空获得 embeddings", channelType)
		}
	}
}

// nil 与 []string{} 必须同义：都走「按渠道类型判定」的存量分支。
func TestSupportsCapabilityNilAndEmptySliceAgree(t *testing.T) {
	cases := []outbound.OutboundType{
		outbound.OutboundTypeOpenAIEmbedding,
		outbound.OutboundTypeOpenAIChat,
		outbound.OutboundTypeVolcengine,
	}
	for _, channelType := range cases {
		nilCaps := &Channel{Type: channelType}
		emptyCaps := &Channel{Type: channelType, Capabilities: []string{}}
		if got, want := emptyCaps.SupportsCapability(CapabilityEmbeddings), nilCaps.SupportsCapability(CapabilityEmbeddings); got != want {
			t.Fatalf("type %d: empty slice = %v, nil = %v; they must agree", channelType, got, want)
		}
	}
}

func TestSupportsCapabilityDeclaredCapabilityWinsOverChannelType(t *testing.T) {
	volcengine := &Channel{
		Type:         outbound.OutboundTypeVolcengine,
		Capabilities: []string{CapabilityEmbeddings},
	}
	if !volcengine.SupportsCapability(CapabilityEmbeddings) {
		t.Fatalf("勾了 embeddings 能力位的火山渠道必须能承接嵌入流量")
	}

	chat := &Channel{
		Type:         outbound.OutboundTypeOpenAIChat,
		Capabilities: []string{CapabilityEmbeddings},
	}
	if !chat.SupportsCapability(CapabilityEmbeddings) {
		t.Fatalf("勾了 embeddings 能力位的 chat 渠道必须能承接嵌入流量")
	}

	// 显式清空 = 回到按渠道类型判定（存量语义），而不是「什么都没有」。
	explicitCapabilityEmbedding := &Channel{
		Type:         outbound.OutboundTypeOpenAIEmbedding,
		Capabilities: []string{"images"},
	}
	if explicitCapabilityEmbedding.SupportsCapability(CapabilityEmbeddings) {
		t.Fatalf("非空能力位列表必须被如实采用：只声明了 images 的渠道不得承接 embeddings")
	}
	if !explicitCapabilityEmbedding.SupportsCapability("images") {
		t.Fatalf("声明的 images 能力位必须生效")
	}
}

func TestSupportsCapabilityUnknownCapabilityIsFalse(t *testing.T) {
	for _, channel := range []*Channel{
		{Type: outbound.OutboundTypeOpenAIChat},
		{Type: outbound.OutboundTypeOpenAIEmbedding},
		{Type: outbound.OutboundTypeVolcengine, Capabilities: []string{CapabilityEmbeddings}},
	} {
		if channel.SupportsCapability("rerank") {
			t.Fatalf("未知能力串不得被判为支持 (channel type %d)", channel.Type)
		}
		if channel.SupportsCapability("") {
			t.Fatalf("空能力串不得被判为支持 (channel type %d)", channel.Type)
		}
	}
}

func TestSupportsCapabilityNilReceiverIsFalse(t *testing.T) {
	var channel *Channel
	if channel.SupportsCapability(CapabilityEmbeddings) {
		t.Fatalf("nil receiver 必须返回 false")
	}
	if channel.SupportsCapability("") {
		t.Fatalf("nil receiver 必须返回 false")
	}
}

// 嵌入自定义路径：空路径必须原样返回 base url（存量渠道零变化），
// 配了路径才动 URL。
func TestGetOpenAIEmbeddingsBaseUrl(t *testing.T) {
	base := []BaseUrl{{URL: "https://ark.example.com/api/v3"}}

	emptyPath := &Channel{BaseUrls: base}
	if got := emptyPath.GetOpenAIEmbeddingsBaseUrl(); got != "https://ark.example.com/api/v3" {
		t.Fatalf("未配路径必须原样返回 base url, got %q", got)
	}

	customPath := &Channel{BaseUrls: base, OpenAIEmbeddingsPath: "/embeddings"}
	if got := customPath.GetOpenAIEmbeddingsBaseUrl(); got != "https://ark.example.com/api/v3/embeddings" {
		t.Fatalf("自定义路径必须按 OpenAIChatPath 的拼法拼进 base url, got %q", got)
	}

	fullPath := &Channel{BaseUrls: []BaseUrl{{URL: "https://ark.example.com"}}, OpenAIEmbeddingsPath: "/api/v3/embeddings"}
	if got := fullPath.GetOpenAIEmbeddingsBaseUrl(); got != "https://ark.example.com/api/v3/embeddings" {
		t.Fatalf("整段自定义路径必须拼在裸 base url 后面, got %q", got)
	}

	absolute := &Channel{BaseUrls: base, OpenAIEmbeddingsPath: "https://other.example.com/custom/embeddings"}
	if got := absolute.GetOpenAIEmbeddingsBaseUrl(); got != "https://other.example.com/custom/embeddings" {
		t.Fatalf("绝对 URL 形态的自定义路径必须原样使用, got %q", got)
	}

	var nilChannel *Channel
	if got := nilChannel.GetOpenAIEmbeddingsBaseUrl(); got != "" {
		t.Fatalf("nil receiver 必须返回空串, got %q", got)
	}
}

// 存量渠道（type 5，未配自定义路径）最终发出去的嵌入 URL 必须与改动前逐字节一致。
//
// 改动前 EmbeddingOutbound 里是 xurl.JoinOpenAIPath(baseUrl, "/v1/embeddings")；
// 改动后是 xurl.JoinCustomOpenAIEmbeddingsPath(GetOpenAIEmbeddingsBaseUrl(), "/v1/embeddings")。
// 未配路径时 GetOpenAIEmbeddingsBaseUrl() == GetBaseUrl()，两条拼法必须给出同一个 URL。
func TestEmbeddingsTargetUrlUnchangedForChannelsWithoutCustomPath(t *testing.T) {
	bases := []string{
		"https://api.openai.com",
		"https://api.openai.com/v1",
		"https://api.openai.com/",
		"https://api.openai.com/v1/embeddings",
		"https://ark.example.com/api/v3",
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
	}
	for _, base := range bases {
		channel := &Channel{Type: outbound.OutboundTypeOpenAIEmbedding, BaseUrls: []BaseUrl{{URL: base}}}
		if got, want := channel.GetOpenAIEmbeddingsBaseUrl(), base; got != want {
			t.Fatalf("base url 必须原样透传(自定义路径为空): got %q, want %q", got, want)
		}
		before, err := xurl.JoinOpenAIPath(base, "/v1/embeddings")
		if err != nil {
			t.Fatalf("legacy join(%q): %v", base, err)
		}
		after, err := xurl.JoinCustomOpenAIEmbeddingsPath(channel.GetOpenAIEmbeddingsBaseUrl(), "/v1/embeddings")
		if err != nil {
			t.Fatalf("new join(%q): %v", base, err)
		}
		if after != before {
			t.Fatalf("base %q: 嵌入目标 URL 变了 —— 改动前 %q, 改动后 %q", base, before, after)
		}
	}
}

// 契约第五节的自定义路径形态：火山这类上游的嵌入端点是 /api/v3/embeddings。
// 用 JoinOpenAIPath 直拼会得到 /api/v3/v1/embeddings（错的），必须原样发出去。
func TestEmbeddingsCustomPathProducesVendorEndpoint(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{base: "https://ark.example.com", path: "/api/v3/embeddings", want: "https://ark.example.com/api/v3/embeddings"},
		{base: "https://ark.example.com/api/v3", path: "/embeddings", want: "https://ark.example.com/api/v3/embeddings"},
		{base: "https://proxy.example.com", path: "https://ark.example.com/api/v3/embeddings", want: "https://ark.example.com/api/v3/embeddings"},
	}
	for _, tt := range cases {
		channel := &Channel{
			Type:                 outbound.OutboundTypeVolcengine,
			BaseUrls:             []BaseUrl{{URL: tt.base}},
			OpenAIEmbeddingsPath: tt.path,
		}
		target, err := xurl.JoinCustomOpenAIEmbeddingsPath(channel.GetOpenAIEmbeddingsBaseUrl(), "/v1/embeddings")
		if err != nil {
			t.Fatalf("join(%q, %q): %v", tt.base, tt.path, err)
		}
		if target != tt.want {
			t.Fatalf("base %q + path %q: got %q, want %q", tt.base, tt.path, target, tt.want)
		}
	}
}
