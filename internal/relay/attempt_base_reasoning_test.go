package relay

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// attemptBase 的核心承诺：客户端说「别思考」，重试到下一个渠道时**仍然**是「别思考」。
//
// 为什么这条必须有测试：codex_shape 为兼容 GPT-5.6 会就地改写共享 internalRequest
// 的 ReasoningEffort（空 → "high"，不认识的值 → "low"），chat outbound 的
// applyReasoningProjection 也会改写它。这些改写只对那一跳成立。重试循环以前只还原
// Model/Messages/PreviousResponseID/ResponsesInputRaw/ResponsesInstructions 五项，
// 不含思考字段——于是首跳为兼容而改的值会骑到次跳：客户端发 none，首跳 codex 改成
// low 失败，次跳 Qwen 就按 low 真的开思考。日志里 reasoning_effort 仍记 none，
// 用户完全看不出来。
func TestAttemptBaseRestoresClientReasoningIntent(t *testing.T) {
	originalEffort := "none"
	budget := int64(2048)
	enableThinking := true
	thinkingBudget := int64(1024)
	previousResponseID := "resp_original"
	instructions := "client instructions"
	thinking := json.RawMessage(`{"type":"disabled"}`)
	contentText := "ping"

	internalRequest := &model.InternalLLMRequest{
		Model:                 "request-model",
		ReasoningEffort:       originalEffort,
		ReasoningBudget:       &budget,
		EnableThinking:        &enableThinking,
		ThinkingBudget:        &thinkingBudget,
		AdaptiveThinking:      true,
		Thinking:              thinking,
		PreviousResponseID:    &previousResponseID,
		ResponsesInstructions: &instructions,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: &contentText}},
		},
	}

	base := attemptBase{
		messages:              append([]model.Message(nil), internalRequest.Messages...),
		previousResponseID:    internalRequest.PreviousResponseID,
		responsesInstructions: internalRequest.ResponsesInstructions,
		reasoningEffort:       internalRequest.ReasoningEffort,
		reasoningBudget:       internalRequest.ReasoningBudget,
		thinking:              cloneRawJSONMessage(internalRequest.Thinking),
		enableThinking:        internalRequest.EnableThinking,
		thinkingBudget:        internalRequest.ThinkingBudget,
		adaptiveThinking:      internalRequest.AdaptiveThinking,
	}

	// 模拟首跳 Codex 的兼容改写 + chat outbound 的投影改写：
	// 客户端要 none，被就地改成 low，thinking 也被换掉。
	internalRequest.ReasoningEffort = "low"
	internalRequest.Thinking = json.RawMessage(`{"type":"enabled"}`)
	internalRequest.EnableThinking = nil
	internalRequest.Model = "upstream-model-name"

	base.restore(internalRequest, "request-model")

	if got := internalRequest.ReasoningEffort; got != originalEffort {
		t.Errorf("ReasoningEffort after restore = %q, want %q (首跳的兼容改写骑到了次跳)", got, originalEffort)
	}
	if internalRequest.ReasoningBudget == nil || *internalRequest.ReasoningBudget != budget {
		t.Errorf("ReasoningBudget after restore = %v, want %d", internalRequest.ReasoningBudget, budget)
	}
	if internalRequest.EnableThinking == nil || *internalRequest.EnableThinking != enableThinking {
		t.Errorf("EnableThinking after restore = %v, want %v", internalRequest.EnableThinking, enableThinking)
	}
	if internalRequest.ThinkingBudget == nil || *internalRequest.ThinkingBudget != thinkingBudget {
		t.Errorf("ThinkingBudget after restore = %v, want %d", internalRequest.ThinkingBudget, thinkingBudget)
	}
	if !internalRequest.AdaptiveThinking {
		t.Error("AdaptiveThinking after restore = false, want true")
	}
	if got := string(internalRequest.Thinking); got != `{"type":"disabled"}` {
		t.Errorf("Thinking after restore = %s, want the client's original", got)
	}
	if got := internalRequest.Model; got != "request-model" {
		t.Errorf("Model after restore = %q, want %q", got, "request-model")
	}
}
