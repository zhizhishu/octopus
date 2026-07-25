package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertAssistantMessageReEmitsCustomToolCall proves the outbound request
// builder re-emits a custom (freeform) tool call from history as a
// custom_tool_call carrying its freeform payload in `input`, NOT as a
// function_call carrying `arguments`. This is the second half of the multi-turn
// round-trip: after the inbound side preserves the custom marker
// (model.ToolCallTypeCustom), the outbound side must not flatten it back to a
// function_call, or a codex upstream that registered a custom tool rejects the
// history / the freeform payload lands in the wrong field.
func TestConvertAssistantMessageReEmitsCustomToolCall(t *testing.T) {
	payload := "const r = await tools.shell_command({\"command\":\"pwd\"});"
	msg := model.Message{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:       "call_1",
			Type:     model.ToolCallTypeCustom,
			Function: model.FunctionCall{Name: "exec", Arguments: payload},
		}},
	}

	items := convertAssistantMessageToResponses(msg)

	var tool *ResponsesItem
	for i := range items {
		if items[i].Type == "custom_tool_call" || items[i].Type == "function_call" {
			tool = &items[i]
			break
		}
	}
	if tool == nil {
		t.Fatalf("no tool-call item emitted, got %+v", items)
	}
	if tool.Type != "custom_tool_call" {
		t.Errorf("custom tool re-emitted as %q, want custom_tool_call", tool.Type)
	}
	if tool.Input != payload {
		t.Errorf("custom freeform payload not carried in input: got Input=%q Arguments=%q, want Input=%q",
			tool.Input, tool.Arguments, payload)
	}
}
