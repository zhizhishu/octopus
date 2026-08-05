package anthropic

import (
	"context"
	"fmt"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// diffAt reports the first byte offset where two aggregated strings differ, so a
// failure points at the fragment that went wrong instead of dumping both strings.
func diffAt(got, want string) int {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	for i := 0; i < limit; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return limit
	}
	return -1
}

func assertAggregated(t *testing.T, label, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	at := diffAt(got, want)
	t.Fatalf("%s: aggregated %d bytes, want %d bytes, first difference at offset %d", label, len(got), len(want), at)
}

// TestMessagesInboundStreamAggregationMatchesLegacyConcat feeds a many-fragment
// stream through the messages aggregator and compares it against the exact
// `s += frag` growth it used before fragments were buffered in a strings.Builder.
// Same fragments, same order, so the aggregated value must be byte-identical.
func TestMessagesInboundStreamAggregationMatchesLegacyConcat(t *testing.T) {
	const fragments = 300

	var chunks []*transformerModel.InternalLLMResponse
	wantContent := ""
	wantReasoning := ""
	wantArgs0 := ""
	wantArgs1 := ""

	for n := 0; n < fragments; n++ {
		text := fmt.Sprintf("chunk-%d ", n)
		reasoning := fmt.Sprintf("think-%d ", n)
		args0 := fmt.Sprintf(`"k%d":%d,`, n, n)
		args1 := fmt.Sprintf(`"m%d":%d,`, n, n)

		// The legacy aggregation, verbatim.
		wantContent += text
		wantReasoning += reasoning
		wantArgs0 += args0
		wantArgs1 += args1

		chunks = append(chunks, &transformerModel.InternalLLMResponse{
			ID:    "msg_1",
			Model: "claude-sonnet-4-5",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					Role:             "assistant",
					Content:          transformerModel.MessageContent{Content: &text},
					ReasoningContent: &reasoning,
					ToolCalls: []transformerModel.ToolCall{
						{Index: 0, ID: "toolu_0", Type: "function", Function: transformerModel.FunctionCall{Name: "write", Arguments: args0}},
						{Index: 1, ID: "toolu_1", Type: "function", Function: transformerModel.FunctionCall{Name: "read", Arguments: args1}},
					},
				},
			}},
		})
	}

	inbound := &MessagesInbound{streamChunks: chunks}
	result, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if result == nil || len(result.Choices) != 1 {
		t.Fatalf("expected 1 aggregated choice, got %#v", result)
	}

	msg := result.Choices[0].Message
	if msg == nil || msg.Content.Content == nil || msg.ReasoningContent == nil {
		t.Fatalf("expected aggregated content and reasoning, got %#v", msg)
	}
	if msg.Role != "assistant" {
		t.Fatalf("expected role to survive aggregation, got %q", msg.Role)
	}
	assertAggregated(t, "content", *msg.Content.Content, wantContent)
	assertAggregated(t, "reasoning", *msg.ReasoningContent, wantReasoning)

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 aggregated tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Index != 0 || msg.ToolCalls[0].ID != "toolu_0" || msg.ToolCalls[0].Function.Name != "write" {
		t.Fatalf("unexpected first tool call metadata: %#v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].Index != 1 || msg.ToolCalls[1].ID != "toolu_1" || msg.ToolCalls[1].Function.Name != "read" {
		t.Fatalf("unexpected second tool call metadata: %#v", msg.ToolCalls[1])
	}
	assertAggregated(t, "tool call 0 arguments", msg.ToolCalls[0].Function.Arguments, wantArgs0)
	assertAggregated(t, "tool call 1 arguments", msg.ToolCalls[1].Function.Arguments, wantArgs1)
}

// TestMessagesInboundStreamAggregationKeepsNilAndEmptyDistinction pins the nil vs
// empty-string behavior the previous `new(string)` + `+=` form had: a choice that
// received an (even empty) fragment gets a non-nil pointer, one that never
// received that field keeps nil.
func TestMessagesInboundStreamAggregationKeepsNilAndEmptyDistinction(t *testing.T) {
	empty := ""
	inbound := &MessagesInbound{streamChunks: []*transformerModel.InternalLLMResponse{
		{
			ID:    "msg_2",
			Model: "claude-sonnet-4-5",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{Role: "assistant", Content: transformerModel.MessageContent{Content: &empty}, ReasoningContent: &empty},
			}},
		},
		{
			Choices: []transformerModel.Choice{{
				Index: 1,
				Delta: &transformerModel.Message{Role: "assistant"},
			}},
		},
	}}

	result, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if result == nil || len(result.Choices) != 2 {
		t.Fatalf("expected 2 aggregated choices, got %#v", result)
	}

	first := result.Choices[0].Message
	if first.Content.Content == nil || *first.Content.Content != "" {
		t.Fatalf("expected non-nil empty content for the choice that got an empty fragment, got %#v", first.Content.Content)
	}
	if first.ReasoningContent == nil || *first.ReasoningContent != "" {
		t.Fatalf("expected non-nil empty reasoning for the choice that got an empty fragment, got %#v", first.ReasoningContent)
	}

	second := result.Choices[1].Message
	if second.Content.Content != nil {
		t.Fatalf("expected nil content for the choice that never got a fragment, got %q", *second.Content.Content)
	}
	if second.ReasoningContent != nil {
		t.Fatalf("expected nil reasoning for the choice that never got a fragment, got %q", *second.ReasoningContent)
	}
}
