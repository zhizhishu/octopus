package authropic

import (
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

func TestAnthropicThinkingStripsTemperatureAndTopP(t *testing.T) {
	temp := 0.2
	topP := 0.95
	req := &model.InternalLLMRequest{
		Model:             "claude-3-7-sonnet-20250219",
		Temperature:       &temp,
		TopP:              &topP,
		AnthropicThinking: json.RawMessage(`{"type":"enabled","budget_tokens":2048}`),
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: lo.ToPtr("Hello Claude")},
			},
		},
	}

	converted := convertToAnthropicRequest(req)

	if converted.Thinking == nil || converted.Thinking.Type != "enabled" {
		t.Fatalf("expected Thinking to be enabled, got %+v", converted.Thinking)
	}
	if converted.Temperature != nil {
		t.Fatalf("expected Temperature to be nil when thinking enabled, got %v", *converted.Temperature)
	}
	if converted.TopP != nil {
		t.Fatalf("expected TopP to be nil when thinking enabled, got %v", *converted.TopP)
	}
}
