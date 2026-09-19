package modelverify

import "encoding/json"

// Response 一次探针请求的协议无关观测输入。
//
// 与参考项目的 adapter.Response 同义，但由本项目自行定义以保持包边界：出站层
// （参考项目的 adapter / transport）刻意未移植——那两层用标准 net/http 自拼请求，
// 会丢弃本项目模拟的客户端身份、破坏渠道指纹契约。这里只保留「观测需要什么」，
// 协议的渲染与解析全部归 oct 的既有出站链路。
type Response struct {
	Content          string
	ReasoningContent string
	FinishReason     string
	Usage            Usage
	ToolCalls        []ToolCall

	// Signature 是 Anthropic extended thinking 的 thinking.signature 原文
	// （base64 包裹的 AEAD protobuf，由 Anthropic 私钥签发并绑定模型名）。
	//
	// 这是本项目能拿到的最硬证据：中转站可以改写文本、可以伪造模型名，
	// 但无法伪造一段能被 Anthropic 自己解开的密文。仅 Anthropic 协议会填。
	Signature string
	// RedactedSignature 是 redacted_thinking.data 原文。同样是服务端产物、
	// 同样不可伪造，但内容不可读（本来就是密文），只能做结构与回放判定。
	RedactedSignature string
}

// Turn 探针多轮会话中的一轮。
//
// 多数探针只需要 Ask.Prompt 的单轮形态。签名回放探针是例外：它必须把上一次响应
// 的 thinking 块原样塞回 assistant 轮，再追加一轮 user 提问——这个"把服务端密文
// 还给服务端"的动作本身就要求多轮。
//
// 刻意不复用 transformer 包的消息类型：modelverify 要能独立测试，
// 协议渲染归桥负责。
type Turn struct {
	// Role 取 "user" / "assistant"。
	Role string
	// Content 该轮的可见文本。
	Content string
	// Signature 仅 assistant 轮用到：把 harvest 拿到的签名原样回填。
	// 对应的 thinking 文本留空是上游的既定做法——模型解封的是密文本身，
	// 不需要我们提供明文。
	Signature string
	// Redacted 标记 Signature 来自 redacted_thinking。桥据此决定回填成哪一种
	// 块，因为 thinking 与 redacted_thinking 是两个不同的 content 类型，
	// 混用会被上游判为格式错误。
	Redacted bool
}

// Usage 归一后的 token 计数。
type Usage struct {
	Prompt     int
	Completion int
	Reasoning  int
	Cached     int
}

// ToolCall 一次工具调用的归一形态。Args 为参数 JSON 原文（流式增量拼接的结果），
// 解析失败时保留原始片段供 ObserveToolCall 判定 args_valid。
type ToolCall struct {
	Name string
	Args json.RawMessage
}

// Ask 一次探针请求的可调参数。零值表示"由出站链路按渠道契约决定"——
// 刻意不在此处设默认 max_tokens 或 temperature，以免探针私自改写 body 形状。
type Ask struct {
	Prompt      string
	MaxTokens   *int
	Temperature *float64
	// Turns 非空时取代 Prompt 成为完整会话；留空则保持单轮 Prompt 行为不变。
	Turns []Turn
	// Thinking 要求本轮开启 extended thinking。只有需要 thinking.signature 的
	// 探针会用到——普通探针开着思考纯属浪费额度与时间。默认 false 走
	// 既有的 {"type":"disabled"}，与普通轮的真 CLI 形态一致。
	Thinking bool
}

// ReasoningUnit 思考量观测单位，由协议**静态**决定，不做运行时回退：
// 同一 cell 内两侧单位必须一致，否则秩检验失效。
//   - "tokens"：usage 上报独立思考 token 数（openai 系协议）
//   - "chars"：usage 无独立思考字段的协议，改用交付的思考文本 rune 数
//
// 未知协议按 tokens 处理（多数协议走 openai 风格 usage），新增协议时应显式登记。
func ReasoningUnit(protocolID string) string {
	if protocolID == "anthropic-messages" {
		return "chars"
	}
	return "tokens"
}
