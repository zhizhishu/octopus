package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// derefString reads a *string field that used to be a plain string, returning ""
// for nil so lifecycle assertions stay nil-safe after Arguments became a pointer.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// feedToolStream drives the streaming Responses transformer with a single tool
// call whose argument fragments are supplied in order, then closes the stream.
// It returns the full raw SSE the client would see.
func feedToolStream(t *testing.T, name string, argFragments ...string) string {
	t.Helper()
	inbound := &ResponseInbound{}
	finishReason := "tool_calls"

	feed := func(stream *model.InternalLLMResponse) string {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		return string(out)
	}
	chunk := func(tc model.ToolCall) *model.InternalLLMResponse {
		return &model.InternalLLMResponse{
			ID:      "chatcmpl_args_norm",
			Model:   "gpt-4o",
			Object:  "chat.completion.chunk",
			Created: 123,
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{tc}},
			}},
		}
	}

	var raw strings.Builder
	// First fragment carries id + name (arguments may be empty).
	raw.WriteString(feed(chunk(model.ToolCall{
		Index:    0,
		ID:       "call_argnorm",
		Type:     "function",
		Function: model.FunctionCall{Name: name, Arguments: firstOrEmpty(argFragments)},
	})))
	// Remaining fragments carry only argument deltas (no id/name), mirroring codex.
	for _, frag := range restFragments(argFragments) {
		raw.WriteString(feed(chunk(model.ToolCall{
			Index:    0,
			Function: model.FunctionCall{Arguments: frag},
		})))
	}

	raw.WriteString(feed(&model.InternalLLMResponse{
		ID:      "chatcmpl_args_norm",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	}))

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	raw.WriteString(string(done))
	return raw.String()
}

func firstOrEmpty(frags []string) string {
	if len(frags) == 0 {
		return ""
	}
	return frags[0]
}

func restFragments(frags []string) []string {
	if len(frags) <= 1 {
		return nil
	}
	return frags[1:]
}

// R2 + F3, streaming path: a function_call with NO upstream arguments must reach
// the codex client as a valid-JSON "{}" (never "" — json.Unmarshal would fail) on
// every finalized surface, while the in-progress output_item.added event carries an
// explicit empty "arguments":"" (F3, the genuine OpenAI Responses shape sub2api also
// emits). Mirrors sub2api chatcompletions_to_responses (args == "" -> "{}").
func TestResponseInboundStreamEmptyToolArgumentsNormalizeToEmptyObject(t *testing.T) {
	raw := feedToolStream(t, "get_time") // no argument fragments at all

	// F3 (wire-level): the added event must literally contain an empty arguments
	// string. omitempty on a bare string would have dropped it.
	if !strings.Contains(raw, `"arguments":""`) {
		t.Fatalf(`F3: function_call output_item.added must carry "arguments":"" literally; raw=%s`, raw)
	}

	events := parseResponsesStreamEvents(t, raw)

	var sawAdded, sawArgsDone, sawItemDone, sawCompleted bool
	for _, ev := range events {
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			sawAdded = true
			if ev.Item.Arguments == nil {
				t.Fatalf("F3: added function_call must set arguments to an explicit empty string, got nil")
			}
			if *ev.Item.Arguments != "" {
				t.Fatalf("F3: added function_call arguments must be \"\", got %q", *ev.Item.Arguments)
			}
		case "response.function_call_arguments.delta":
			t.Fatalf("no delta must be emitted for an empty-argument tool call, got %#v", ev)
		case "response.function_call_arguments.done":
			sawArgsDone = true
			if ev.Arguments != "{}" {
				t.Fatalf("R2: empty arguments must normalize to {} on function_call_arguments.done, got %q", ev.Arguments)
			}
		case "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			sawItemDone = true
			if got := derefString(ev.Item.Arguments); got != "{}" {
				t.Fatalf("R2: empty arguments must normalize to {} on output_item.done, got %q", got)
			}
		case "response.completed":
			sawCompleted = true
			if ev.Response == nil {
				t.Fatalf("response.completed missing response payload")
			}
			assertCompletedFunctionCallArgs(t, ev.Response.Output, "{}")
		}
	}
	if !sawAdded || !sawArgsDone || !sawItemDone || !sawCompleted {
		t.Fatalf("missing expected events: added=%v argsDone=%v itemDone=%v completed=%v",
			sawAdded, sawArgsDone, sawItemDone, sawCompleted)
	}
}

