package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// TestResponseInboundReasoningOnlyFinishLifecycle covers the stream termination
// lifecycle for reasoning-only responses (e.g. DeepSeek-R1 emitting only
// reasoning_content delta and finishing without generating text content).
func TestResponseInboundReasoningOnlyFinishLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("reasoning_only_last_chunk_has_finish_and_followed_by_usage", func(t *testing.T) {
		inbound := &ResponseInbound{model: "deepseek-reasoner"}

		// Chunk 1: reasoning content delta
		chunk1 := &model.InternalLLMResponse{
			ID:    "chatcmpl_ds_1",
			Model: "deepseek-reasoner",
			Choices: []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						Role:             "assistant",
						ReasoningContent: lo.ToPtr("Let me think about this problem."),
					},
				},
			},
		}
		events1, err := inbound.TransformStream(ctx, chunk1)
		if err != nil {
			t.Fatalf("chunk 1 failed: %v", err)
		}
		if !strings.Contains(string(events1), "response.reasoning_summary_text.delta") {
			t.Fatalf("expected reasoning summary delta in chunk 1, got:\n%s", string(events1))
		}

		// Chunk 2: last reasoning delta together with finish_reason: "stop"
		chunk2 := &model.InternalLLMResponse{
			ID:    "chatcmpl_ds_1",
			Model: "deepseek-reasoner",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						ReasoningContent: lo.ToPtr(" Done thinking."),
					},
				},
			},
		}
		events2, err := inbound.TransformStream(ctx, chunk2)
		if err != nil {
			t.Fatalf("chunk 2 failed: %v", err)
		}
		events2Str := string(events2)
		// Must finalize reasoning item immediately upon finish_reason
		if !strings.Contains(events2Str, "response.reasoning_summary_text.done") {
			t.Fatalf("expected reasoning_summary_text.done in chunk 2, got:\n%s", events2Str)
		}
		if !strings.Contains(events2Str, "response.reasoning_summary_part.done") {
			t.Fatalf("expected reasoning_summary_part.done in chunk 2, got:\n%s", events2Str)
		}
		if !strings.Contains(events2Str, "response.output_item.done") {
			t.Fatalf("expected output_item.done in chunk 2, got:\n%s", events2Str)
		}
		if inbound.sawContentAfterFinish {
			t.Fatal("sawContentAfterFinish must be false for same-chunk finish+delta")
		}
		if inbound.responseCompleted {
			t.Fatal("responseCompleted should not be true before usage or [DONE]")
		}

		// Chunk 3: usage-only chunk
		chunk3 := &model.InternalLLMResponse{
			ID:      "chatcmpl_ds_1",
			Model:   "deepseek-reasoner",
			Choices: []model.Choice{},
			Usage: &model.Usage{
				PromptTokens:     20,
				CompletionTokens: 15,
				TotalTokens:      35,
			},
		}
		events3, err := inbound.TransformStream(ctx, chunk3)
		if err != nil {
			t.Fatalf("chunk 3 failed: %v", err)
		}
		events3Str := string(events3)
		if !strings.Contains(events3Str, `"type":"response.completed"`) {
			t.Fatalf("expected response.completed on usage chunk, got:\n%s", events3Str)
		}
		if !inbound.responseCompleted {
			t.Fatal("responseCompleted should be true after usage chunk")
		}

		// Chunk 4: [DONE] marker
		doneChunk := &model.InternalLLMResponse{Object: "[DONE]"}
		eventsDone, err := inbound.TransformStream(ctx, doneChunk)
		if err != nil {
			t.Fatalf("done chunk failed: %v", err)
		}
		eventsDoneStr := string(eventsDone)
		if strings.Contains(eventsDoneStr, `"type":"response.completed"`) {
			t.Fatalf("response.completed must not be emitted again on [DONE], got:\n%s", eventsDoneStr)
		}
		if !strings.Contains(eventsDoneStr, "data: [DONE]\n\n") {
			t.Fatalf("expected [DONE] event, got:\n%s", eventsDoneStr)
		}

		// Total raw events verification
		allEventsStr := string(events1) + events2Str + events3Str + eventsDoneStr
		events := parseResponsesStreamEvents(t, allEventsStr)
		assertResponsesStreamItemLifecycle(t, events)

		if count := strings.Count(allEventsStr, `"type":"response.completed"`); count != 1 {
			t.Fatalf("expected exactly 1 response.completed event, got %d", count)
		}
	})

	t.Run("reasoning_only_last_chunk_has_finish_and_followed_by_done_only", func(t *testing.T) {
		inbound := &ResponseInbound{model: "deepseek-reasoner"}

		chunk1 := &model.InternalLLMResponse{
			ID:    "chatcmpl_ds_2",
			Model: "deepseek-reasoner",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Role:             "assistant",
						ReasoningContent: lo.ToPtr("All reasoning in one chunk."),
					},
				},
			},
		}
		events1, err := inbound.TransformStream(ctx, chunk1)
		if err != nil {
			t.Fatalf("chunk 1 failed: %v", err)
		}
		events1Str := string(events1)
		if !strings.Contains(events1Str, "response.output_item.done") {
			t.Fatalf("expected output_item.done in chunk 1, got:\n%s", events1Str)
		}

		// Direct [DONE] without usage chunk
		doneChunk := &model.InternalLLMResponse{Object: "[DONE]"}
		eventsDone, err := inbound.TransformStream(ctx, doneChunk)
		if err != nil {
			t.Fatalf("done chunk failed: %v", err)
		}
		eventsDoneStr := string(eventsDone)
		if !strings.Contains(eventsDoneStr, `"type":"response.completed"`) {
			t.Fatalf("expected response.completed on [DONE], got:\n%s", eventsDoneStr)
		}
		if !strings.Contains(eventsDoneStr, "data: [DONE]\n\n") {
			t.Fatalf("expected [DONE] data event, got:\n%s", eventsDoneStr)
		}

		allEventsStr := events1Str + eventsDoneStr
		events := parseResponsesStreamEvents(t, allEventsStr)
		assertResponsesStreamItemLifecycle(t, events)

		if count := strings.Count(allEventsStr, `"type":"response.completed"`); count != 1 {
			t.Fatalf("expected exactly 1 response.completed event, got %d", count)
		}
	})

	t.Run("regular_content_finish_and_usage", func(t *testing.T) {
		inbound := &ResponseInbound{model: "gpt-4o"}

		// Chunk 1: text delta
		chunk1 := &model.InternalLLMResponse{
			ID:    "chatcmpl_text_1",
			Model: "gpt-4o",
			Choices: []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						Role:    "assistant",
						Content: model.MessageContent{Content: lo.ToPtr("Hello world.")},
					},
				},
			},
		}
		events1, err := inbound.TransformStream(ctx, chunk1)
		if err != nil {
			t.Fatalf("chunk 1 failed: %v", err)
		}

		// Chunk 2: last delta with finish_reason
		chunk2 := &model.InternalLLMResponse{
			ID:    "chatcmpl_text_1",
			Model: "gpt-4o",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Content: model.MessageContent{Content: lo.ToPtr(" Farewell!")},
					},
				},
			},
		}
		events2, err := inbound.TransformStream(ctx, chunk2)
		if err != nil {
			t.Fatalf("chunk 2 failed: %v", err)
		}
		events2Str := string(events2)
		if !strings.Contains(events2Str, "response.output_item.done") {
			t.Fatalf("expected output_item.done in chunk 2, got:\n%s", events2Str)
		}

		// Chunk 3: usage
		chunk3 := &model.InternalLLMResponse{
			ID:      "chatcmpl_text_1",
			Model:   "gpt-4o",
			Choices: []model.Choice{},
			Usage: &model.Usage{
				PromptTokens:     10,
				CompletionTokens: 10,
				TotalTokens:      20,
			},
		}
		events3, err := inbound.TransformStream(ctx, chunk3)
		if err != nil {
			t.Fatalf("chunk 3 failed: %v", err)
		}
		events3Str := string(events3)
		if !strings.Contains(events3Str, `"type":"response.completed"`) {
			t.Fatalf("expected response.completed in chunk 3, got:\n%s", events3Str)
		}

		// Chunk 4: [DONE]
		doneChunk := &model.InternalLLMResponse{Object: "[DONE]"}
		eventsDone, err := inbound.TransformStream(ctx, doneChunk)
		if err != nil {
			t.Fatalf("done chunk failed: %v", err)
		}

		allEventsStr := string(events1) + events2Str + events3Str + string(eventsDone)
		events := parseResponsesStreamEvents(t, allEventsStr)
		assertResponsesStreamItemLifecycle(t, events)

		if count := strings.Count(allEventsStr, `"type":"response.completed"`); count != 1 {
			t.Fatalf("expected exactly 1 response.completed event, got %d", count)
		}
	})

	t.Run("extra_content_after_finish_sets_sawContentAfterFinish_and_completes_at_done", func(t *testing.T) {
		inbound := &ResponseInbound{model: "gemini-3.7-flash-high"}

		// Chunk 1: finish with content
		chunk1 := &model.InternalLLMResponse{
			ID:    "chatcmpl_trailing",
			Model: "gemini-3.7-flash-high",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Role:    "assistant",
						Content: model.MessageContent{Content: lo.ToPtr("Part 1.")},
					},
				},
			},
		}
		events1, err := inbound.TransformStream(ctx, chunk1)
		if err != nil {
			t.Fatalf("chunk 1 failed: %v", err)
		}
		if inbound.sawContentAfterFinish {
			t.Fatal("sawContentAfterFinish should not be true on initial chunk with finish")
		}

		// Chunk 2: extra content arriving after finish was already set
		chunk2 := &model.InternalLLMResponse{
			ID:    "chatcmpl_trailing",
			Model: "gemini-3.7-flash-high",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("stop"),
					Delta: &model.Message{
						Content: model.MessageContent{Content: lo.ToPtr(" Trailing part 2.")},
					},
				},
			},
			Usage: &model.Usage{
				PromptTokens:     10,
				CompletionTokens: 8,
				TotalTokens:      18,
			},
		}
		events2, err := inbound.TransformStream(ctx, chunk2)
		if err != nil {
			t.Fatalf("chunk 2 failed: %v", err)
		}
		if !inbound.sawContentAfterFinish {
			t.Fatal("sawContentAfterFinish must be true after extra content past finish")
		}
		if inbound.responseCompleted {
			t.Fatal("responseCompleted must be withheld while sawContentAfterFinish is true")
		}

		// Chunk 3: [DONE]
		doneChunk := &model.InternalLLMResponse{Object: "[DONE]"}
		eventsDone, err := inbound.TransformStream(ctx, doneChunk)
		if err != nil {
			t.Fatalf("done chunk failed: %v", err)
		}
		eventsDoneStr := string(eventsDone)
		if !strings.Contains(eventsDoneStr, `"type":"response.completed"`) {
			t.Fatalf("expected response.completed on [DONE], got:\n%s", eventsDoneStr)
		}

		allEventsStr := string(events1) + string(events2) + eventsDoneStr
		events := parseResponsesStreamEvents(t, allEventsStr)
		assertResponsesStreamItemLifecycle(t, events)

		if count := strings.Count(allEventsStr, `"type":"response.completed"`); count != 1 {
			t.Fatalf("expected exactly 1 response.completed event, got %d", count)
		}
	})
}
