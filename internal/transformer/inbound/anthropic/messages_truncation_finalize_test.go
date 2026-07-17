package anthropic

import (
	"bytes"
	"context"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// TestTransformStreamFinalizesOnDoneWithoutFinishReason verifies that when the upstream
// stream ends (a synthesized [DONE]) after content was streamed but WITHOUT ever
// delivering a finish_reason — a weak model running away to max_tokens, or a truncated /
// cut upstream — the anthropic converter still closes the message envelope: it emits
// content_block_stop + message_delta (carrying a stop_reason) + message_stop. Without
// this the stream is left dangling and strict clients (Claude Code) reject the turn.
func TestTransformStreamFinalizesOnDoneWithoutFinishReason(t *testing.T) {
	inbound := &MessagesInbound{}

	// A text content chunk: opens message_start + content_block_start(text) + a delta.
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:    "msg_1",
		Model: "glm-4.6",
		Choices: []transformerModel.Choice{
			{
				Index: 0,
				Delta: &transformerModel.Message{
					Role:    "assistant",
					Content: transformerModel.MessageContent{Content: sigPtr("partial answer")},
				},
			},
		},
	}); err != nil {
		t.Fatalf("content chunk: %v", err)
	}

	// Stream end WITHOUT any finish_reason chunk ever arriving.
	out, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}

	for _, want := range []string{"content_block_stop", "message_delta", "message_stop", "max_tokens"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("expected terminal %q on [DONE] without finish_reason, got: %s", want, out)
		}
	}
}

// TestTransformStreamDoneNoopWhenNothingStarted is the shape-safety guard: a [DONE] with
// no prior content (failover / empty stream) still produces NO events, exactly as before.
// The finalize only fires once a message envelope was actually opened.
func TestTransformStreamDoneNoopWhenNothingStarted(t *testing.T) {
	inbound := &MessagesInbound{}
	out, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no events for [DONE] before any content, got: %s", out)
	}
}

// TestTransformStreamFinalizesTruncatedThinkingBlock verifies the [DONE] finalize also
// closes an open THINKING block (not just text) when the upstream truncates.
func TestTransformStreamFinalizesTruncatedThinkingBlock(t *testing.T) {
	inbound := &MessagesInbound{}
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:    "msg_think",
		Model: "glm-4.6",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{Role: "assistant", ReasoningContent: sigPtr("let me think")},
		}},
	}); err != nil {
		t.Fatalf("thinking chunk: %v", err)
	}
	out, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	for _, want := range []string{"content_block_stop", "message_delta", "message_stop"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("expected terminal %q closing the open thinking block, got: %s", want, out)
		}
	}
}

// TestTransformStreamFinalizesTruncatedToolBlock verifies the [DONE] finalize closes an
// open tool_use block whose arguments were cut off mid-stream. The partial tool JSON can't
// be completed (inherent to truncation), but the turn must still be a well-formed terminated
// Anthropic stream (message_stop) rather than a dangling one.
func TestTransformStreamFinalizesTruncatedToolBlock(t *testing.T) {
	inbound := &MessagesInbound{}
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:    "msg_tool",
		Model: "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				Role: "assistant",
				ToolCalls: []transformerModel.ToolCall{{
					Index:    0,
					ID:       "call_x",
					Type:     "function",
					Function: transformerModel.FunctionCall{Name: "get_weather", Arguments: `{"city":`},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("tool chunk: %v", err)
	}
	out, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	for _, want := range []string{"content_block_stop", "message_stop"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("expected terminal %q closing the open tool block, got: %s", want, out)
		}
	}
}
