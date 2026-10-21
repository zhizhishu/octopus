package openai

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// reasoning_effort 是客户端自由透传、relay 不校验的字段，所以大小写与空白
// 变体是必然会出现的输入，不是理论上的边角。
//
// 这里曾经用手写 case 配 strings.ToLower：Lower 不去空白，客户端发 " max "
// 就比不中任何 case → 静默变成「不开思考」。实测 "max"/"MAX"/"Max" 都开，
// 唯独带空格的两种不开；而同一个值走 anthropic 通道（EffortBudgetFor 会
// TrimSpace）却正常 —— 同一条请求换个通道，强度就变了。
func TestGLMThinkingIgnoresCaseAndSpace(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		for _, variant := range []string{
			effort, strings.ToUpper(effort), strings.Title(effort),
			" " + effort, effort + " ", "\t" + effort + "\n",
		} {
			req := &model.InternalLLMRequest{Model: "glm-4.6", ReasoningEffort: variant}
			applyGLMThinking(req)
			if !rawJSONPresent(req.Thinking) {
				t.Errorf("effort=%q 应当开启思考（与 %q 等价），实际没发 thinking 字段", variant, effort)
			}
		}
	}
}

// 明确要求关闭的两个档位不能被归一逻辑吞掉。
func TestGLMThinkingDisabledLevelsStillWork(t *testing.T) {
	for _, effort := range []string{"minimal", "none", " MINIMAL ", "None"} {
		req := &model.InternalLLMRequest{Model: "glm-4.6", ReasoningEffort: effort}
		applyGLMThinking(req)
		if string(req.Thinking) != `{"type":"disabled"}` {
			t.Errorf("effort=%q 应当关闭思考，实际 %s", effort, req.Thinking)
		}
	}
}

// 未知档位保持"不注入"——这是刻意的保守选择，不能被上面的归一顺手改掉。
func TestGLMThinkingUnknownLevelStaysUntouched(t *testing.T) {
	for _, effort := range []string{"ultra", "auto", "adaptive", ""} {
		req := &model.InternalLLMRequest{Model: "glm-4.6", ReasoningEffort: effort}
		applyGLMThinking(req)
		if rawJSONPresent(req.Thinking) {
			t.Errorf("effort=%q 是未知档位，不应注入 thinking，实际 %s", effort, req.Thinking)
		}
	}
}
