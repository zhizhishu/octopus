package openai

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func thinkingWire(t *testing.T, in *ChatInbound, chunk *model.InternalLLMResponse) []model.InternalLLMResponse {
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
		t.Fatal("source chunk mutated")
	}
	if chunk.Object == "[DONE]" && !strings.HasSuffix(string(wire), "data: [DONE]\n\n") {
		t.Fatalf("missing terminal sentinel: %q", wire)
	}
	var events []model.InternalLLMResponse
	for _, frame := range strings.Split(string(wire), "\n\n") {
		if frame == "" || frame == "data: [DONE]" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("invalid SSE frame: %q", frame)
		}
		var event model.InternalLLMResponse
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func thinkingChoice(t *testing.T, raw string) *model.InternalLLMResponse {
	t.Helper()
	if raw == "DONE" {
		return &model.InternalLLMResponse{Object: "[DONE]"}
	}
	var chunk model.InternalLLMResponse
	if err := json.Unmarshal([]byte(`{"object":"chat.completion.chunk","choices":[`+raw+`]}`), &chunk); err != nil {
		t.Fatal(err)
	}
	return &chunk
}

func TestChatThinkingBoundariesWire(t *testing.T) {
	const open, close = "<think>\n", "\n</think>\n"
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"empty_delta_finish", []string{`{"delta":{"reasoning_content":"r"}}`, `{"delta":{},"finish_reason":"stop"}`, "DONE"}, open + "r" + close},
		{"nil_delta_finish", []string{`{"delta":{"reasoning_content":"r"}}`, `{"finish_reason":"stop"}`}, open + "r" + close},
		{"first_reasoning_finish", []string{`{"delta":{"reasoning_content":"r"},"finish_reason":"stop"}`}, open + "r" + close},
		{"later_reasoning_finish", []string{`{"delta":{"reasoning_content":"r"}}`, `{"delta":{"reasoning_content":"s"},"finish_reason":"stop"}`}, open + "rs" + close},
		{"first_reasoning_answer", []string{`{"delta":{"reasoning_content":"r","content":"answer"}}`, `{"delta":{},"finish_reason":"stop"}`}, open + "r" + close + "answer"},
		{"later_reasoning_answer", []string{`{"delta":{"reasoning_content":"r"}}`, `{"delta":{"reasoning_content":"s","content":"answer"}}`}, open + "rs" + close + "answer"},
		{"done_without_finish", []string{`{"delta":{"reasoning_content":"r"}}`, "DONE", "DONE"}, open + "r" + close},
		{"late_reasoning", []string{`{"delta":{"reasoning_content":"r","content":"answer"}}`, `{"delta":{"reasoning_content":"late"}}`, `{"delta":{"reasoning_content":"r"}}`, "DONE"}, open + "r" + close + "answer" + open + "later" + close},
		{"whitespace_answer", []string{`{"delta":{"reasoning_content":"r","content":" \n"}}`, "DONE"}, open + "r" + close + " \n"},
		{"empty_content_is_not_answer", []string{`{"delta":{"reasoning_content":"r","content":""}}`, `{"delta":{"reasoning_content":"s","content":""}}`, "DONE"}, open + "rs" + close},
		{"reasoning_alias", []string{`{"delta":{"reasoning":"r","content":"answer"}}`}, open + "r" + close + "answer"},
		{"image_wire", []string{`{"delta":{"reasoning_content":"r","content":[{"type":"text","text":"answer"},{"type":"image_url","image_url":{"url":"https://upstream.example/image.png"}}]}}`}, open + "r" + close + "answer\n![image](https://upstream.example/image.png)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &ChatInbound{thinkingToContent: true}
			var text strings.Builder
			for _, raw := range tc.chunks {
				for _, event := range thinkingWire(t, in, thinkingChoice(t, raw)) {
					for _, c := range event.Choices {
						if c.Delta == nil {
							continue
						}
						if c.Delta.Content.Content != nil {
							text.WriteString(*c.Delta.Content.Content)
						}
						if c.Delta.GetReasoningContent() != "" {
							t.Fatal("folded reasoning duplicated")
						}
					}
				}
			}
			if text.String() != tc.want {
				t.Fatalf("wire = %q; want %q", text.String(), tc.want)
			}
		})
	}
}

func TestChatThinkingOptOutAndNoop(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		in := &ChatInbound{thinkingToContent: enabled}
		if enabled {
			thinkingWire(t, in, thinkingChoice(t, `{"delta":{"reasoning_content":"r"}}`))
		}
		for _, raw := range []string{`{"delta":{}}`, `{"delta":null}`, `{"delta":{"content":""}}`, `{"delta":{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}`} {
			chunk := thinkingChoice(t, raw)
			events := thinkingWire(t, in, chunk)
			if len(events) != 1 || !reflect.DeepEqual(events[0].Choices, chunk.Choices) {
				t.Fatalf("noop changed: %+v", events)
			}
		}
	}
	in := &ChatInbound{}
	chunk := thinkingChoice(t, `{"delta":{"reasoning_content":"r","reasoning":"alias","content":"answer"},"finish_reason":"stop"}`)
	events := thinkingWire(t, in, chunk)
	if len(events) != 1 || !reflect.DeepEqual(events[0], *chunk) {
		t.Fatal("opt-out changed")
	}
	if len(thinkingWire(t, in, thinkingChoice(t, "DONE"))) != 0 {
		t.Fatal("opt-out synthesized closer")
	}
}

