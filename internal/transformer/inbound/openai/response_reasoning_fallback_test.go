package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestResponseInboundUsesReasoningFieldFallback verifies that an upstream that
// emits the `reasoning` field (OpenRouter/Ollama style) instead of
// `reasoning_content` still surfaces reasoning text when bridged to the OpenAI
// Responses streaming protocol.
func TestResponseInboundUsesReasoningFieldFallback(t *testing.T) {
	inbound := &ResponseInbound{}

	reasoning := "thinking via reasoning field"
	out, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_1",
		Model:   "glm-4.6",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role:      "assistant",
				Reasoning: ptr(reasoning),
			},
		}},
	})
	if err != nil {
		t.Fatalf("TransformStream: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, "response.reasoning_summary_text.delta") {
		t.Fatalf("expected a reasoning summary delta event, got: %s", got)
	}
	if !strings.Contains(got, reasoning) {
		t.Fatalf("expected reasoning text to be preserved, got: %s", got)
	}
}

func ptr(s string) *string { return &s }
