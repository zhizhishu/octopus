package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
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

// TestChatInboundStreamAggregationMatchesLegacyConcat feeds a many-fragment
// stream through the chat aggregator and compares it against the exact `s += frag`
// growth the aggregator used before it buffered fragments in a strings.Builder.
// Same fragments, same order, so the aggregated value must be byte-identical.
func TestChatInboundStreamAggregationMatchesLegacyConcat(t *testing.T) {
	const fragments = 300

	var chunks []*model.InternalLLMResponse
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

		chunks = append(chunks, &model.InternalLLMResponse{
			ID:    "chatcmpl_1",
			Model: "gpt-4o",
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{
					Role:             "assistant",
					Content:          model.MessageContent{Content: &text},
					ReasoningContent: &reasoning,
					ToolCalls: []model.ToolCall{
						{Index: 0, ID: "call_0", Type: "function", Function: model.FunctionCall{Name: "write", Arguments: args0}},
						{Index: 1, ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "read", Arguments: args1}},
					},
				},
			}},
		})
	}

	inbound := &ChatInbound{streamChunks: chunks}
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

// TestChatInboundStreamAggregationKeepsNilAndEmptyDistinction pins the nil vs
// empty-string behavior the previous `new(string)` + `+=` form had: a choice that
// received an (even empty) content fragment gets a non-nil pointer, one that never
// received content keeps nil. Chat reasoning only materializes on a non-empty
// fragment, because its guard is on the value, not the pointer.
func TestChatInboundStreamAggregationKeepsNilAndEmptyDistinction(t *testing.T) {
	empty := ""
	inbound := &ChatInbound{streamChunks: []*model.InternalLLMResponse{
		{
			ID:    "chatcmpl_2",
			Model: "gpt-4o",
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: &empty}, ReasoningContent: &empty},
			}},
		},
		{
			Choices: []model.Choice{{
				Index: 1,
				Delta: &model.Message{Role: "assistant"},
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
	if first.ReasoningContent != nil {
		t.Fatalf("expected empty reasoning fragment to stay unset, got %#v", *first.ReasoningContent)
	}

	second := result.Choices[1].Message
	if second.Content.Content != nil {
		t.Fatalf("expected nil content for the choice that never got a fragment, got %q", *second.Content.Content)
	}
	if second.ReasoningContent != nil {
		t.Fatalf("expected nil reasoning for the choice that never got a fragment, got %q", *second.ReasoningContent)
	}
}

// TestResponseInboundStreamAggregationMatchesLegacyConcat is the responses-inbound
// twin of the chat test above. Its reasoning guard is on the pointer, so an empty
// reasoning fragment must still materialize a non-nil empty value.
func TestResponseInboundStreamAggregationMatchesLegacyConcat(t *testing.T) {
	const fragments = 300

	var chunks []*model.InternalLLMResponse
	wantContent := ""
	wantReasoning := ""
	wantArgs := ""

	for n := 0; n < fragments; n++ {
		text := fmt.Sprintf("chunk-%d ", n)
		reasoning := fmt.Sprintf("think-%d ", n)
		args := fmt.Sprintf(`"k%d":%d,`, n, n)

		wantContent += text
		wantReasoning += reasoning
		wantArgs += args

		chunks = append(chunks, &model.InternalLLMResponse{
			ID:    "resp_1",
			Model: "gpt-5.5",
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{
					Role:             "assistant",
					Content:          model.MessageContent{Content: &text},
					ReasoningContent: &reasoning,
					ToolCalls: []model.ToolCall{
						{Index: 0, ID: "call_0", Type: "function", Function: model.FunctionCall{Name: "write", Arguments: args}},
					},
				},
			}},
		})
	}

	inbound := &ResponseInbound{streamChunks: chunks}
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
	assertAggregated(t, "content", *msg.Content.Content, wantContent)
	assertAggregated(t, "reasoning", *msg.ReasoningContent, wantReasoning)
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 aggregated tool call, got %d", len(msg.ToolCalls))
	}
	assertAggregated(t, "tool call arguments", msg.ToolCalls[0].Function.Arguments, wantArgs)

	empty := ""
	emptyReasoning := &ResponseInbound{streamChunks: []*model.InternalLLMResponse{{
		ID:      "resp_2",
		Model:   "gpt-5.5",
		Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: &empty}}},
	}}}
	emptyResult, err := emptyReasoning.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if emptyResult.Choices[0].Message.ReasoningContent == nil {
		t.Fatal("expected an empty reasoning fragment to still materialize a non-nil value")
	}
	if *emptyResult.Choices[0].Message.ReasoningContent != "" {
		t.Fatalf("expected empty reasoning, got %q", *emptyResult.Choices[0].Message.ReasoningContent)
	}
	if emptyResult.Choices[0].Message.Content.Content != nil {
		t.Fatal("expected nil content when no content fragment arrived")
	}
}

