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
