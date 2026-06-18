package anthropic

import (
	"bytes"
	"context"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// TestTransformStreamUsesReasoningFieldFallback verifies that an upstream that
// emits the `reasoning` field (OpenRouter/Ollama style) instead of
// `reasoning_content` still produces a thinking_delta carrying the reasoning
// text when bridged to the Anthropic Messages streaming protocol.
func TestTransformStreamUsesReasoningFieldFallback(t *testing.T) {
	inbound := &MessagesInbound{}

	reasoning := "thinking via reasoning field"
	out, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:    "msg_1",
		Model: "glm-4.6",
		Choices: []transformerModel.Choice{
			{
				Index: 0,
				Delta: &transformerModel.Message{
					Role:      "assistant",
					Reasoning: sigPtr(reasoning),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("TransformStream: %v", err)
	}
	if !bytes.Contains(out, []byte("thinking_delta")) {
		t.Fatalf("expected a thinking_delta event, got: %s", out)
	}
	if !bytes.Contains(out, []byte(reasoning)) {
		t.Fatalf("expected reasoning text to be preserved, got: %s", out)
	}
}
