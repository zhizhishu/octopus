package anthropic

import (
	"context"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func sigPtr(s string) *string { return &s }

// TestGetInternalResponseAggregatesReasoningSignature verifies that when an
// upstream Anthropic stream is aggregated into a single non-stream response, the
// thinking signature (delivered by the outbound transformer as a complete value
// on a ReasoningSignature delta) is preserved alongside the accumulated reasoning
// content. Losing it makes Anthropic reject the thinking block on the next turn.
func TestGetInternalResponseAggregatesReasoningSignature(t *testing.T) {
	inbound := &MessagesInbound{
		streamChunks: []*transformerModel.InternalLLMResponse{
			{
				ID:    "msg_1",
				Model: "claude-sonnet-4-5",
				Choices: []transformerModel.Choice{
					{
						Index: 0,
						Delta: &transformerModel.Message{
							Role:             "assistant",
							ReasoningContent: sigPtr("Let me "),
						},
					},
				},
			},
			{
				Choices: []transformerModel.Choice{
					{
						Index: 0,
						Delta: &transformerModel.Message{
							ReasoningContent: sigPtr("think."),
						},
					},
				},
			},
			{
				// Signature arrives as a complete value in its own delta.
				Choices: []transformerModel.Choice{
					{
						Index: 0,
						Delta: &transformerModel.Message{
							ReasoningSignature: sigPtr("abc123signature"),
						},
					},
				},
			},
		},
	}

	resp, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %#v", resp)
	}
	msg := resp.Choices[0].Message
	if msg == nil {
		t.Fatal("expected aggregated message")
	}
	if msg.ReasoningContent == nil || *msg.ReasoningContent != "Let me think." {
		t.Fatalf("reasoning content not aggregated: %#v", msg.ReasoningContent)
	}
	if msg.ReasoningSignature == nil || *msg.ReasoningSignature != "abc123signature" {
		t.Fatalf("reasoning signature lost during aggregation: %#v", msg.ReasoningSignature)
	}
}

// TestTransformStreamSignatureOnlyOpensThinkingBlock pins the fix for a
// signature-only thinking stream: when the upstream emits a thinking signature
// with NO reasoning text (no thinking_delta), the inbound synthesizer must still
// open the thinking content block BEFORE the signature_delta. Previously the
// signature_delta (and the terminal content_block_stop) were emitted with no
// content_block_start ever sent, and the official Anthropic SDK — which indexes
// the content-block list by index — raised IndexError. Every delta/stop must have
// a matching content_block_start at the same index, and message_stop must be
// emitted exactly once.
func TestTransformStreamSignatureOnlyOpensThinkingBlock(t *testing.T) {
	inbound := &MessagesInbound{}

	signature := "sig-only-abc"
	delta, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "msg_sig_only",
		Object: "chat.completion.chunk",
		Model:  "claude-sonnet-4-5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{Role: "assistant", ReasoningSignature: sigPtr(signature)},
		}},
	})
	if err != nil {
		t.Fatalf("signature-only stream: %v", err)
	}

	finishReason := "stop"
	finish, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "msg_sig_only",
		Object: "chat.completion.chunk",
		Model:  "claude-sonnet-4-5",
		Choices: []transformerModel.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	})
	if err != nil {
		t.Fatalf("finish stream: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE] stream: %v", err)
	}

	events := parseAnthropicStreamEvents(t, delta, finish, done)

	opened := map[int64]bool{}
	stopped := map[int64]bool{}
	sawSignatureDelta := false
	messageStopCount := 0
	for _, ev := range events {
		switch ev.Type {
		case "content_block_start":
			if ev.Index == nil {
				t.Fatalf("content_block_start without index: %#v", ev)
			}
			opened[*ev.Index] = true
		case "content_block_delta":
			if ev.Index == nil {
				t.Fatalf("content_block_delta without index: %#v", ev)
			}
			if !opened[*ev.Index] {
				t.Fatalf("content_block_delta at index %d has no preceding content_block_start; events=%+v", *ev.Index, events)
			}
			if ev.Delta != nil && ev.Delta.Type != nil && *ev.Delta.Type == "signature_delta" {
				sawSignatureDelta = true
			}
		case "content_block_stop":
			if ev.Index == nil {
				t.Fatalf("content_block_stop without index: %#v", ev)
			}
			if !opened[*ev.Index] {
				t.Fatalf("content_block_stop at index %d has no preceding content_block_start; events=%+v", *ev.Index, events)
			}
			stopped[*ev.Index] = true
		case "message_stop":
			messageStopCount++
		}
	}

	if !sawSignatureDelta {
		t.Fatalf("signature-only stream must preserve the signature_delta, events=%+v", events)
	}
	if len(opened) != 1 || !stopped[0] {
		t.Fatalf("expected exactly one thinking block opened and stopped at index 0, opened=%v stopped=%v events=%+v", opened, stopped, events)
	}
	if messageStopCount != 1 {
		t.Fatalf("expected exactly one message_stop, got %d; events=%+v", messageStopCount, events)
	}
}

// TestTransformStreamEmptyReplyEmitsNoOrphanStop guards the finish-reason path: a
// reply that never opened any content block (empty answer) must NOT emit a
// content_block_stop without a matching content_block_start.
func TestTransformStreamEmptyReplyEmitsNoOrphanStop(t *testing.T) {
	inbound := &MessagesInbound{}

	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:      "msg_empty",
		Object:  "chat.completion.chunk",
		Model:   "claude-sonnet-4-5",
		Choices: []transformerModel.Choice{{Index: 0, Delta: &transformerModel.Message{Role: "assistant"}}},
	}); err != nil {
		t.Fatalf("role-only stream: %v", err)
	}

	finishReason := "stop"
	finish, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "msg_empty",
		Object: "chat.completion.chunk",
		Model:  "claude-sonnet-4-5",
		Choices: []transformerModel.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	})
	if err != nil {
		t.Fatalf("finish stream: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE] stream: %v", err)
	}

	events := parseAnthropicStreamEvents(t, finish, done)
	messageStopCount := 0
	for _, ev := range events {
		switch ev.Type {
		case "content_block_stop":
			t.Fatalf("empty reply must not emit an orphan content_block_stop; events=%+v", events)
		case "message_stop":
			messageStopCount++
		}
	}
	if messageStopCount != 1 {
		t.Fatalf("expected exactly one message_stop, got %d; events=%+v", messageStopCount, events)
	}
}

// TestGetInternalResponseSignatureLastNonEmptyWins verifies that empty signature
// deltas do not clobber a previously captured signature, and the last non-empty
// value is kept.
func TestGetInternalResponseSignatureLastNonEmptyWins(t *testing.T) {
	inbound := &MessagesInbound{
		streamChunks: []*transformerModel.InternalLLMResponse{
			{
				ID:    "msg_2",
				Model: "claude-sonnet-4-5",
				Choices: []transformerModel.Choice{
					{Index: 0, Delta: &transformerModel.Message{ReasoningSignature: sigPtr("first")}},
				},
			},
			{
				Choices: []transformerModel.Choice{
					{Index: 0, Delta: &transformerModel.Message{ReasoningSignature: sigPtr("")}},
				},
			},
			{
				Choices: []transformerModel.Choice{
					{Index: 0, Delta: &transformerModel.Message{ReasoningSignature: sigPtr("second")}},
				},
			},
		},
	}

	resp, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg == nil || msg.ReasoningSignature == nil {
		t.Fatalf("expected a signature, got %#v", msg)
	}
	if *msg.ReasoningSignature != "second" {
		t.Fatalf("expected last non-empty signature 'second', got %q", *msg.ReasoningSignature)
	}
}
