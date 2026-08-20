package relay

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

func TestNormalizeChatToolCallPairingTolerance(t *testing.T) {
	callID := "call_upstream_123"
	replyID := "fc_client_456"
	replyContent := `{"result":"success"}`

	messages := []model.Message{
		{
			Role:    "user",
			Content: model.MessageContent{Content: lo.ToPtr("hello")},
		},
		{
			Role: "assistant",
			ToolCalls: []model.ToolCall{
				{
					ID:   callID,
					Type: "function",
					Function: model.FunctionCall{
						Name:      "test_tool",
						Arguments: "{}",
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: &replyID,
			Content:    model.MessageContent{Content: &replyContent},
		},
	}

	paired := normalizeChatToolCallPairing(messages)

	if len(paired) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(paired))
	}

	assistantMsg := paired[1]
	if len(assistantMsg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(assistantMsg.ToolCalls))
	}

	toolMsg := paired[2]
	if toolMsg.ToolCallID == nil || *toolMsg.ToolCallID != assistantMsg.ToolCalls[0].ID {
		t.Fatalf("expected matching tool_call_id, got toolMsg=%v assistant=%v", toolMsg.ToolCallID, assistantMsg.ToolCalls[0].ID)
	}
}
