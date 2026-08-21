package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

func TestResponseInboundDoesNotDropTailTextAfterFinishReason(t *testing.T) {
	inbound := &ResponseInbound{
		model: "gemini-3.7-flash-high",
	}
	ctx := context.Background()

	// Chunk 1: regular text
	chunk1 := &model.InternalLLMResponse{
		ID:    "resp_test_1",
		Model: "gemini-3.7-flash-high",
		Choices: []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: lo.ToPtr("Hello, this is part one. ")},
				},
			},
		},
	}
	events1, err := inbound.TransformStream(ctx, chunk1)
	if err != nil {
		t.Fatalf("chunk 1 failed: %v", err)
	}
	if !strings.Contains(string(events1), "Hello, this is part one. ") {
		t.Fatalf("expected chunk 1 text in output, got %s", string(events1))
	}

	// Chunk 2: carries finish_reason: "stop"
	chunk2 := &model.InternalLLMResponse{
		ID:    "resp_test_1",
		Model: "gemini-3.7-flash-high",
		Choices: []model.Choice{
			{
				Index:        0,
				FinishReason: lo.ToPtr("stop"),
				Delta: &model.Message{
					Content: model.MessageContent{Content: lo.ToPtr("And this is part two at finish.")},
				},
			},
		},
	}
	events2, err := inbound.TransformStream(ctx, chunk2)
	if err != nil {
		t.Fatalf("chunk 2 failed: %v", err)
	}
	if !strings.Contains(string(events2), "And this is part two at finish.") {
		t.Fatalf("expected chunk 2 text in output even with finish_reason, got %s", string(events2))
	}

	// Chunk 3: trailing text fragment before [DONE]
	chunk3 := &model.InternalLLMResponse{
		ID:    "resp_test_1",
		Model: "gemini-3.7-flash-high",
		Choices: []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Content: model.MessageContent{Content: lo.ToPtr(" Final tail words.")},
				},
			},
		},
	}
	events3, err := inbound.TransformStream(ctx, chunk3)
	if err != nil {
		t.Fatalf("chunk 3 failed: %v", err)
	}
	if !strings.Contains(string(events3), " Final tail words.") {
		t.Fatalf("expected tail text in output, got %s", string(events3))
	}
}
