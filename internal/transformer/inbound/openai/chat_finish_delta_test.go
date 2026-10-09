package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// finishDeltaWire feeds one internal chunk through the client-facing Chat stream
// builder and returns the raw downstream frame(s) as `data:` payload strings. It
// also asserts the source chunk (kept for aggregation / billing) is never mutated.
func finishDeltaWire(t *testing.T, in *ChatInbound, chunk *model.InternalLLMResponse) []string {
	t.Helper()
	before, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := in.TransformStream(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("source chunk mutated:\n before=%s\n after=%s", before, after)
	}
	var out []string
	for _, frame := range strings.Split(strings.TrimSuffix(string(wire), "\n\n"), "\n\n") {
		if frame == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(frame, "data: "))
	}
	return out
}

// TestChatFinishChunkAlwaysCarriesDelta locks the downstream shape fix: an upstream
// finish-only frame that omits `delta` (choice has finish_reason but no delta) must
// be serialized with `"delta":{"role":"assistant"}` so strict clients that read
// choices[0].delta.content never dereference a missing object.
func TestChatFinishChunkAlwaysCarriesDelta(t *testing.T) {
	in := &ChatInbound{}
	chunk := &model.InternalLLMResponse{
		ID:      "cmpl-1",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "reasoning-model",
		Choices: []model.Choice{{Index: 0, FinishReason: ptr("stop")}},
	}
	lines := finishDeltaWire(t, in, chunk)
	if len(lines) != 1 {
		t.Fatalf("want 1 frame, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"delta":{"role":"assistant"}`) {
		t.Fatalf("finish frame missing delta: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason lost: %s", lines[0])
	}
}

// TestChatFinishDeltaValueContract pins the exact wire value so future edits cannot
// silently change the backfilled object (role name or field set).
func TestChatFinishDeltaValueContract(t *testing.T) {
	in := &ChatInbound{}
	chunk := &model.InternalLLMResponse{
		Object:  "chat.completion.chunk",
		Choices: []model.Choice{{Index: 0, FinishReason: ptr("tool_calls")}},
	}
	lines := finishDeltaWire(t, in, chunk)
	var frame struct {
		Choices []struct {
			Delta        json.RawMessage `json:"delta"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.Choices) != 1 {
		t.Fatalf("choices = %+v", frame.Choices)
	}
	if got := string(frame.Choices[0].Delta); got != `{"role":"assistant"}` {
		t.Fatalf("delta = %s; want {\"role\":\"assistant\"}", got)
	}
	if frame.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", frame.Choices[0].FinishReason)
	}
}

// TestChatFinishDeltaBackfillLeavesOtherShapesAlone guards the non-regression
// invariants: content chunks, tool-call chunks and the empty-choices usage chunk
// must all pass through byte-identical (the backfill is a pure no-op for them).
func TestChatFinishDeltaBackfillLeavesOtherShapesAlone(t *testing.T) {
	in := &ChatInbound{}
	cases := []struct {
		name  string
		chunk *model.InternalLLMResponse
	}{
		{
			name: "content_delta",
			chunk: &model.InternalLLMResponse{
				Object:  "chat.completion.chunk",
				Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr("hello")}}}},
			},
		},
		{
			name: "tool_call_delta",
			chunk: &model.InternalLLMResponse{
				Object: "chat.completion.chunk",
				Choices: []model.Choice{{Index: 0, Delta: &model.Message{ToolCalls: []model.ToolCall{{
					Index: 0, ID: "call_1", Type: "function",
					Function: model.FunctionCall{Name: "lookup", Arguments: "{}"},
				}}}}},
			},
		},
		{
			name: "empty_delta_finish",
			chunk: &model.InternalLLMResponse{
				Object:  "chat.completion.chunk",
				Choices: []model.Choice{{Index: 0, Delta: &model.Message{}, FinishReason: ptr("stop")}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.chunk)
			if err != nil {
				t.Fatal(err)
			}
			lines := finishDeltaWire(t, in, tc.chunk)
			if len(lines) != 1 || lines[0] != string(before) {
				t.Fatalf("chunk altered:\n got=%v\n want=%s", lines, before)
			}
		})
	}
}

// TestChatUsageOnlyChunkHasNoDelta ensures the empty-choices usage frame keeps its
// `"choices":[]` shape and never grows a synthetic delta.
func TestChatUsageOnlyChunkHasNoDelta(t *testing.T) {
	in := &ChatInbound{}
	chunk := &model.InternalLLMResponse{
		Object: "chat.completion.chunk",
		Usage:  &model.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	lines := finishDeltaWire(t, in, chunk)
	if len(lines) != 1 {
		t.Fatalf("want 1 frame, got %v", lines)
	}
	if !strings.Contains(lines[0], `"choices":[]`) {
		t.Fatalf("empty choices lost: %s", lines[0])
	}
	if strings.Contains(lines[0], `"delta"`) {
		t.Fatalf("usage frame grew a delta: %s", lines[0])
	}
}

// TestChatFinishDeltaBackfillSparseChoices covers a multi-choice frame: only the
// finish-bearing choice is backfilled; the content choice is untouched, and the
// source chunk (with its nil delta) stays pristine for aggregation.
func TestChatFinishDeltaBackfillSparseChoices(t *testing.T) {
	in := &ChatInbound{}
	chunk := &model.InternalLLMResponse{
		Object: "chat.completion.chunk",
		Choices: []model.Choice{
			{Index: 0, Delta: &model.Message{Content: model.MessageContent{Content: ptr("a")}}},
			{Index: 1, FinishReason: ptr("length")},
		},
	}
	lines := finishDeltaWire(t, in, chunk)
	if len(lines) != 1 {
		t.Fatalf("want 1 frame, got %v", lines)
	}
	var frame struct {
		Choices []struct {
			Index        int             `json:"index"`
			Delta        json.RawMessage `json:"delta"`
			FinishReason *string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.Choices) != 2 {
		t.Fatalf("choices = %+v", frame.Choices)
	}
	if got := string(frame.Choices[0].Delta); got != `{"content":"a"}` {
		t.Fatalf("content choice changed: %s", got)
	}
	if got := string(frame.Choices[1].Delta); got != `{"role":"assistant"}` {
		t.Fatalf("finish choice delta = %s", got)
	}
	if frame.Choices[1].FinishReason == nil || *frame.Choices[1].FinishReason != "length" {
		t.Fatalf("finish_reason = %v", frame.Choices[1].FinishReason)
	}
}