func TestChatThinkingClosersAndRawAggregation(t *testing.T) {
	in := &ChatInbound{thinkingToContent: true}
	var chunk model.InternalLLMResponse
	if err := json.Unmarshal([]byte(`{"id":"cmpl-test","object":"chat.completion.chunk","created":42,"model":"test-model","system_fingerprint":"fp-test","service_tier":"standard","usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6},"choices":[{"index":7,"delta":{"reasoning_content":"b"}},{"index":2,"delta":{"reasoning_content":"a","tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"logprobs":{"content":[]}}]}`), &chunk); err != nil {
		t.Fatal(err)
	}
	events := thinkingWire(t, in, &chunk)
	if len(events) != 1 || !reflect.DeepEqual(events[0].Usage, chunk.Usage) ||
		!reflect.DeepEqual(events[0].Choices[1].Logprobs, chunk.Choices[1].Logprobs) ||
		!reflect.DeepEqual(events[0].Choices[1].Delta.ToolCalls, chunk.Choices[1].Delta.ToolCalls) {
		t.Fatal("wire metadata lost")
	}
	closers := thinkingWire(t, in, thinkingChoice(t, "DONE"))
	if len(closers) != 1 {
		t.Fatalf("closers = %+v", closers)
	}
	c := closers[0]
	if len(c.Choices) != 2 || c.Choices[0].Index != 2 || c.Choices[1].Index != 7 {
		t.Fatal("non-deterministic sparse choice closure")
	}
	if c.ID != chunk.ID || c.Created != chunk.Created || c.Model != chunk.Model || c.SystemFingerprint != chunk.SystemFingerprint || c.ServiceTier != chunk.ServiceTier {
		t.Fatal("closing metadata lost")
	}
	if c.Usage != nil || c.Error != nil {
		t.Fatal("synthetic metadata duplicated")
	}
	for _, choice := range c.Choices {
		if choice.FinishReason != nil || choice.Logprobs != nil || choice.Delta == nil || len(choice.Delta.ToolCalls) != 0 || choice.Delta.Content.Content == nil || *choice.Delta.Content.Content != "\n</think>\n" {
			t.Fatalf("invalid closer: %+v", choice)
		}
	}
	if len(thinkingWire(t, in, thinkingChoice(t, "DONE"))) != 0 {
		t.Fatal("duplicate closers")
	}
	if len(in.streamChunks) != 1 {
		t.Fatal("synthetic chunk entered aggregation")
	}
	agg, err := in.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if agg == nil || len(agg.Choices) != 2 || agg.Choices[0].Message.GetReasoningContent() != "a" || agg.Choices[1].Message.GetReasoningContent() != "b" || !reflect.DeepEqual(agg.Usage, chunk.Usage) {
		t.Fatalf("raw aggregation corrupted: %+v", agg)
	}
	for _, choice := range agg.Choices {
		if p := choice.Message.Content.Content; p != nil && strings.Contains(*p, "<think>") {
			t.Fatal("wire marker entered aggregation")
		}
	}
}

func TestChatThinkingChoiceFinishIndependent(t *testing.T) {
	in := &ChatInbound{thinkingToContent: true}
	chunk := thinkingChoice(t, `{"index":7,"delta":{"reasoning_content":"b"}},{"index":2,"delta":{"reasoning_content":"a"}}`)
	thinkingWire(t, in, chunk)
	finish := thinkingChoice(t, `{"index":2,"delta":{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}`)
	events := thinkingWire(t, in, finish)
	if len(events) != 1 || events[0].Choices[0].FinishReason == nil || *events[0].Choices[0].FinishReason != "tool_calls" || !reflect.DeepEqual(events[0].Choices[0].Delta.ToolCalls, finish.Choices[0].Delta.ToolCalls) {
		t.Fatal("finish or tool lost")
	}
	closers := thinkingWire(t, in, thinkingChoice(t, "DONE"))
	if len(closers) != 1 || len(closers[0].Choices) != 1 || closers[0].Choices[0].Index != 7 {
		t.Fatal("choice state not independent")
	}
}

func TestChatThinkingMultipartPreserved(t *testing.T) {
	in := &ChatInbound{thinkingToContent: true}
	chunk := thinkingChoice(t, `{"delta":{"reasoning_content":"r","content":[{"type":"text","text":"answer"},{"type":"file","file":{"filename":"test.txt","file_data":"data:text/plain;base64,WA=="}},{"type":"input_audio","audio":{"format":"wav","data":"WA=="}}]}}`)
	events := thinkingWire(t, in, chunk)
	if len(events) != 1 || len(events[0].Choices) != 1 {
		t.Fatal("multipart event missing")
	}
	parts := events[0].Choices[0].Delta.Content.MultipleContent
	if len(parts) != 4 || parts[0].Text == nil || *parts[0].Text != "<think>\nr\n</think>\n" || !reflect.DeepEqual(parts[1:], chunk.Choices[0].Delta.Content.MultipleContent) {
		t.Fatalf("multipart content lost: %+v", parts)
	}
}
