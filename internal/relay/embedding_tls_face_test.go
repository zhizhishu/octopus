package relay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// 嵌入流量不许套任何 CLI 身份 —— header 和 TLS 握手**两条路都要隔离**。
//
// 【缺陷形态】曾经只有 header 那条路隔离了：`applyHeaderDefaults` 对
// IsEmbeddingRequest() 提前 return，只发通用头。但 `sendRequest` 里的
// `channelTLSFace(ra.channel)` 不认这个例外，照样按渠道类型套 CLI 面容。
// 能力位放开后 Anthropic/Responses 类渠道能承接嵌入流量，于是同一条请求
// 出现分裂形态：
//
//	请求头      = 通用（没有 claude/codex 身份）
//	ClientHello = 真 Claude Code CLI 的 Node/OpenSSL 指纹
//
// 这既无意义（嵌入没有 CLI 可言），又违反项目红线「嵌入流量不碰指纹路径」。
// 只隔离 header 等于没隔离 —— 指纹是从握手漏出去的。

func anthropicCloakedChannel() *dbmodel.Channel {
	return &dbmodel.Channel{
		ID:   1,
		Type: 2, // OutboundTypeAnthropic
		Name: "upstream.example",
		BaseUrls: []dbmodel.BaseUrl{
			{URL: "https://upstream.example"},
		},
		Cloak: dbmodel.ChannelCloak{Mode: "auto"},
	}
}

func embeddingInternalRequest() *transformermodel.InternalLLMRequest {
	text := "hello"
	return &transformermodel.InternalLLMRequest{
		Model: "text-embedding-3-small",
		EmbeddingInput: &transformermodel.EmbeddingInput{
			Single: &text,
		},
		RawAPIFormat: transformermodel.APIFormatOpenAIEmbedding,
	}
}

func chatInternalRequest() *transformermodel.InternalLLMRequest {
	return &transformermodel.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []transformermodel.Message{
			{Role: "user"},
		},
	}
}

// 对照组先行：普通请求在 Anthropic 渠道上必须戴 claude 面容。
// 若这条就为空，说明 face 注入整体没生效，下面那条断言会空转通过。
func TestCloakedChannelChatRequestWearsCLIFace(t *testing.T) {
	ra := &relayAttempt{
		channel:      anthropicCloakedChannel(),
		relayRequest: &relayRequest{internalRequest: chatInternalRequest()},
	}
	if got := ra.outboundTLSFace(); got != "claude" {
		t.Fatalf("Anthropic 渠道的普通 chat 请求应当戴 claude 面容，实际 %q", got)
	}
}

// 正题：同一条渠道上的嵌入请求不许戴任何 CLI 面容。
func TestEmbeddingRequestWearsNoCLITLSFace(t *testing.T) {
	ra := &relayAttempt{
		channel:      anthropicCloakedChannel(),
		relayRequest: &relayRequest{internalRequest: embeddingInternalRequest()},
	}
	if got := ra.outboundTLSFace(); got != "" {
		t.Fatalf("嵌入请求戴上了 %q 面容：嵌入没有 CLI 可言，请求头已经是通用的，"+
			"握手却露着 CLI 指纹 —— 只隔离 header 等于没隔离", got)
	}
}

// internalRequest 为 nil（raw/probe 等路径）时保持原有行为，不能被新分支误伤。
func TestNilInternalRequestKeepsChannelFace(t *testing.T) {
	ra := &relayAttempt{channel: anthropicCloakedChannel(), relayRequest: &relayRequest{}}
	if got := ra.outboundTLSFace(); got != "claude" {
		t.Fatalf("internalRequest 为 nil 时必须保持渠道面容，实际 %q", got)
	}
}

// 真正的守门：`sendRequest` 必须**通过 outboundTLSFace** 决定面容，
// 而不是绕开它直接调 channelTLSFace。上面几条只测了纯函数，
// 而这次的缺陷恰恰是"函数对、调用点漏了" —— 必须钉住调用点。
func TestSendRequestDecidesFaceThroughOutboundTLSFace(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "relay.go", nil, 0)
	if err != nil {
		t.Fatalf("parse relay.go: %v", err)
	}

	var sendRequest *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "sendRequest" {
			continue
		}
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			sendRequest = fn
		}
	}
	if sendRequest == nil {
		t.Fatal("relay.go 里找不到 (*relayAttempt).sendRequest")
	}

	usesOutboundFace := false
	var rawChannelTLSFace bool
	ast.Inspect(sendRequest, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "outboundTLSFace":
			usesOutboundFace = true
		case "channelTLSFace", "ChannelTLSFace", "WithTLSFace":
			// WithTLSFace 本身要在里面（它是施加动作），但 channelTLSFace 不行 ——
			// 那意味着绕过了嵌入例外。
			if sel.Sel.Name != "WithTLSFace" {
				rawChannelTLSFace = true
			}
		}
		return true
	})

	if rawChannelTLSFace {
		t.Fatal("sendRequest 里直接调用了 channelTLSFace：绕过了嵌入流量的例外，握手身份会和请求头分裂")
	}
	if !usesOutboundFace {
		t.Fatal("sendRequest 没有通过 outboundTLSFace 决定面容：判定点被旁路，" +
			"嵌入例外在真实发送路径上不生效（纯函数测试会全绿而线上照旧漏指纹）")
	}
}
