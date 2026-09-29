package gemini

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// 反例（§9 真机 T-D W03 WG stream 发现）：上游以 OpenAI 流式工具调用分片
// （首片 name+空 arguments，后续片增量 arguments）到达时，Gemini 流式出口
// 把每个分片原样转成 functionCall part——首片只有 name 没有 args、后续片
// name 为空——Gemini 客户端无法重组，参数整体丢失。正确行为：分片聚合，
// 终止时发出完整 functionCall（name + 完整 args）。
func TestStreamToolCallFragmentsAggregateIntoCompleteFunctionCall(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					Role: "assistant",
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_1",
						Index: 0,
						Function: transformerModel.FunctionCall{
							Name:      "oct9_echo",
							Arguments: "",
						},
					}},
				},
			}},
		},
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						Index: 0,
						Function: transformerModel.FunctionCall{
							Arguments: `{"text": "OCT9-`,
						},
					}},
				},
			}},
		},
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						Index: 0,
						Function: transformerModel.FunctionCall{
							Arguments: `FRAG}"}`,
						},
					}},
				},
			}},
		},
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index:        0,
				Delta:        &transformerModel.Message{},
				FinishReason: strPtr("tool_calls"),
			}},
		},
	}

	var emitted []map[string]interface{}
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("transform stream: %v", err)
		}
		if len(out) == 0 {
			continue
		}
		data := strings.TrimPrefix(strings.TrimSpace(string(out)), "data: ")
		var frame map[string]interface{}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("frame not json: %v (%q)", err, data)
		}
		emitted = append(emitted, frame)
	}

	// 收集所有发出去的 functionCall part。
	var calls []map[string]interface{}
	for _, frame := range emitted {
		for _, cand := range frame["candidates"].([]interface{}) {
			content, _ := cand.(map[string]interface{})["content"].(map[string]interface{})
			if content == nil {
				continue
			}
			parts, _ := content["parts"].([]interface{})
			for _, p := range parts {
				part, _ := p.(map[string]interface{})
				if fc, ok := part["functionCall"].(map[string]interface{}); ok {
					calls = append(calls, fc)
				}
			}
		}
	}

	if len(calls) == 0 {
		t.Fatalf("no functionCall part emitted at all; frames=%d", len(emitted))
	}
	// 必须存在一个完整可用的 functionCall：名字对 + 参数完整。
	var complete bool
	for _, fc := range calls {
		name, _ := fc["name"].(string)
		if name != "oct9_echo" {
			continue
		}
		args, _ := fc["args"].(map[string]interface{})
		if args == nil {
			continue
		}
		if text, _ := args["text"].(string); text == `OCT9-FRAG}` {
			complete = true
		}
	}
	if !complete {
		t.Fatalf("no complete functionCall (name=oct9_echo, args.text=OCT9-FRAG}) emitted; got %v", calls)
	}
	// 不允许发出"空名字"的碎片 functionCall（协议垃圾）。
	for _, fc := range calls {
		name, _ := fc["name"].(string)
		if name == "" {
			t.Fatalf("fragmented functionCall with empty name emitted: %v", fc)
		}
	}
}

func strPtr(s string) *string { return &s }

// 兜底路径：上游流提前结束（[DONE] 到了但 choice 没发 finish_reason），
// 已缓冲的工具调用分片仍须以完整 functionCall 发出，参数不能丢。
func TestStreamToolCallFragmentsFlushOnStreamDone(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_2",
						Index: 0,
						Function: transformerModel.FunctionCall{
							Name:      "oct9_echo",
							Arguments: `{"text": "OCT9-`,
						},
					}},
				},
			}},
		},
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						Index: 0,
						Function: transformerModel.FunctionCall{
							Arguments: `DONE}"}`,
						},
					}},
				},
			}},
		},
		{Object: "[DONE]"},
	}

	var sawComplete bool
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("transform stream: %v", err)
		}
		if len(out) == 0 {
			continue
		}
		data := strings.TrimPrefix(strings.TrimSpace(string(out)), "data: ")
		var frame map[string]interface{}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("frame not json: %v (%q)", err, data)
		}
		for _, cand := range frame["candidates"].([]interface{}) {
			content, _ := cand.(map[string]interface{})["content"].(map[string]interface{})
			if content == nil {
				continue
			}
			parts, _ := content["parts"].([]interface{})
			for _, p := range parts {
				part, _ := p.(map[string]interface{})
				fc, ok := part["functionCall"].(map[string]interface{})
				if !ok {
					continue
				}
				if name, _ := fc["name"].(string); name != "oct9_echo" {
					t.Fatalf("unexpected functionCall name: %v", fc)
				}
				args, _ := fc["args"].(map[string]interface{})
				text, _ := args["text"].(string)
				if text != `OCT9-DONE}` {
					t.Fatalf("incomplete args on flush: %v", fc)
				}
				sawComplete = true
			}
		}
	}
	if !sawComplete {
		t.Fatalf("stream ended without emitting the buffered functionCall")
	}
}

// 单块完整工具调用（不分片的上游，如 §9 T-G 实测形态）：也必须在收尾帧
// 发出完整 functionCall，且不得提前发碎片。
func TestStreamSingleChunkToolCallEmitsCompleteOnFinish(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_3",
						Index: 0,
						Function: transformerModel.FunctionCall{
							Name:      "oct9_echo",
							Arguments: `{"text": "OCT9-ONE"}`,
						},
					}},
				},
				FinishReason: strPtr("tool_calls"),
			}},
		},
	}

	var frames []map[string]interface{}
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("transform stream: %v", err)
		}
		if len(out) == 0 {
			continue
		}
		data := strings.TrimPrefix(strings.TrimSpace(string(out)), "data: ")
		var frame map[string]interface{}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("frame not json: %v (%q)", err, data)
		}
		frames = append(frames, frame)
	}
	if len(frames) != 1 {
		t.Fatalf("expected exactly one frame, got %d", len(frames))
	}
	cand, _ := frames[0]["candidates"].([]interface{})[0].(map[string]interface{})
	content, _ := cand["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("expected one part, got %v", parts)
	}
	part, _ := parts[0].(map[string]interface{})
	fc, ok := part["functionCall"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected functionCall part, got %v", part)
	}
	if name, _ := fc["name"].(string); name != "oct9_echo" {
		t.Fatalf("wrong name: %v", fc)
	}
	args, _ := fc["args"].(map[string]interface{})
	if text, _ := args["text"].(string); text != "OCT9-ONE" {
		t.Fatalf("wrong args: %v", fc)
	}
}
