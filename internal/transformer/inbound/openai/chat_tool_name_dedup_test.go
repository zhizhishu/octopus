package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestMergeToolCallDoesNotDuplicateName guards the downstream half of the
// streamed function-name duplication fix. Some upstream shapes repeat the full
// function name on every streamed chunk. The tool name is atomic (never a
// streamed fragment), so mergeToolCall must take the first non-empty value and
// never concatenate — otherwise the name becomes "get_weatherget_weather..."
// while the arguments (which ARE incremental) accumulate correctly.
func TestMergeToolCallDoesNotDuplicateName(t *testing.T) {
	var toolCalls []model.ToolCall

	// First delta establishes the tool call with its name and the first
	// argument fragment.
	toolCalls = mergeToolCall(toolCalls, model.ToolCall{
		Index:    0,
		ID:       "call_1",
		Type:     "function",
		Function: model.FunctionCall{Name: "get_weather", Arguments: `{"a":`},
	})
	// Subsequent deltas repeat the full name (as a Responses upstream does) with
	// incremental arguments.
	toolCalls = mergeToolCall(toolCalls, model.ToolCall{
		Index:    0,
		Function: model.FunctionCall{Name: "get_weather", Arguments: `1`},
	})
	toolCalls = mergeToolCall(toolCalls, model.ToolCall{
		Index:    0,
		Function: model.FunctionCall{Name: "get_weather", Arguments: `}`},
	})

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly 1 aggregated tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("expected name to stay %q, got %q (duplication regression)", "get_weather", toolCalls[0].Function.Name)
	}
	if toolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("expected arguments to accumulate to %q, got %q", `{"a":1}`, toolCalls[0].Function.Arguments)
	}
}
