package gemini

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

// TestGenerateContentInboundStreamAggregationMatchesLegacyConcat feeds a
// many-fragment stream through the gemini aggregator and compares it against the
// exact `s += frag` growth it used before fragments were buffered in a
// strings.Builder. Same fragments, same order, so the aggregated value must be
// byte-identical.
func TestGenerateContentInboundStreamAggregationMatchesLegacyConcat(t *testing.T) {
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
			ID:    "resp_1",
			Model: "gemini-2.5-pro",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					Role:             "assistant",
					Content:          transformerModel.MessageContent{Content: &text},
					ReasoningContent: &reasoning,
					ToolCalls: []transformerModel.ToolCall{
						{Index: 0, ID: "call_0", Type: "function", Function: transformerModel.FunctionCall{Name: "write", Arguments: args0}},
						{Index: 1, ID: "call_1", Type: "function", Function: transformerModel.FunctionCall{Name: "read", Arguments: args1}},
					},
				},
			}},
		})
	}

	inbound := &GenerateContentInbound{streamChunks: chunks}
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
	if msg.ToolCalls[0].Index != 0 || msg.ToolCalls[0].ID != "call_0" || msg.ToolCalls[0].Function.Name != "write" {
		t.Fatalf("unexpected first tool call metadata: %#v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].Index != 1 || msg.ToolCalls[1].ID != "call_1" || msg.ToolCalls[1].Function.Name != "read" {
		t.Fatalf("unexpected second tool call metadata: %#v", msg.ToolCalls[1])
	}
	assertAggregated(t, "tool call 0 arguments", msg.ToolCalls[0].Function.Arguments, wantArgs0)
	assertAggregated(t, "tool call 1 arguments", msg.ToolCalls[1].Function.Arguments, wantArgs1)
}

// TestGenerateContentInboundStreamAggregationKeepsNilAndEmptyDistinction pins the
// nil vs empty-string behavior the previous `new(string)` + `+=` form had: a
// choice that received an (even empty) fragment gets a non-nil pointer, one that
// never received that field keeps nil.
func TestGenerateContentInboundStreamAggregationKeepsNilAndEmptyDistinction(t *testing.T) {
	empty := ""
	inbound := &GenerateContentInbound{streamChunks: []*transformerModel.InternalLLMResponse{
		{
			ID:    "resp_2",
			Model: "gemini-2.5-pro",
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
	// The legacy gemini reasoning merge only materialized on a non-empty fragment
	// (`GetReasoningContent() != ""`), so an empty reasoning fragment keeps nil.
	if first.ReasoningContent != nil {
		t.Fatalf("expected nil reasoning for an empty reasoning fragment (legacy gate), got %q", *first.ReasoningContent)
	}

	second := result.Choices[1].Message
	if second.Content.Content != nil {
		t.Fatalf("expected nil content for the choice that never got a fragment, got %q", *second.Content.Content)
	}
	if second.ReasoningContent != nil {
		t.Fatalf("expected nil reasoning for the choice that never got a fragment, got %q", *second.ReasoningContent)
	}
}

// TestConvertGeminiContentThoughtPartsMatchLegacyConcat pins the request-side
// thought-part accumulation: multiple thought parts concatenate in order (the old
// `*msg.ReasoningContent += text` growth), a thought-free message keeps nil
// ReasoningContent, and a thought-only message is still kept as an assistant turn.
func TestConvertGeminiContentThoughtPartsMatchLegacyConcat(t *testing.T) {
	content := &transformerModel.GeminiContent{
		Role: "model",
		Parts: []*transformerModel.GeminiPart{
			{Text: "think-a ", Thought: true},
			{Text: "visible one"},
			{Text: "think-b ", Thought: true},
			{Text: "think-c", Thought: true},
		},
	}
	converted := convertGeminiContent(content, 0, map[string][]string{})
	if len(converted) != 1 {
		t.Fatalf("expected 1 converted message, got %#v", converted)
	}
	msg := converted[0]
	if msg.ReasoningContent == nil {
		t.Fatalf("expected reasoning content from thought parts, got nil")
	}
	assertAggregated(t, "thought reasoning", *msg.ReasoningContent, "think-a think-b think-c")
	if msg.Content.Content == nil || *msg.Content.Content != "visible one" {
		t.Fatalf("expected plain text preserved, got %#v", msg.Content.Content)
	}

	// Thought-free message keeps nil ReasoningContent.
	plain := convertGeminiContent(&transformerModel.GeminiContent{
		Role:  "model",
		Parts: []*transformerModel.GeminiPart{{Text: "hello"}},
	}, 1, map[string][]string{})
	if len(plain) != 1 || plain[0].ReasoningContent != nil {
		t.Fatalf("expected nil reasoning for a thought-free message, got %#v", plain)
	}

	// Thought-only message must survive (the keep/drop check reads ReasoningContent).
	thoughtOnly := convertGeminiContent(&transformerModel.GeminiContent{
		Role:  "model",
		Parts: []*transformerModel.GeminiPart{{Text: "only-think", Thought: true}},
	}, 2, map[string][]string{})
	if len(thoughtOnly) != 1 {
		t.Fatalf("expected thought-only message to be kept, got %#v", thoughtOnly)
	}
	if thoughtOnly[0].ReasoningContent == nil || *thoughtOnly[0].ReasoningContent != "only-think" {
		t.Fatalf("expected thought-only reasoning preserved, got %#v", thoughtOnly[0].ReasoningContent)
	}
}
