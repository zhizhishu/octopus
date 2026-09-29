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

// 反例（协同审查 R1）：部分上游在同一工具的每个分片里重复完整函数名
// （既有 chat_tool_name_dedup_test.go 记录的形态）。名字是原子值，取第一个
// 非空值，绝不拼接；且出口名字必须与聚合（日志/计费口径）一致。
func TestStreamRepeatedFullNameNotConcatenated(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_r1",
						Index: 0,
						Function: transformerModel.FunctionCall{
							Name:      "oct9_lookup",
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
							Name:      "oct9_lookup",
							Arguments: `{"q": "OCT9-`,
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
							Name:      "oct9_lookup",
							Arguments: `R1}"}`,
						},
					}},
				},
				FinishReason: strPtr("tool_calls"),
			}},
		},
	}

	var wire strings.Builder
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("transform stream: %v", err)
		}
		wire.Write(out)
	}
	if strings.Contains(wire.String(), "oct9_lookupoct9_lookup") {
		t.Fatalf("tool name concatenated on client wire: %s", wire.String())
	}
	if !strings.Contains(wire.String(), `"name":"oct9_lookup"`) {
		t.Fatalf("complete functionCall missing: %s", wire.String())
	}

	// 出口与聚合口径必须一致：客户端看到的名字 = 日志/计费记录的名字。
	aggregate, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	aggName := aggregate.Choices[0].Message.ToolCalls[0].Function.Name
	if aggName != "oct9_lookup" {
		t.Fatalf("aggregate name diverges: %q", aggName)
	}
}

// 名字迟到：首片只有参数增量、名字为空，后续片才带完整名字。
func TestStreamNameArrivesInLaterFragment(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_late",
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
							Name:      "oct9_late",
							Arguments: `LATE}"}`,
						},
					}},
				},
				FinishReason: strPtr("tool_calls"),
			}},
		},
	}

	var sawComplete bool
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("transform stream: %v", err)
		}
		if !strings.Contains(string(out), `"name":"oct9_late"`) {
			continue
		}
		if !strings.Contains(string(out), `OCT9-LATE}`) {
			t.Fatalf("late name emitted without complete args: %s", out)
		}
		sawComplete = true
	}
	if !sawComplete {
		t.Fatalf("late-arriving name never emitted as a complete call")
	}
}

// 参数归一化契约（参考项目 CLIProxyAPI parseArgsToObjectRaw 同款）：
// JSON 对象原样通过；空、截断、null/数组/标量一律归一为空对象——调用
// 不丢、不报错、客户端拿到结构完好的 functionCall。
func TestParseToolCallArgsObject(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      map[string]interface{}
	}{
		{"valid object", `{"x":1}`, map[string]interface{}{"x": float64(1)}},
		{"empty object", `{}`, map[string]interface{}{}},
		{"empty string", "", map[string]interface{}{}},
		{"whitespace only", "  ", map[string]interface{}{}},
		{"truncated object", `{"x":`, map[string]interface{}{}},
		{"json null", `null`, map[string]interface{}{}},
		{"json array", `[]`, map[string]interface{}{}},
		{"json scalar", `42`, map[string]interface{}{}},
		{"garbage", `not json`, map[string]interface{}{}},
	}
	for _, tc := range cases {
		got := parseToolCallArgsObject(tc.arguments)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("%s: key %q got %v, want %v", tc.name, k, got[k], v)
			}
		}
	}
}

// 截断参数（如 finish_reason=length 切断 JSON）仍须发出带名字的完好调用，
// 不丢调用、不报错——与参考项目的 fail-open 归一化一致。
func TestStreamTruncatedArgsStillEmitNamedCall(t *testing.T) {
	inbound := &GenerateContentInbound{}
	chunks := []*transformerModel.InternalLLMResponse{
		{
			Object: "chat.completion.chunk",
			Choices: []transformerModel.Choice{{
				Index: 0,
				Delta: &transformerModel.Message{
					ToolCalls: []transformerModel.ToolCall{{
						ID:    "call_trunc",
						Index: 0,
						Function: transformerModel.FunctionCall{
							Name:      "oct9_trunc",
							Arguments: `{"x":`,
						},
					}},
				},
			}},
		},
		{Object: "[DONE]"},
	}

	var sawNamed bool
	for _, chunk := range chunks {
		out, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("truncated args must not error: %v", err)
		}
		if strings.Contains(string(out), `"name":"oct9_trunc"`) {
			sawNamed = true
		}
	}
	if !sawNamed {
		t.Fatalf("truncated-args call dropped entirely")
	}
}
