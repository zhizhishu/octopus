package authropic

import (
	"testing"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// assistantContentBlocks returns the content blocks of the first assistant message
// produced by convertToAnthropicRequest.
func assistantContentBlocks(t *testing.T, msgs []anthropicModel.MessageParam) []anthropicModel.MessageContentBlock {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m.Content.MultipleContent
		}
	}
	t.Fatalf("no assistant message found in %#v", msgs)
	return nil
}

func blockTypesPresent(blocks []anthropicModel.MessageContentBlock) map[string]int {
	counts := map[string]int{}
	for _, b := range blocks {
		counts[b.Type]++
	}
	return counts
}

// TestAnthropicOutboundDropsUnsignedThinkingWithToolCalls pins that
// convertAssistantWithToolCalls emits a thinking block ONLY when the reasoning
// carries a signature. Anthropic 400s on a thinking block without a valid
// signature, so an unsigned reasoning block (e.g. crossing codex Responses ->
// Anthropic where claude's thinking.signature was never surfaced) must be dropped
// while the tool_use / text blocks survive (the assistant message stays non-empty).
func TestAnthropicOutboundDropsUnsignedThinkingWithToolCalls(t *testing.T) {
	newReq := func(sig *string) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: "claude-sonnet-4-5",
			Messages: []model.Message{
				{Role: "user", Content: model.MessageContent{Content: stringPtr("whats the weather?")}},
				{
					Role:               "assistant",
					ReasoningContent:   stringPtr("I should call get_weather"),
					ReasoningSignature: sig,
					Content:            model.MessageContent{Content: stringPtr("calling the tool")},
					ToolCalls: []model.ToolCall{{
						ID:       "toolu_1",
						Type:     "function",
						Function: model.FunctionCall{Name: "get_weather", Arguments: `{"city":"Beijing"}`},
					}},
				},
			},
		}
	}

	// (a) unsigned reasoning -> NO thinking block, but tool_use + text present.
	blocks := assistantContentBlocks(t, convertToAnthropicRequest(newReq(nil)).Messages)
	counts := blockTypesPresent(blocks)
	if counts["thinking"] != 0 {
		t.Fatalf("unsigned reasoning must not emit a thinking block, got blocks %#v", blocks)
	}
	if counts["tool_use"] != 1 {
		t.Fatalf("expected the tool_use block to survive, got blocks %#v", blocks)
	}
	if counts["text"] != 1 {
		t.Fatalf("expected the text block to survive, got blocks %#v", blocks)
	}
	if len(blocks) == 0 {
		t.Fatalf("assistant message must not be left empty")
	}

	// (b) signed reasoning -> thinking block IS present with that signature
	// (claude->claude regression guard).
	sig := "sig-abc123"
	signedBlocks := assistantContentBlocks(t, convertToAnthropicRequest(newReq(&sig)).Messages)
	var thinking *anthropicModel.MessageContentBlock
	for i := range signedBlocks {
		if signedBlocks[i].Type == "thinking" {
			thinking = &signedBlocks[i]
			break
		}
	}
	if thinking == nil {
		t.Fatalf("signed reasoning must emit a thinking block, got blocks %#v", signedBlocks)
	}
	if thinking.Signature == nil || *thinking.Signature != sig {
		t.Fatalf("thinking block signature = %#v, want %q", thinking.Signature, sig)
	}
	if thinking.Thinking == nil || *thinking.Thinking != "I should call get_weather" {
		t.Fatalf("thinking block text = %#v, want %q", thinking.Thinking, "I should call get_weather")
	}
	if blockTypesPresent(signedBlocks)["tool_use"] != 1 {
		t.Fatalf("expected the tool_use block alongside the signed thinking block, got %#v", signedBlocks)
	}
}

// TestAnthropicOutboundDropsUnsignedThinkingTextOnly pins the same signature gate on
// the buildMultipleContentWithThinking path (assistant message with reasoning + plain
// text content, no tool calls). The text block is always appended, so the message is
// never left empty; the thinking block appears only when a signature is present.
func TestAnthropicOutboundDropsUnsignedThinkingTextOnly(t *testing.T) {
	newReq := func(sig *string) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: "claude-sonnet-4-5",
			Messages: []model.Message{
				{Role: "user", Content: model.MessageContent{Content: stringPtr("hello")}},
				{
					Role:               "assistant",
					ReasoningContent:   stringPtr("thinking about the greeting"),
					ReasoningSignature: sig,
					Content:            model.MessageContent{Content: stringPtr("hi there")},
				},
			},
		}
	}

	// (a) unsigned -> no thinking block, text block survives.
	blocks := assistantContentBlocks(t, convertToAnthropicRequest(newReq(nil)).Messages)
	counts := blockTypesPresent(blocks)
	if counts["thinking"] != 0 {
		t.Fatalf("unsigned reasoning must not emit a thinking block, got blocks %#v", blocks)
	}
	if counts["text"] != 1 {
		t.Fatalf("expected the text block to survive, got blocks %#v", blocks)
	}

	// (b) signed -> thinking block present with the signature.
	sig := "sig-textonly-9"
	signedBlocks := assistantContentBlocks(t, convertToAnthropicRequest(newReq(&sig)).Messages)
	var thinking *anthropicModel.MessageContentBlock
	for i := range signedBlocks {
		if signedBlocks[i].Type == "thinking" {
			thinking = &signedBlocks[i]
			break
		}
	}
	if thinking == nil {
		t.Fatalf("signed reasoning must emit a thinking block, got blocks %#v", signedBlocks)
	}
	if thinking.Signature == nil || *thinking.Signature != sig {
		t.Fatalf("thinking block signature = %#v, want %q", thinking.Signature, sig)
	}
}
