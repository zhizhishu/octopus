package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestResponseInboundNonStreamCarriesEncryptedContent pins the non-stream fix:
// convertToResponsesAPIResponse must round-trip claude's thinking.signature as the
// reasoning item's encrypted_content so a codex client can replay it next turn
// (prevents the unsigned-thinking-block 400 on the following turn).
func TestResponseInboundNonStreamCarriesEncryptedContent(t *testing.T) {
	sig := "claude-sig-nonstream-xyz"
	resp := &model.InternalLLMResponse{
		ID:     "resp_reasoning",
		Object: "chat.completion",
		Model:  "claude-opus-4-8",
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role:               "assistant",
				ReasoningContent:   ptr("deep thought"),
				ReasoningSignature: ptr(sig),
				Content:            model.MessageContent{Content: ptr("final answer")},
			},
			FinishReason: ptr("stop"),
		}},
	}

	body, err := (&ResponseInbound{}).TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse returned error: %v", err)
	}

	var out ResponsesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("failed to decode responses api response %q: %v", string(body), err)
	}

	var reasoning *ResponsesItem
	for i := range out.Output {
		if out.Output[i].Type == "reasoning" {
			reasoning = &out.Output[i]
			break
		}
	}
	if reasoning == nil {
		t.Fatalf("expected a reasoning output item, got %s", string(body))
	}
	if reasoning.EncryptedContent == nil || *reasoning.EncryptedContent != sig {
		t.Fatalf("reasoning.encrypted_content = %#v, want %q (body: %s)", reasoning.EncryptedContent, sig, string(body))
	}
}

// TestResponseInboundStreamCarriesEncryptedContent pins the stream fix: a
// signature_delta -> Delta.ReasoningSignature chunk is captured into
// i.reasoningSignature, and closeReasoningItem emits it as the reasoning
// output_item.done's encrypted_content.
func TestResponseInboundStreamCarriesEncryptedContent(t *testing.T) {
	inbound := &ResponseInbound{}
	sig := "claude-sig-stream-xyz"
	finishReason := "stop"

	var raw []byte
	feed := func(stream *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		raw = append(raw, out...)
	}

	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{
			ID:      "chatcmpl_reasoning",
			Model:   "claude-opus-4-8",
			Object:  "chat.completion.chunk",
			Created: 123,
		}
	}

	// chunk 1: reasoning text (opens the reasoning output item).
	c1 := base()
	c1.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: ptr("thinking...")}}}
	feed(c1)

	// chunk 2: claude's signature arrives in its own signature_delta chunk.
	c2 := base()
	c2.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningSignature: ptr(sig)}}}
	feed(c2)

	// chunk 3: finish -> closes the reasoning item (output_item.done carries encrypted_content).
	c3 := base()
	c3.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(c3)

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	raw = append(raw, done...)

	events := parseResponsesStreamEvents(t, string(raw))

	var reasoningDone *ResponsesItem
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "reasoning" {
			reasoningDone = ev.Item
			break
		}
	}
	if reasoningDone == nil {
		t.Fatalf("expected a reasoning output_item.done event, got %s", string(raw))
	}
	if reasoningDone.EncryptedContent == nil || *reasoningDone.EncryptedContent != sig {
		t.Fatalf("reasoning output_item.done encrypted_content = %#v, want %q (raw: %s)", reasoningDone.EncryptedContent, sig, string(raw))
	}
}
