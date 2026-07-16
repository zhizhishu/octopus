package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
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

// parseAnthropicStreamEvents decodes the concatenated SSE bytes emitted across
// several TransformStream calls back into StreamEvent structs. Each event is
// serialized as `event:<type>\ndata:<json>\n\n`; we only need the JSON payload
// carried on the data lines.
func parseAnthropicStreamEvents(t *testing.T, chunks ...[]byte) []StreamEvent {
	t.Helper()
	var events []StreamEvent
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		for _, line := range strings.Split(string(chunk), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimPrefix(line, "data:")
			var ev StreamEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatalf("failed to unmarshal SSE data %q: %v", payload, err)
			}
			events = append(events, ev)
		}
	}
	return events
}

// TestTransformStreamInterleavedToolCallArgsRouteToOwnBlocks drives TransformStream
// with two tool_use blocks that are opened first (upstream indices 0 and 1) and only
// afterwards receive their argument fragments, out of block order. Each
// input_json_delta must land on the Anthropic content-block index that its upstream
// tool_call index owns — not on the latest-opened block. Before FIX A1 both argument
// deltas were emitted at &i.contentIndex (the latest block), so `{"a":1}` and `{"b":2}`
// collided on the same block; this test locks the per-index routing.
func TestTransformStreamInterleavedToolCallArgsRouteToOwnBlocks(t *testing.T) {
	inbound := &MessagesInbound{}

	// chunk1: open tool call 0 (call_a / get_weather), no arguments yet.
	chunk1, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_tools",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				Role: "assistant",
				ToolCalls: []transformerModel.ToolCall{{
					Index:    0,
					ID:       "call_a",
					Type:     "function",
					Function: transformerModel.FunctionCall{Name: "get_weather"},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chunk1 stream: %v", err)
	}

	// chunk2: open tool call 1 (call_b / get_time), no arguments yet.
	chunk2, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_tools",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				ToolCalls: []transformerModel.ToolCall{{
					Index:    1,
					ID:       "call_b",
					Type:     "function",
					Function: transformerModel.FunctionCall{Name: "get_time"},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chunk2 stream: %v", err)
	}

	// chunk3: arguments for tool call 0, arriving after tool call 1 was opened.
	chunk3, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_tools",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				ToolCalls: []transformerModel.ToolCall{{
					Index:    0,
					Function: transformerModel.FunctionCall{Arguments: `{"a":1}`},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chunk3 stream: %v", err)
	}

	// chunk4: arguments for tool call 1.
	chunk4, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_tools",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				ToolCalls: []transformerModel.ToolCall{{
					Index:    1,
					Function: transformerModel.FunctionCall{Arguments: `{"b":2}`},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chunk4 stream: %v", err)
	}

	// finish chunk: FinishReason tool_calls + usage in the same chunk.
	finishReason := "tool_calls"
	finish, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_tools",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
		Usage: &transformerModel.Usage{
			PromptTokens:     12,
			CompletionTokens: 6,
		},
	})
	if err != nil {
		t.Fatalf("finish stream: %v", err)
	}

	events := parseAnthropicStreamEvents(t, chunk1, chunk2, chunk3, chunk4, finish)

	// Resolve the content-block index each tool_use owns from its content_block_start.
	blockOfToolID := map[string]int64{}
	for _, ev := range events {
		if ev.Type == "content_block_start" && ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			if ev.Index == nil {
				t.Fatalf("content_block_start for tool_use %q missing index", ev.ContentBlock.ID)
			}
			blockOfToolID[ev.ContentBlock.ID] = *ev.Index
		}
	}
	blockA, okA := blockOfToolID["call_a"]
	blockB, okB := blockOfToolID["call_b"]
	if !okA || !okB {
		t.Fatalf("expected content_block_start for both tool calls, got %#v", blockOfToolID)
	}
	if blockA == blockB {
		t.Fatalf("expected the two tool_use blocks to occupy DIFFERENT indices, both were %d", blockA)
	}

	// Resolve the block index each input_json_delta was emitted at.
	blockOfPartialJSON := map[string]int64{}
	for _, ev := range events {
		if ev.Type != "content_block_delta" || ev.Delta == nil || ev.Delta.Type == nil || *ev.Delta.Type != "input_json_delta" {
			continue
		}
		if ev.Delta.PartialJSON == nil {
			t.Fatalf("input_json_delta missing partial_json at index %#v", ev.Index)
		}
		if ev.Index == nil {
			t.Fatalf("input_json_delta %q missing index", *ev.Delta.PartialJSON)
		}
		blockOfPartialJSON[*ev.Delta.PartialJSON] = *ev.Index
	}

	gotA, okDA := blockOfPartialJSON[`{"a":1}`]
	gotB, okDB := blockOfPartialJSON[`{"b":2}`]
	if !okDA || !okDB {
		t.Fatalf("expected both argument deltas to be emitted, got %#v", blockOfPartialJSON)
	}
	if gotA != blockA {
		t.Fatalf(`argument {"a":1} landed on block %d, want tool call 0's block %d`, gotA, blockA)
	}
	if gotB != blockB {
		t.Fatalf(`argument {"b":2} landed on block %d, want tool call 1's block %d`, gotB, blockB)
	}
}
