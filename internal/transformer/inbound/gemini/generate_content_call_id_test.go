package gemini

import (
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"testing"
)

func TestNativeGeminiFunctionResponseIDsFollowCalls(t *testing.T) {
	original := &model.GeminiGenerateContentRequest{Contents: []*model.GeminiContent{
		{Role: "model", Parts: []*model.GeminiPart{
			{FunctionCall: &model.GeminiFunctionCall{ID: "id-1", Name: "lookup", Args: map[string]interface{}{"x": 1}}},
			{FunctionCall: &model.GeminiFunctionCall{ID: "id-2", Name: "lookup", Args: map[string]interface{}{"x": 2}}},
		}},
		{Role: "user", Parts: []*model.GeminiPart{
			{FunctionResponse: &model.GeminiFunctionResponse{ID: "id-2", Name: "lookup", Response: map[string]interface{}{"result": "second"}}},
			{FunctionResponse: &model.GeminiFunctionResponse{ID: "id-1", Name: "lookup", Response: map[string]interface{}{"result": "first"}}},
		}},
	}}
	msg := convertGeminiMessages(original)
	if len(msg) != 3 || len(msg[0].ToolCalls) != 2 {
		t.Fatalf("lost Gemini call turns: %#v", msg)
	}
	if msg[0].ToolCalls[0].ID != "id-1" || msg[0].ToolCalls[1].ID != "id-2" {
		t.Fatalf("call IDs changed: %#v", msg[0].ToolCalls)
	}
	if msg[1].ToolCallID == nil || *msg[1].ToolCallID != "id-2" || msg[2].ToolCallID == nil || *msg[2].ToolCallID != "id-1" {
		t.Fatalf("response IDs mismatched in out-of-order parallel results: %#v", msg)
	}
	fromInternal := convertMessageToGeminiContent(&msg[0])
	if len(fromInternal.Parts) != 2 || fromInternal.Parts[0].FunctionCall.ID != "id-1" || fromInternal.Parts[1].FunctionCall.ID != "id-2" {
		t.Fatalf("native function calls lost upstream ids: %#v", fromInternal)
	}
}
