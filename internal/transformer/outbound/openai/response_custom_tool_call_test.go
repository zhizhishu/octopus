package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestResponseOutboundCapturesCustomToolCallInput feeds the exact upstream event
// sequence a codex 0.144.x custom (freeform-grammar) tool produces — output_item.added
// (custom_tool_call) → custom_tool_call_input.delta → custom_tool_call_input.done →
// output_item.done — and asserts the internal tool call carries the tool name once,
// the full freeform input as its arguments, and the "custom" type marker. Before the
// fix the input lived in `input` (not `arguments`) and was dropped entirely.
func TestResponseOutboundCapturesCustomToolCallInput(t *testing.T) {
	outbound := &ResponseOutbound{}

	// Freeform JS input (NOT JSON arguments); embeds quotes/newlines on purpose.
	input := "const r = await tools.shell_command({\"command\":\"pwd\"});\ntext(r);\n"
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	events := []string{
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"ctc_1","type":"custom_tool_call","status":"in_progress","call_id":"call_1","input":"","name":"exec"}}`,
		fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":%s}`, inputJSON),
		fmt.Sprintf(`{"type":"response.custom_tool_call_input.done","output_index":1,"item_id":"ctc_1","input":%s}`, inputJSON),
		fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_1","input":%s,"name":"exec"}}`, inputJSON),
	}

	var (
		aggregatedArgs string
		nameCount      int
		seenType       string
		seenID         string
	)
	for _, ev := range events {
		resp, err := outbound.TransformStream(context.Background(), []byte(ev))
		if err != nil {
			t.Fatalf("TransformStream returned error for %s: %v", ev, err)
		}
		if resp == nil {
			continue
		}
		for _, choice := range resp.Choices {
			if choice.Delta == nil {
				continue
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Type != model.ToolCallTypeCustom {
					t.Fatalf("expected custom tool-call type, got %q", tc.Type)
				}
				seenType = tc.Type
				if tc.ID != "" {
					seenID = tc.ID
				}
				if tc.Function.Name != "" {
					nameCount++
					if tc.Function.Name != "exec" {
						t.Fatalf("expected tool name exec, got %q", tc.Function.Name)
					}
				}
				aggregatedArgs += tc.Function.Arguments
			}
		}
	}

	if seenType != model.ToolCallTypeCustom {
		t.Fatalf("expected the tool call to be marked custom, got %q", seenType)
	}
	if seenID != "call_1" {
		t.Fatalf("expected call_id call_1 to be carried, got %q", seenID)
	}
	if nameCount != 1 {
		t.Fatalf("expected the tool name to be emitted exactly once, got %d", nameCount)
	}
	if aggregatedArgs != input {
		t.Fatalf("expected aggregated arguments to equal the full freeform input.\n got: %q\nwant: %q", aggregatedArgs, input)
	}

	// The terminal event must still map to a tool_calls finish so the inbound
	// closes the tool item instead of a plain stop.
	completed, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.completed",
		"response":{"id":"resp_1","object":"response","model":"gpt-5.6","status":"completed","output":[]}
	}`))
	if err != nil {
		t.Fatalf("TransformStream response.completed returned error: %v", err)
	}
	if completed == nil || len(completed.Choices) != 1 || completed.Choices[0].FinishReason == nil {
		t.Fatalf("expected finish chunk, got %#v", completed)
	}
	if *completed.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls finish reason, got %#v", completed.Choices[0].FinishReason)
	}
}

// TestResponseOutboundCustomToolCallDoneEmitsUnstreamedSuffix verifies that when
// custom_tool_call_input.done carries more text than the deltas already streamed,
// only the not-yet-seen suffix is emitted (no duplication of the streamed prefix).
func TestResponseOutboundCustomToolCallDoneEmitsUnstreamedSuffix(t *testing.T) {
	outbound := &ResponseOutbound{}

	feed := func(payload string) *model.InternalLLMResponse {
		resp, err := outbound.TransformStream(context.Background(), []byte(payload))
		if err != nil {
			t.Fatalf("TransformStream error for %s: %v", payload, err)
		}
		return resp
	}

	feed(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_2","type":"custom_tool_call","status":"in_progress","call_id":"call_2","input":"","name":"exec"}}`)
	feed(`{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_2","delta":"foo"}`)
	// done carries the full input "foobar"; only "bar" is new.
	doneChunk := feed(`{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_2","input":"foobar"}`)
	if doneChunk == nil || len(doneChunk.Choices) != 1 || doneChunk.Choices[0].Delta == nil || len(doneChunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected a suffix chunk from done, got %#v", doneChunk)
	}
	tc := doneChunk.Choices[0].Delta.ToolCalls[0]
	if tc.Type != model.ToolCallTypeCustom {
		t.Fatalf("expected custom type on suffix chunk, got %q", tc.Type)
	}
	if tc.Function.Arguments != "bar" {
		t.Fatalf("expected only the unstreamed suffix 'bar', got %q", tc.Function.Arguments)
	}
}

// TestResponseOutboundFunctionCallStaysFunction is the regression guard that
// normal function_call tool calls are unaffected by the custom-tool-call handling
// (still type "function", arguments carried from `arguments`).
func TestResponseOutboundFunctionCallStaysFunction(t *testing.T) {
	outbound := &ResponseOutbound{}

	added, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.added","output_index":0,
		"item":{"id":"fc_1","type":"function_call","call_id":"call_fn","name":"get_weather"}
	}`))
	if err != nil {
		t.Fatalf("added error: %v", err)
	}
	if added == nil || added.Choices[0].Delta.ToolCalls[0].Type != "function" {
		t.Fatalf("expected function type on function_call, got %#v", added)
	}

	delta, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":1}"
	}`))
	if err != nil {
		t.Fatalf("delta error: %v", err)
	}
	got := delta.Choices[0].Delta.ToolCalls[0]
	if got.Type != "function" {
		t.Fatalf("expected function type on argument delta, got %q", got.Type)
	}
	if got.Function.Arguments != `{"q":1}` {
		t.Fatalf("expected function arguments to survive, got %q", got.Function.Arguments)
	}
}
