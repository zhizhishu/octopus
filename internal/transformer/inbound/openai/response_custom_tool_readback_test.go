package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// TestConvertItemCustomToolCallReadBackPreservesInput proves the multi-turn
// read-back path (convertItemToMessage) must preserve a custom_tool_call's
// freeform payload and its custom type.
//
// When a codex client sends a prior custom_tool_call back in the NEXT turn's
// input history, the freeform payload lives in the wire field `input`
// (ResponsesItem.Input), NOT `arguments`, and the item type is
// "custom_tool_call". Per model.ToolCallTypeCustom's contract, the internal
// round-trip must keep Type == ToolCallTypeCustom and carry the freeform text in
// Function.Arguments so the outbound side can re-emit a custom_tool_call.
//
// On the buggy code this FAILS: convertItemToMessage hardcodes Type "function"
// and reads item.Arguments (empty for custom tools), silently dropping the
// freeform payload -> the model "forgets" the tool call on multi-turn.
func TestConvertItemCustomToolCallReadBackPreservesInput(t *testing.T) {
	payload := "const r = await tools.shell_command({\"command\":\"pwd\"});"
	item := &ResponsesItem{
		Type:   "custom_tool_call",
		CallID: "call_1",
		Name:   "exec",
		Input:  lo.ToPtr(payload),
	}
	msg, err := convertItemToMessage(item)
	if err != nil {
		t.Fatalf("convertItemToMessage: %v", err)
	}
	if msg == nil || len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %+v", msg)
	}
	tc := msg.ToolCalls[0]
	if tc.Type != model.ToolCallTypeCustom {
		t.Errorf("custom tool type lost on read-back: got %q, want %q", tc.Type, model.ToolCallTypeCustom)
	}
	if tc.Function.Arguments != payload {
		t.Errorf("custom tool freeform payload lost on read-back: got %q, want %q", tc.Function.Arguments, payload)
	}
}
