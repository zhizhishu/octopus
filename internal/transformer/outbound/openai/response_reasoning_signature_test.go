package openai

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertToLLMResponseFromResponsesCapturesEncryptedContent pins the
// non-stream path: a reasoning output item's encrypted_content must land in
// Message.ReasoningSignature so the next turn can replay it.
func TestConvertToLLMResponseFromResponsesCapturesEncryptedContent(t *testing.T) {
	encrypted := "gAAAAABencrypted-nonstream"
	summary := "kept summary"
	status := "completed"
	text := "final"
	resp := &ResponsesResponse{
		ID:     "resp_1",
		Model:  "gpt-5.5",
		Status: &status,
		Output: []ResponsesItem{{
			Type: "reasoning",
			Summary: []ResponsesReasoningSummary{{
				Type: "summary_text",
				Text: summary,
			}},
			EncryptedContent: &encrypted,
		}, {
			Type: "message",
			Role: "assistant",
			Content: &ResponsesInput{Items: []ResponsesItem{{
				Type: "output_text",
				Text: &text,
			}}},
		}},
	}

	got := convertToLLMResponseFromResponses(resp)
	if got == nil || len(got.Choices) != 1 || got.Choices[0].Message == nil {
		t.Fatalf("expected one assistant choice, got %#v", got)
	}
	msg := got.Choices[0].Message
	if msg.ReasoningContent == nil || *msg.ReasoningContent != summary {
		t.Fatalf("expected reasoning summary %q, got %#v", summary, msg.ReasoningContent)
	}
	// The encrypted_content is captured OpenAI-tagged so a cross-protocol replay
	// cannot emit it as another provider's signature; it decodes back to the raw.
	if raw, ok := model.OpenAIEncryptedContent(msg.ReasoningSignature); !ok || raw != encrypted {
		t.Fatalf("expected OpenAI-tagged reasoning signature decoding to %q, got %#v", encrypted, msg.ReasoningSignature)
	}
}

// TestResponseOutboundStreamLiftsReasoningEncryptedContent pins the stream
// path: response.output_item.done for a reasoning item must lift
// encrypted_content into Delta.ReasoningSignature (previously non-tool
// output_item.done returned nil and dropped the blob).
func TestResponseOutboundStreamLiftsReasoningEncryptedContent(t *testing.T) {
	outbound := &ResponseOutbound{}
	encrypted := "gAAAAABencrypted-stream"
	event := `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + encrypted + `","summary":[{"type":"summary_text","text":"thinking"}]}}`

	resp, err := outbound.TransformStream(context.Background(), []byte(event))
	if err != nil {
		t.Fatalf("TransformStream returned error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].Delta == nil {
		t.Fatalf("expected a reasoning delta, got %#v", resp)
	}
	delta := resp.Choices[0].Delta
	// The done branch lifts ONLY the encrypted_content (OpenAI-tagged); it decodes
	// back to the raw blob.
	if raw, ok := model.OpenAIEncryptedContent(delta.ReasoningSignature); !ok || raw != encrypted {
		t.Fatalf("expected OpenAI-tagged ReasoningSignature decoding to %q, got %#v", encrypted, delta.ReasoningSignature)
	}
	// The item.summary text must NOT be emitted here: it is already streamed via
	// response.reasoning_summary_text.delta, so lifting it again double-counts it.
	if delta.ReasoningContent != nil {
		t.Fatalf("expected no summary ReasoningContent in the done branch, got %#v", delta.ReasoningContent)
	}
}