// Real (non-empty) tool arguments must pass through byte-for-byte — normalization
// only rescues the empty case and must never rewrite a genuine payload. The added
// event still carries "arguments":"" because the value streams via *.delta.
func TestResponseInboundStreamRealToolArgumentsUnchanged(t *testing.T) {
	raw := feedToolStream(t, "calc", `{"x":`, `1}`)
	events := parseResponsesStreamEvents(t, raw)

	const want = `{"x":1}`
	var sawArgsDone, sawItemDone, sawCompleted bool
	for _, ev := range events {
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				if got := derefString(ev.Item.Arguments); got != "" {
					t.Fatalf("added function_call arguments must be empty (value streams via delta), got %q", got)
				}
			}
		case "response.function_call_arguments.done":
			sawArgsDone = true
			if ev.Arguments != want {
				t.Fatalf("real arguments corrupted on function_call_arguments.done: got %q want %q", ev.Arguments, want)
			}
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				sawItemDone = true
				if got := derefString(ev.Item.Arguments); got != want {
					t.Fatalf("real arguments corrupted on output_item.done: got %q want %q", got, want)
				}
			}
		case "response.completed":
			sawCompleted = true
			if ev.Response != nil {
				assertCompletedFunctionCallArgs(t, ev.Response.Output, want)
			}
		}
	}
	if !sawArgsDone || !sawItemDone || !sawCompleted {
		t.Fatalf("missing expected events: argsDone=%v itemDone=%v completed=%v", sawArgsDone, sawItemDone, sawCompleted)
	}
}

// R2, non-streaming path: convertToResponsesAPIResponse must apply the same
// empty-arguments -> "{}" normalization so a non-streamed function_call is valid JSON.
func TestResponseInboundNonStreamEmptyToolArgumentsNormalized(t *testing.T) {
	inbound := &ResponseInbound{}
	resp := &model.InternalLLMResponse{
		ID:    "chatcmpl_ns",
		Model: "gpt-4o",
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					ID:       "call_ns",
					Type:     "function",
					Function: model.FunctionCall{Name: "get_time", Arguments: ""},
				}},
			},
		}},
	}

	body, err := inbound.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse returned error: %v", err)
	}

	var parsed struct {
		Output []struct {
			Type      string  `json:"type"`
			Arguments *string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to decode non-stream response: %v", err)
	}

	var found bool
	for _, it := range parsed.Output {
		if it.Type != "function_call" {
			continue
		}
		found = true
		if it.Arguments == nil || *it.Arguments != "{}" {
			t.Fatalf("R2 non-stream: empty arguments must normalize to {}, got %v", it.Arguments)
		}
		var v any
		if err := json.Unmarshal([]byte(*it.Arguments), &v); err != nil {
			t.Fatalf("R2 non-stream: arguments must be valid JSON, got %q: %v", *it.Arguments, err)
		}
	}
	if !found {
		t.Fatalf("non-stream output missing function_call item")
	}
}

// assertCompletedFunctionCallArgs verifies the terminal response.completed.output
// carries the function_call with the expected (valid-JSON) arguments.
func assertCompletedFunctionCallArgs(t *testing.T, output []ResponsesItem, want string) {
	t.Helper()
	var found bool
	for _, it := range output {
		if it.Type != "function_call" {
			continue
		}
		found = true
		got := derefString(it.Arguments)
		if got != want {
			t.Fatalf("R2: completed.output function_call arguments = %q, want %q", got, want)
		}
		var v any
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Fatalf("R2: completed.output arguments must be valid JSON, got %q: %v", got, err)
		}
	}
	if !found {
		t.Fatalf("response.completed.output missing function_call item")
	}
}
