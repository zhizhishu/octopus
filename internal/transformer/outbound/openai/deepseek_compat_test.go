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

	req := &model.InternalLLMRequest{
		Model:            "deepseek-reasoner",
		Temperature:      &temp,
		TopP:             &topP,
		PresencePenalty:  &presence,
		FrequencyPenalty: &frequency,
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: lo.ToPtr("hello")},
			},
		},
	}

	applyThirdPartyChatParamCompat(req, "https://api.deepseek.com")

	if req.Temperature != nil {
		t.Fatalf("expected Temperature to be nil, got %v", *req.Temperature)
	}
	if req.TopP != nil {
		t.Fatalf("expected TopP to be nil, got %v", *req.TopP)
	}
	if req.PresencePenalty != nil {
		t.Fatalf("expected PresencePenalty to be nil, got %v", *req.PresencePenalty)
	}
	if req.FrequencyPenalty != nil {
		t.Fatalf("expected FrequencyPenalty to be nil, got %v", *req.FrequencyPenalty)
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
