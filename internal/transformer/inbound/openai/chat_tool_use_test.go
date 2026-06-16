package openai

import (
	"context"
	"testing"
)

func TestChatInboundNormalizesLegacyFunctionCall(t *testing.T) {
	req, err := (&ChatInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"look up octopus"},
			{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{\"q\":\"octopus\"}"}},
			{"role":"function","name":"lookup","content":"{\"ok\":true}"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", req.Messages)
	}

	assistant := req.Messages[1]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected legacy function_call to become one tool call, got %#v", assistant.ToolCalls)
	}
	toolCall := assistant.ToolCalls[0]
	if toolCall.ID == "" || toolCall.Function.Name != "lookup" || toolCall.Function.Arguments != `{"q":"octopus"}` {
		t.Fatalf("unexpected tool call: %#v", toolCall)
	}
	if assistant.FunctionCall != nil {
		t.Fatalf("expected legacy function_call field to be cleared, got %#v", assistant.FunctionCall)
	}

	tool := req.Messages[2]
	if tool.Role != "tool" {
		t.Fatalf("expected legacy function role to become tool, got %#v", tool.Role)
	}
	if tool.ToolCallID == nil || *tool.ToolCallID != toolCall.ID {
		t.Fatalf("expected function result to reference generated call id %q, got %#v", toolCall.ID, tool.ToolCallID)
	}
	if tool.ToolCallName == nil || *tool.ToolCallName != "lookup" {
		t.Fatalf("expected function result to keep tool name, got %#v", tool.ToolCallName)
	}
}
