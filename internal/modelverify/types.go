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
