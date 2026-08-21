package openai

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

func TestResponseInboundAggregatesSparseChoiceIndexes(t *testing.T) {
	inbound := &ResponseInbound{
		model: "gpt-4o",
	}
	ctx := context.Background()

	// Chunk with choice index 1 (choice 0 omitted)
	chunk1 := &model.InternalLLMResponse{
		ID:    "resp_sparse_test",
		Model: "gpt-4o",
		Choices: []model.Choice{
			{
				Index: 1,
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: lo.ToPtr("Sparse choice 1 text")},
				},
			},
		},
	}
	_, err := inbound.TransformStream(ctx, chunk1)
	if err != nil {
		t.Fatalf("chunk 1 failed: %v", err)
	}

	// Chunk with choice index 3
	chunk3 := &model.InternalLLMResponse{
		ID:    "resp_sparse_test",
		Model: "gpt-4o",
		Choices: []model.Choice{
			{
				Index: 3,
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: lo.ToPtr("Sparse choice 3 text")},
				},
			},
		},
	}
	_, err = inbound.TransformStream(ctx, chunk3)
	if err != nil {
		t.Fatalf("chunk 3 failed: %v", err)
	}

	// Aggregate non-stream response
	aggregated, err := inbound.GetInternalResponse(ctx)
	if err != nil {
		t.Fatalf("GetInternalResponse failed: %v", err)
	}

	if len(aggregated.Choices) != 2 {
		t.Fatalf("expected exactly 2 choices from sparse indexes [1, 3], got %d choices: %+v", len(aggregated.Choices), aggregated.Choices)
	}
	if aggregated.Choices[0].Index != 1 || *aggregated.Choices[0].Message.Content.Content != "Sparse choice 1 text" {
		t.Fatalf("unexpected choice 0: %+v", aggregated.Choices[0])
	}
	if aggregated.Choices[1].Index != 3 || *aggregated.Choices[1].Message.Content.Content != "Sparse choice 3 text" {
		t.Fatalf("unexpected choice 1: %+v", aggregated.Choices[1])
	}
}