// TestResponseInboundStreamedToolArgumentsForwardUnchanged drives the live
// streaming path (not the end-of-stream aggregation): every argument fragment must
// still be forwarded verbatim as its own delta event, in order, and the finalized
// function_call must carry the exact concatenation the `+=` form produced.
func TestResponseInboundStreamedToolArgumentsForwardUnchanged(t *testing.T) {
	const fragments = 200

	inbound := &ResponseInbound{}
	wantArgs := ""
	sent := make([]string, 0, fragments+1)
	var forwarded strings.Builder

	for n := 0; n <= fragments; n++ {
		var frag string
		switch {
		case n == 0:
			frag = `{"k0":0`
		case n == fragments:
			frag = `}`
		default:
			frag = fmt.Sprintf(`,"k%d":%d`, n, n)
		}
		wantArgs += frag
		sent = append(sent, frag)

		out, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
			ID:      "resp_tool",
			Object:  "chat.completion.chunk",
			Model:   "gpt-5.5",
			Created: 1,
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{ToolCalls: []model.ToolCall{
					{Index: 0, ID: "call_0", Type: "function", Function: model.FunctionCall{Name: "write", Arguments: frag}},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("TransformStream fragment %d returned error: %v", n, err)
		}
		forwarded.Write(out)
	}

	// Every fragment reached the client as its own delta, in arrival order.
	deltas := make([]string, 0, len(sent))
	for _, event := range parseResponsesSSE(t, forwarded.String()) {
		if event.Type == "response.function_call_arguments.delta" {
			deltas = append(deltas, event.Delta)
		}
	}
	if len(deltas) != len(sent) {
		t.Fatalf("expected %d forwarded argument deltas, got %d", len(sent), len(deltas))
	}
	for i := range sent {
		if deltas[i] != sent[i] {
			t.Fatalf("forwarded delta %d changed: got %q, sent %q", i, deltas[i], sent[i])
		}
	}

	// The accumulated value every later reader sees is the plain concatenation.
	assertAggregated(t, "streamed tool arguments", inbound.toolCalls[0].Function.Arguments, wantArgs)

	finishReason := "tool_calls"
	final, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "resp_tool",
		Object:  "chat.completion.chunk",
		Model:   "gpt-5.5",
		Created: 1,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	})
	if err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	sawDone := false
	for _, event := range parseResponsesSSE(t, string(final)) {
		if event.Type != "response.function_call_arguments.done" {
			continue
		}
		sawDone = true
		assertAggregated(t, "function_call_arguments.done", event.Arguments, wantArgs)
	}
	if !sawDone {
		t.Fatal("expected a response.function_call_arguments.done event on the finish boundary")
	}
}

// parseResponsesSSE decodes the "data: {...}\n\n" frames the responses inbound emits.
func parseResponsesSSE(t *testing.T, raw string) []ResponsesStreamEvent {
	t.Helper()
	var events []ResponsesStreamEvent
	for _, line := range strings.Split(raw, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		var event ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("failed to decode emitted event %q: %v", payload, err)
		}
		events = append(events, event)
	}
	return events
}
