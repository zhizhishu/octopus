package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// streamToolCall feeds a two-chunk streaming function call (name in the first
// chunk, arguments split across both) into the inbound converter.
func streamToolCall(t *testing.T, inbound *ResponseInbound) {
	t.Helper()
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID: "cc", Model: "deepseek-v4-pro", Object: "chat.completion.chunk", Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					Index: 0, ID: "call_abc", Type: "function",
					Function: model.FunctionCall{Name: "smartResearch", Arguments: `{"q":`},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("tool-call chunk 1: %v", err)
	}
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID: "cc", Model: "deepseek-v4-pro", Object: "chat.completion.chunk", Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				ToolCalls: []model.ToolCall{{
					Index:    0,
					Function: model.FunctionCall{Arguments: `"A share"}`},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("tool-call chunk 2: %v", err)
	}
}

func assertCompletedCarriesFunctionCall(t *testing.T, got string) {
	t.Helper()
	if !strings.Contains(got, `"type":"response.completed"`) {
		t.Fatalf("expected a response.completed event, got %s", got)
	}
	// The terminal event here is ONLY response.completed (created/in_progress were
	// emitted on an earlier chunk), so an empty output array is the regression.
	if strings.Contains(got, `"output":[]`) {
		t.Fatalf("response.completed.output must not be empty for a tool-call turn: %s", got)
	}
	if !strings.Contains(got, `"type":"function_call"`) || !strings.Contains(got, "call_abc") || !strings.Contains(got, "smartResearch") {
		t.Fatalf("response.completed.output must contain the function_call (call_abc/smartResearch): %s", got)
	}
	// "A share" comes only from the SECOND argument chunk, so its presence in the
	// terminal item proves the arguments were fully accumulated across chunks.
	if !strings.Contains(got, "A share") || !strings.Contains(got, `"arguments":`) {
		t.Fatalf("function_call arguments must be fully accumulated in the output item: %s", got)
	}
}

// TestResponseInboundCompletedIncludesFunctionCallInOutput guards the streaming
// terminal-output fix (usage-terminated stream). A /v1/responses stream whose
// upstream turn is a tool call must deliver the function_call in
// response.completed.response.output — not an empty []. OpenAI-SDK / Cherry Studio
// clients read response.completed.output for the final result; an empty array made
// them see zero tool calls and stop after the first step instead of executing it.
func TestResponseInboundCompletedIncludesFunctionCallInOutput(t *testing.T) {
	inbound := &ResponseInbound{}
	streamToolCall(t, inbound)

	finishReason := "tool_calls"
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID: "cc", Model: "deepseek-v4-pro", Object: "chat.completion.chunk", Created: 1,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	}); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}

	completed, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID: "cc", Model: "deepseek-v4-pro", Object: "chat.completion.chunk", Created: 1,
		Usage: &model.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
	})
	if err != nil {
		t.Fatalf("usage chunk: %v", err)
	}
	assertCompletedCarriesFunctionCall(t, string(completed))
}

// TestResponseInboundCompletedOnDoneIncludesFunctionCall covers the [DONE]-terminated
// path (no usage chunk) via completeResponseEvents.
func TestResponseInboundCompletedOnDoneIncludesFunctionCall(t *testing.T) {
	inbound := &ResponseInbound{}
	streamToolCall(t, inbound)

	finishReason := "tool_calls"
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID: "cc", Model: "deepseek-v4-pro", Object: "chat.completion.chunk", Created: 1,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	}); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("done chunk: %v", err)
	}
	assertCompletedCarriesFunctionCall(t, string(done))
}
