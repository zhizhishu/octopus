package authropic

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// TestFilterOutResponsesCustomTools verifies that a Responses custom (freeform)
// tool call is stripped from Claude-bound history together with its paired tool
// result, while standard function tool calls, their results, and message text
// are preserved.
func TestFilterOutResponsesCustomTools(t *testing.T) {
	messages := []model.Message{
		{Role: "user", Content: model.MessageContent{Content: lo.ToPtr("hi")}},
		{
			Role:    "assistant",
			Content: model.MessageContent{Content: lo.ToPtr("working")},
			ToolCalls: []model.ToolCall{
				{ID: "call_std", Type: "function", Function: model.FunctionCall{Name: "get_weather", Arguments: `{"city":"Tokyo"}`}},
				{ID: "call_custom", Type: model.ToolCallTypeCustom, Function: model.FunctionCall{Name: "exec", Arguments: "const r = tools.exec();"}},
			},
		},
		{Role: "tool", ToolCallID: lo.ToPtr("call_std"), Content: model.MessageContent{Content: lo.ToPtr("sunny")}},
		{Role: "tool", ToolCallID: lo.ToPtr("call_custom"), Content: model.MessageContent{Content: lo.ToPtr("done")}},
	}

	out := filterOutResponsesCustomTools(messages)

	var asst *model.Message
	var toolIDs []string
	for i := range out {
		switch out[i].Role {
		case "assistant":
			asst = &out[i]
		case "tool":
			if out[i].ToolCallID != nil {
				toolIDs = append(toolIDs, *out[i].ToolCallID)
			}
		}
	}
	if asst == nil || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_std" {
		t.Fatalf("custom tool call not stripped / standard call lost: %+v", asst)
	}
	if asst.Content.Content == nil || *asst.Content.Content != "working" {
		t.Errorf("assistant text should be preserved, got %+v", asst.Content)
	}
	if len(toolIDs) != 1 || toolIDs[0] != "call_std" {
		t.Errorf("tool results after filter = %v, want only [call_std] (custom result dropped)", toolIDs)
	}
}

// TestFilterOutResponsesCustomToolsDropsEmptyAssistant verifies that an assistant
// turn that carried only a custom tool call (no text) is dropped entirely, along
// with its orphaned result, rather than encoded as an empty message.
func TestFilterOutResponsesCustomToolsDropsEmptyAssistant(t *testing.T) {
	messages := []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Type: model.ToolCallTypeCustom, Function: model.FunctionCall{Name: "exec", Arguments: "code"}}}},
		{Role: "tool", ToolCallID: lo.ToPtr("c1"), Content: model.MessageContent{Content: lo.ToPtr("out")}},
		{Role: "user", Content: model.MessageContent{Content: lo.ToPtr("next")}},
	}

	out := filterOutResponsesCustomTools(messages)

	for _, m := range out {
		if m.Role == "assistant" {
			t.Errorf("custom-only assistant turn should be dropped, got %+v", m)
		}
		if m.Role == "tool" {
			t.Errorf("orphaned custom tool result should be dropped, got %+v", m)
		}
	}
	if len(out) != 1 || out[0].Role != "user" {
		t.Errorf("expected only the user message to remain, got %d messages", len(out))
	}
}
