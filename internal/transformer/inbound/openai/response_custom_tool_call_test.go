package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestResponseInboundReEmitsCustomToolCall verifies that an internal tool call
// marked custom (model.ToolCallTypeCustom) is streamed back to a codex client as
// a Responses custom_tool_call — an output item of type custom_tool_call plus
// response.custom_tool_call_input.delta/.done events carrying the freeform input —
// and NOT as a function_call. A codex client that registered a custom tool rejects
// a function_call in its place, so the custom nature must survive the round-trip.
func TestResponseInboundReEmitsCustomToolCall(t *testing.T) {
	inbound := &ResponseInbound{}
	input := "const r = await tools.shell_command({\"command\":\"pwd\"});\ntext(r);\n"
	finishReason := "tool_calls"

	feeds := []*model.InternalLLMResponse{
		// Item announcement (name + id, no payload yet).
		{
			ID:      "chatcmpl_c",
			Model:   "gpt-5.6",
			Object:  "chat.completion.chunk",
			Created: 1,
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
					Index:    0,
					ID:       "call_1",
					Type:     model.ToolCallTypeCustom,
					Function: model.FunctionCall{Name: "exec"},
				}}},
			}},
		},
		// Freeform input payload.
		{
			ID:      "chatcmpl_c",
			Model:   "gpt-5.6",
			Object:  "chat.completion.chunk",
			Created: 1,
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
					Index:    0,
					ID:       "call_1",
					Type:     model.ToolCallTypeCustom,
					Function: model.FunctionCall{Arguments: input},
				}}},
			}},
		},
		// Finish.
		{
			ID:      "chatcmpl_c",
			Model:   "gpt-5.6",
			Object:  "chat.completion.chunk",
			Created: 1,
			Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
			Usage:   &model.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}

	var raw strings.Builder
	for _, f := range feeds {
		out, err := inbound.TransformStream(context.Background(), f)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		raw.Write(out)
	}
	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done returned error: %v", err)
	}
	raw.Write(done)

	got := raw.String()

	// It must NOT emit any function_call shape for a custom tool call.
	if strings.Contains(got, "response.function_call_arguments") {
		t.Fatalf("custom tool call must not emit function_call_arguments events, got %s", got)
	}
	if strings.Contains(got, `"type":"function_call"`) {
		t.Fatalf("custom tool call must not emit a function_call output item, got %s", got)
	}

	events := parseResponsesStreamEvents(t, got)

	var (
		sawAdded         bool
		sawInputDelta    bool
		sawInputDone     bool
		sawItemDone      bool
		completedHasItem bool
		deltaText        string
		doneInput        string
	)
	for _, ev := range events {
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "custom_tool_call" {
				sawAdded = true
				if ev.Item.CallID != "call_1" || ev.Item.Name != "exec" {
					t.Fatalf("unexpected custom_tool_call added item: %#v", ev.Item)
				}
				if ev.Item.Input == nil {
					t.Fatalf("expected in-progress custom_tool_call to carry an (empty) input field, got nil")
				}
			}
		case "response.custom_tool_call_input.delta":
			sawInputDelta = true
			deltaText += ev.Delta
		case "response.custom_tool_call_input.done":
			sawInputDone = true
			doneInput = ev.Input
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "custom_tool_call" {
				sawItemDone = true
				if ev.Item.Input == nil || *ev.Item.Input != input {
					t.Fatalf("expected output_item.done custom_tool_call input %q, got %#v", input, ev.Item)
				}
			}
		case "response.completed":
			if ev.Response != nil {
				for _, it := range ev.Response.Output {
					if it.Type == "custom_tool_call" && it.Input != nil && *it.Input == input {
						completedHasItem = true
					}
				}
			}
		}
	}

	if !sawAdded {
		t.Fatalf("expected a custom_tool_call output_item.added, got %s", got)
	}
	if !sawInputDelta || deltaText != input {
		t.Fatalf("expected custom_tool_call_input.delta carrying the full input %q, got %q", input, deltaText)
	}
	if !sawInputDone || doneInput != input {
		t.Fatalf("expected custom_tool_call_input.done carrying the full input %q, got %q", input, doneInput)
	}
	if !sawItemDone {
		t.Fatalf("expected a completed custom_tool_call output_item.done, got %s", got)
	}
	if !completedHasItem {
		t.Fatalf("expected response.completed.output to include the custom_tool_call item, got %s", got)
	}
}

// TestResponseInboundFunctionCallStaysFunction is the regression guard that a
// normal function tool call is still streamed as function_call +
// function_call_arguments.delta/.done, unaffected by the custom-tool-call path.
func TestResponseInboundFunctionCallStaysFunction(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "tool_calls"

	feeds := []*model.InternalLLMResponse{
		{
			ID: "chatcmpl_f", Model: "gpt-4o", Object: "chat.completion.chunk", Created: 1,
			Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				Index: 0, ID: "call_fn", Type: "function", Function: model.FunctionCall{Name: "get_weather"},
			}}}}},
		},
		{
			ID: "chatcmpl_f", Model: "gpt-4o", Object: "chat.completion.chunk", Created: 1,
			Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				Index: 0, ID: "call_fn", Type: "function", Function: model.FunctionCall{Arguments: `{"city":"SF"}`},
			}}}}},
		},
		{
			ID: "chatcmpl_f", Model: "gpt-4o", Object: "chat.completion.chunk", Created: 1,
			Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
			Usage:   &model.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}

	var raw strings.Builder
	for _, f := range feeds {
		out, err := inbound.TransformStream(context.Background(), f)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		raw.Write(out)
	}
	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.Write(done)

	got := raw.String()
	if strings.Contains(got, "custom_tool_call") {
		t.Fatalf("function tool call must not emit any custom_tool_call shape, got %s", got)
	}
	if !strings.Contains(got, `"type":"response.function_call_arguments.delta"`) {
		t.Fatalf("expected function_call_arguments.delta, got %s", got)
	}
	if !strings.Contains(got, `"type":"response.function_call_arguments.done"`) {
		t.Fatalf("expected function_call_arguments.done, got %s", got)
	}
	if !strings.Contains(got, `"type":"function_call"`) {
		t.Fatalf("expected a function_call output item, got %s", got)
	}
}
