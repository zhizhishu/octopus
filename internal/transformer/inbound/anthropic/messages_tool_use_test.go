package anthropic

import (
	"context"
	"testing"
)

func TestTransformRequestPreservesToolChoiceAndParallelPreference(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":256,
		"tools":[
			{
				"name":"lookup",
				"description":"lookup data",
				"input_schema":{"type":"object","properties":{"query":{"type":"string"}}}
			}
		],
		"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},
		"messages":[
			{"role":"user","content":"find data"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"data"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"result"}],"is_error":false},
				{"type":"text","text":"thanks"}
			]}
		]
	}`)

	req, err := (&MessagesInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil {
		t.Fatalf("expected named tool choice, got %#v", req.ToolChoice)
	}
	if req.ToolChoice.NamedToolChoice.Type != "function" || req.ToolChoice.NamedToolChoice.Function.Name != "lookup" {
		t.Fatalf("unexpected named tool choice: %#v", req.ToolChoice.NamedToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("expected parallel tool calls to be disabled, got %#v", req.ParallelToolCalls)
	}

	var toolMsgFound bool
	var pairedUserFound bool
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			toolMsgFound = true
			if msg.ToolCallID == nil || *msg.ToolCallID != "toolu_1" {
				t.Fatalf("expected tool_call_id toolu_1, got %#v", msg.ToolCallID)
			}
			if msg.MessageIndex == nil || *msg.MessageIndex != 2 {
				t.Fatalf("expected tool result to keep source message index 2, got %#v", msg.MessageIndex)
			}
		}
		if msg.Role == "user" && msg.MessageIndex != nil && *msg.MessageIndex == 2 {
			pairedUserFound = true
			if msg.Content.Content == nil || *msg.Content.Content != "thanks" {
				t.Fatalf("expected paired user text to survive, got %#v", msg.Content)
			}
		}
	}
	if !toolMsgFound {
		t.Fatalf("expected tool result message to be preserved: %#v", req.Messages)
	}
	if !pairedUserFound {
		t.Fatalf("expected user content paired with tool_result to be preserved: %#v", req.Messages)
	}
}

func TestTransformRequestKeepsEmptyToolResult(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_empty"}]}
		]
	}`)

	req, err := (&MessagesInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected one tool message, got %#v", req.Messages)
	}
	msg := req.Messages[0]
	if msg.Role != "tool" {
		t.Fatalf("expected tool message, got %#v", msg)
	}
	if msg.ToolCallID == nil || *msg.ToolCallID != "toolu_empty" {
		t.Fatalf("expected tool_call_id toolu_empty, got %#v", msg.ToolCallID)
	}
	if msg.Content.Content == nil || *msg.Content.Content != "" {
		t.Fatalf("expected empty string tool result content, got %#v", msg.Content)
	}
}
