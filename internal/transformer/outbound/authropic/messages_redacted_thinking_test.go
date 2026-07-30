package authropic

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertMultiplePartContentPrependsSignedThinking pins that a multipart
// assistant message carrying reasoning + a real signature emits a signed
// thinking block as its FIRST content block. Before the fix convertMultiplePartContent
// never prepended a thinking block (unlike the tool-call / text-only paths), so a
// multipart turn lost its thinking-continuity metadata.
func TestConvertMultiplePartContentPrependsSignedThinking(t *testing.T) {
	sig := "sig-multipart-123"
	msg := model.Message{
		Role:               "assistant",
		ReasoningContent:   stringPtr("let me think about both parts"),
		ReasoningSignature: &sig,
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: stringPtr("part one")},
				{Type: "text", Text: stringPtr("part two")},
			},
		},
	}

	blocks := convertMultiplePartContent(msg).MultipleContent
	if len(blocks) == 0 {
		t.Fatalf("expected content blocks, got none")
	}
	if blocks[0].Type != "thinking" {
		t.Fatalf("first block type = %q, want %q; blocks=%#v", blocks[0].Type, "thinking", blocks)
	}
	if blocks[0].Signature == nil || *blocks[0].Signature != sig {
		t.Fatalf("first block signature = %#v, want %q", blocks[0].Signature, sig)
	}
	if blocks[0].Thinking == nil || *blocks[0].Thinking != "let me think about both parts" {
		t.Fatalf("first block thinking = %#v, want %q", blocks[0].Thinking, "let me think about both parts")
	}
	// The original parts must still follow the thinking block.
	if blockTypesPresent(blocks)["text"] != 2 {
		t.Fatalf("expected both text parts to survive after the thinking block, got %#v", blocks)
	}
}

// TestConvertMultiplePartContentPrependsRedactedThinking pins that a multipart
// assistant message whose only reasoning is a redacted-marked signature (no
// visible thinking text) emits a redacted_thinking block — carrying the decoded
// opaque data — as its FIRST content block, so the payload is replayed to the
// upstream on the next turn.
func TestConvertMultiplePartContentPrependsRedactedThinking(t *testing.T) {
	const blob = "REDACTED_BLOB_ABC=="
	sig := model.EncodeRedactedThinkingSignature(blob)
	msg := model.Message{
		Role:               "assistant",
		ReasoningSignature: &sig,
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: stringPtr("visible answer")},
			},
		},
	}

	blocks := convertMultiplePartContent(msg).MultipleContent
	if len(blocks) == 0 {
		t.Fatalf("expected content blocks, got none")
	}
	if blocks[0].Type != "redacted_thinking" {
		t.Fatalf("first block type = %q, want %q; blocks=%#v", blocks[0].Type, "redacted_thinking", blocks)
	}
	if blocks[0].Data != blob {
		t.Fatalf("first block data = %q, want %q", blocks[0].Data, blob)
	}
}
