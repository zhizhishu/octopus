package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// TestRollingFinishGuardIsNotVacuous proves the rolling-finish regression tests are
// testing something real, by checking the exact two state transitions the fix hinges
// on. A guard that silently stopped applying would make the other tests pass for the
// wrong reason (see the "vacuous verification" failure mode: a test that cannot fail
// looks identical to a test that passes).
//
// Transition 1: a chunk carrying BOTH finish_reason and content must NOT complete the
// response, so later chunks still stream.
// Transition 2: a chunk carrying finish_reason with an EMPTY delta must complete
// immediately, preserving well-behaved upstream latency.
func TestRollingFinishGuardIsNotVacuous(t *testing.T) {
	ctx := context.Background()

	t.Run("finish_with_content_defers_completion", func(t *testing.T) {
		inbound := &ResponseInbound{model: "gemini-3.7-flash-high"}

		firstChunk := &model.InternalLLMResponse{
			ID:    "resp_guard_defer",
			Model: "gemini-3.7-flash-high",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Role:    "assistant",
						Content: model.MessageContent{Content: lo.ToPtr("开头文字")},
					},
				},
			},
			Usage: &model.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		}
		if _, err := inbound.TransformStream(ctx, firstChunk); err != nil {
			t.Fatalf("first chunk failed: %v", err)
		}

		if inbound.responseCompleted {
			t.Fatal("response was completed on a chunk that still carried content; " +
				"the rolling-finish guard is not in effect")
		}
		if !inbound.sawContentAfterFinish {
			t.Fatal("sawContentAfterFinish was not set for a finish chunk carrying content")
		}

		secondChunk := &model.InternalLLMResponse{
			ID:    "resp_guard_defer",
			Model: "gemini-3.7-flash-high",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Content: model.MessageContent{Content: lo.ToPtr("后续文字")},
					},
				},
			},
			Usage: &model.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18},
		}
		secondEvents, err := inbound.TransformStream(ctx, secondChunk)
		if err != nil {
			t.Fatalf("second chunk failed: %v", err)
		}
		if !strings.Contains(string(secondEvents), "后续文字") {
			t.Fatalf("content after the first finish marker was dropped:\n%s", string(secondEvents))
		}
	})

	t.Run("finish_without_content_completes_now", func(t *testing.T) {
		inbound := &ResponseInbound{model: "gpt-5.6_Reasoning"}

		textChunk := &model.InternalLLMResponse{
			ID:    "resp_guard_immediate",
			Model: "gpt-5.6_Reasoning",
			Choices: []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						Role:    "assistant",
						Content: model.MessageContent{Content: lo.ToPtr("正文")},
					},
				},
			},
		}
		if _, err := inbound.TransformStream(ctx, textChunk); err != nil {
			t.Fatalf("text chunk failed: %v", err)
		}

		terminalChunk := &model.InternalLLMResponse{
			ID:    "resp_guard_immediate",
			Model: "gpt-5.6_Reasoning",
			Choices: []model.Choice{
				{Index: 0, FinishReason: lo.ToPtr("stop"), Delta: &model.Message{}},
			},
			Usage: &model.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		}
		if _, err := inbound.TransformStream(ctx, terminalChunk); err != nil {
			t.Fatalf("terminal chunk failed: %v", err)
		}

		if !inbound.responseCompleted {
			t.Fatal("a clean terminal chunk must complete the response immediately")
		}
		if inbound.sawContentAfterFinish {
			t.Fatal("sawContentAfterFinish must stay false for a clean terminal chunk")
		}
	})
}

// TestChunkCarriesGeneratedContentClassification pins the helper the guard depends on,
// so a future change to the delta shape cannot silently reclassify a content-bearing
// chunk as empty (which would resurrect the truncation bug).
func TestChunkCarriesGeneratedContentClassification(t *testing.T) {
	testCases := []struct {
		name         string
		choice       model.Choice
		wantsContent bool
	}{
		{
			name:         "nil delta",
			choice:       model.Choice{Index: 0},
			wantsContent: false,
		},
		{
			name:         "empty delta",
			choice:       model.Choice{Index: 0, Delta: &model.Message{}},
			wantsContent: false,
		},
		{
			name: "empty string content",
			choice: model.Choice{Index: 0, Delta: &model.Message{
				Content: model.MessageContent{Content: lo.ToPtr("")},
			}},
			wantsContent: false,
		},
		{
			name: "text content",
			choice: model.Choice{Index: 0, Delta: &model.Message{
				Content: model.MessageContent{Content: lo.ToPtr("你好")},
			}},
			wantsContent: true,
		},
		{
			name: "tool call fragment",
			choice: model.Choice{Index: 0, Delta: &model.Message{
				ToolCalls: []model.ToolCall{{Index: 0, ID: "call_1", Type: "function"}},
			}},
			wantsContent: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := chunkCarriesGeneratedContent(testCase.choice); got != testCase.wantsContent {
				t.Fatalf("chunkCarriesGeneratedContent = %v, want %v", got, testCase.wantsContent)
			}
		})
	}
}
