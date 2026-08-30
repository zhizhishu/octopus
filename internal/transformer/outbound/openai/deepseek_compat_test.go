package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

func TestDeepSeekReasonerSamplingParamsStripped(t *testing.T) {
	temp := 0.7
	topP := 0.95
	presence := 0.5
	frequency := 0.5

	for _, modelName := range []string{"deepseek-reasoner", "deepseek-r1", "deepseek-r2", "deepseek-v5-reasoner", "deepseek-reasoner-v2", "deepseek-v4-thinking"} {
		req := &model.InternalLLMRequest{
			Model:            modelName,
			Temperature:      &temp,
			TopP:             &topP,
			PresencePenalty:  &presence,
			FrequencyPenalty: &frequency,
			ReasoningEffort:  "high",
			Messages: []model.Message{
				{
					Role:    "user",
					Content: model.MessageContent{Content: lo.ToPtr("hello")},
				},
			},
		}

		applyThirdPartyChatParamCompat(req, "https://api.deepseek.com")

		if req.Temperature != nil {
			t.Fatalf("[%s] expected Temperature to be nil, got %v", modelName, *req.Temperature)
		}
		if req.TopP != nil {
			t.Fatalf("[%s] expected TopP to be nil, got %v", modelName, *req.TopP)
		}
		if req.PresencePenalty != nil {
			t.Fatalf("[%s] expected PresencePenalty to be nil, got %v", modelName, *req.PresencePenalty)
		}
		if req.FrequencyPenalty != nil {
			t.Fatalf("[%s] expected FrequencyPenalty to be nil, got %v", modelName, *req.FrequencyPenalty)
		}
		if req.ReasoningEffort != "" {
			t.Fatalf("[%s] expected ReasoningEffort to be empty, got %q", modelName, req.ReasoningEffort)
		}
	}
}

func TestMergeConsecutiveSameRoleChatMessages(t *testing.T) {
	reasoning := "thinking steps..."
	text := "final answer"
	req := &model.InternalLLMRequest{
		Model: "deepseek-chat",
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: lo.ToPtr("question")},
			},
			{
				Role:             "assistant",
				ReasoningContent: &reasoning,
			},
			{
				Role:    "assistant",
				Content: model.MessageContent{Content: &text},
			},
		},
	}

	applyThirdPartyChatParamCompat(req, "https://api.deepseek.com")

	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages after merging, got %d", len(req.Messages))
	}
	mergedAssistant := req.Messages[1]
	if mergedAssistant.Role != "assistant" {
		t.Fatalf("expected assistant role, got %s", mergedAssistant.Role)
	}
	if mergedAssistant.ReasoningContent == nil || *mergedAssistant.ReasoningContent != reasoning {
		t.Fatalf("expected reasoning preserved, got %v", mergedAssistant.ReasoningContent)
	}
	if mergedAssistant.Content.Content == nil || *mergedAssistant.Content.Content != text {
		t.Fatalf("expected text content preserved, got %v", mergedAssistant.Content.Content)
	}
}
