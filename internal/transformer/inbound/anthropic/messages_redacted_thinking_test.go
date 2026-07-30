package anthropic

import (
	"context"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// TestRedactedThinkingInboundPreservesData verifies that a redacted_thinking
// request block is preserved onto the parsed assistant message's
// ReasoningSignature (marker-encoded) instead of being dropped. The opaque
// payload must survive so the outbound path can replay it on the next turn;
// dropping it makes Anthropic 400 a thinking+tool continuation.
func TestRedactedThinkingInboundPreservesData(t *testing.T) {
	const blob = "EvcBCkgIAxABGAIiQredactedBlobPayload=="

	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "redacted_thinking", "data": "` + blob + `"},
				{"type": "text", "text": "answer"}
			]}
		]
	}`)

	inbound := &MessagesInbound{}
	req, err := inbound.TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var assistant *transformerModel.Message
	for i := range req.Messages {
		if req.Messages[i].Role == "assistant" {
			assistant = &req.Messages[i]
			break
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant message parsed, got %#v", req.Messages)
	}

	if !transformerModel.IsRedactedThinkingSignature(assistant.ReasoningSignature) {
		t.Fatalf("assistant ReasoningSignature is not redacted-marked: %#v", assistant.ReasoningSignature)
	}
	data, ok := transformerModel.DecodeRedactedThinkingSignature(*assistant.ReasoningSignature)
	if !ok {
		t.Fatalf("DecodeRedactedThinkingSignature failed for %q", *assistant.ReasoningSignature)
	}
	if data != blob {
		t.Fatalf("decoded redacted data = %q, want %q", data, blob)
	}
}
