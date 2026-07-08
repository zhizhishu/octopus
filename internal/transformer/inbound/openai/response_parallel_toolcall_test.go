package openai

import (
	"context"
	"testing"
)

// TestParallelToolCallsMergeIntoOneAssistantMessage guards the DeepSeek/Cursor fix.
//
// Cursor (Responses API) emits parallel tool calls in one assistant turn as two
// consecutive `function_call` items. They must convert to ONE assistant message
// whose tool_calls array holds both calls, followed by the two tool outputs.
// Emitting one assistant message per call produces an assistant→assistant→tool→tool
// sequence that strict OpenAI-compatible upstreams (DeepSeek/ele-deepseek) reject
// with a misleading "did not match any variant of untagged enum
// ChatCompletionToolChoiceOption" deserialize error.
func TestParallelToolCallsMergeIntoOneAssistantMessage(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list and read"}]},
			{"type":"function_call","call_id":"c0","name":"Glob","arguments":"{\"pattern\":\"*.go\"}"},
			{"type":"function_call","call_id":"c1","name":"ReadFile","arguments":"{\"path\":\"main.go\"}"},
			{"type":"function_call_output","call_id":"c0","output":"a.go"},
			{"type":"function_call_output","call_id":"c1","output":"package main"}
		],
		"tools":[{"type":"function","name":"Glob","parameters":{"type":"object"}},{"type":"function","name":"ReadFile","parameters":{"type":"object"}}],
		"tool_choice":"auto"
	}`)

	ir, err := (&ResponseInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	assistantToolMsgs, toolMsgs := 0, 0
	for _, m := range ir.Messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				assistantToolMsgs++
				if len(m.ToolCalls) != 2 {
					t.Errorf("parallel calls must merge into one assistant message with 2 tool_calls, got %d", len(m.ToolCalls))
				}
			}
		case "tool":
			toolMsgs++
		}
	}
	if assistantToolMsgs != 1 {
		t.Errorf("expected exactly 1 assistant tool-call message (parallel calls merged), got %d", assistantToolMsgs)
	}
	if toolMsgs != 2 {
		t.Errorf("expected 2 tool output messages, got %d", toolMsgs)
	}

	// The assistant tool-call message must be immediately followed by a tool output
	// (no assistant→assistant interleave that breaks tool_call/tool pairing).
	for i, m := range ir.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if i+1 >= len(ir.Messages) || ir.Messages[i+1].Role != "tool" {
				t.Errorf("assistant tool-call message must be immediately followed by a tool output, got %+v at %d", ir.Messages, i)
			}
		}
	}
}

// TestSingleToolCallStillEmitsOneAssistantMessage ensures the merge logic does not
// regress the common single-call case: one function_call -> one assistant message.
func TestSingleToolCallStillEmitsOneAssistantMessage(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read"}]},
			{"type":"function_call","call_id":"c0","name":"ReadFile","arguments":"{\"path\":\"main.go\"}"},
			{"type":"function_call_output","call_id":"c0","output":"package main"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}
		],
		"tools":[{"type":"function","name":"ReadFile","parameters":{"type":"object"}}],
		"tool_choice":"auto"
	}`)

	ir, err := (&ResponseInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	assistantToolMsgs := 0
	for _, m := range ir.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistantToolMsgs++
			if len(m.ToolCalls) != 1 {
				t.Errorf("single call must stay 1 tool_call, got %d", len(m.ToolCalls))
			}
		}
	}
	if assistantToolMsgs != 1 {
		t.Errorf("expected 1 assistant tool-call message, got %d", assistantToolMsgs)
	}
}
