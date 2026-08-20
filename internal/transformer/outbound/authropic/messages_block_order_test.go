package authropic

import (
	"testing"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// contentBlockTypes flattens a converted Anthropic message into its content block
// types so ordering assertions read as the contract they lock.
func contentBlockTypes(content anthropicModel.MessageContent) []string {
	types := make([]string, 0, len(content.MultipleContent))
	for _, block := range content.MultipleContent {
		types = append(types, block.Type)
	}
	return types
}

func assertBlockTypes(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected block types %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected block types %v, got %v", want, got)
		}
	}
}

// TestConvertMessagesKeepsTextBeforeToolUseWhenMergingAssistantTurns locks the
// Anthropic contract that an assistant content array must never place text after
// tool_use. The Responses inbound splits one upstream turn into several assistant
// messages (a function_call item and a message item each become their own
// message); convertMessages then concatenates them to satisfy Anthropic's
// role-alternation rule. Without reordering, that concatenation produced
// [text, tool_use, text] and Anthropic rejected the request with
// "assistant message N contains text after tool_use".
func TestConvertMessagesKeepsTextBeforeToolUseWhenMergingAssistantTurns(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: stringPtr("read the file")},
			},
			{
				Role:    "assistant",
				Content: model.MessageContent{Content: stringPtr("Let me check...")},
				ToolCalls: []model.ToolCall{{
					ID:   "call_abc",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "Read",
						Arguments: `{"file":"test.go"}`,
					},
				}},
			},
			{
				Role:    "assistant",
				Content: model.MessageContent{Content: stringPtr("Reading the file now")},
			},
		},
	}

	messages := convertMessages(req)

	if len(messages) != 2 {
		t.Fatalf("expected user + merged assistant message, got %d: %#v", len(messages), messages)
	}

	assistant := messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("expected merged message to be the assistant turn, got role %q", assistant.Role)
	}

	assertBlockTypes(t, contentBlockTypes(assistant.Content), []string{"text", "text", "tool_use"})
}

// TestConvertMessagesKeepsThinkingFirstWhenMergingAssistantTurns locks the other
// half of the ordering contract: a replayed thinking block must stay at the head
// of the merged assistant message even when the trailing message contributes
// plain text, because Anthropic requires thinking to lead the content array.
func TestConvertMessagesKeepsThinkingFirstWhenMergingAssistantTurns(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: stringPtr("plan then act")},
			},
			{
				Role:               "assistant",
				Content:            model.MessageContent{Content: stringPtr("Here is the plan.")},
				ReasoningContent:   stringPtr("thinking about the plan"),
				ReasoningSignature: stringPtr("sig-abc"),
				ToolCalls: []model.ToolCall{{
					ID:   "call_plan",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "Write",
						Arguments: `{"file":"plan.md"}`,
					},
				}},
			},
			{
				Role:    "assistant",
				Content: model.MessageContent{Content: stringPtr("Writing it out.")},
			},
		},
	}

	messages := convertMessages(req)

	if len(messages) != 2 {
		t.Fatalf("expected user + merged assistant message, got %d: %#v", len(messages), messages)
	}

	assertBlockTypes(t, contentBlockTypes(messages[1].Content), []string{"thinking", "text", "text", "tool_use"})
}

// TestReorderAssistantBlocksLeavesAlreadyValidOrderUntouched guards against the
// reorder pass rewriting a message that already satisfies the contract, so a
// genuine claude->claude turn keeps its original block sequence intact.
func TestReorderAssistantBlocksLeavesAlreadyValidOrderUntouched(t *testing.T) {
	first := "first"
	second := "second"
	blocks := []anthropicModel.MessageContentBlock{
		{Type: "thinking"},
		{Type: "text", Text: &first},
		{Type: "text", Text: &second},
		{Type: "tool_use"},
	}

	reordered := reorderAssistantBlocks(blocks)

	if len(reordered) != len(blocks) {
		t.Fatalf("expected block count to be preserved, got %d want %d", len(reordered), len(blocks))
	}
	for i := range blocks {
		if reordered[i].Type != blocks[i].Type {
			t.Fatalf("expected order preserved at %d: got %q want %q", i, reordered[i].Type, blocks[i].Type)
		}
	}
	if reordered[1].Text == nil || *reordered[1].Text != first {
		t.Fatalf("expected stable text order, got %#v", reordered[1].Text)
	}
	if reordered[2].Text == nil || *reordered[2].Text != second {
		t.Fatalf("expected stable text order, got %#v", reordered[2].Text)
	}
}

// TestReorderAssistantBlocksMovesTrailingTextAheadOfToolUse pins the raw
// reordering behaviour on the exact shape the upstream contract rejects.
func TestReorderAssistantBlocksMovesTrailingTextAheadOfToolUse(t *testing.T) {
	leading := "Let me check..."
	trailing := "Reading the file now"
	blocks := []anthropicModel.MessageContentBlock{
		{Type: "text", Text: &leading},
		{Type: "tool_use"},
		{Type: "text", Text: &trailing},
	}

	reordered := reorderAssistantBlocks(blocks)

	assertBlockTypes(t, contentBlockTypes(anthropicModel.MessageContent{MultipleContent: reordered}), []string{"text", "text", "tool_use"})
	if reordered[0].Text == nil || *reordered[0].Text != leading {
		t.Fatalf("expected leading text to stay first, got %#v", reordered[0].Text)
	}
	if reordered[1].Text == nil || *reordered[1].Text != trailing {
		t.Fatalf("expected trailing text to keep relative order, got %#v", reordered[1].Text)
	}
}
