package openai

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// collectWire concatenates content and reasoning seen on the wire for choice 0.
func collectWire(t *testing.T, in *ChatInbound, raws []string) (content, reasoning string) {
	t.Helper()
	var c, r strings.Builder
	for _, raw := range raws {
		for _, ev := range thinkingWire(t, in, thinkingChoice(t, raw)) {
			for _, ch := range ev.Choices {
				if ch.Delta == nil {
					continue
				}
				if ch.Delta.Content.Content != nil {
					c.WriteString(*ch.Delta.Content.Content)
				}
				r.WriteString(ch.Delta.GetReasoningContent())
			}
		}
	}
	return c.String(), r.String()
}

// Stream and non-stream produce the same visible text when the switch is on,
// and neither keeps reasoning_content (new-api style, 2026-10-31).
func TestThinkingToContentStreamNonStreamParity(t *testing.T) {
	cases := []struct {
		name              string
		chunks            []string
		reasoning, answer string
	}{
		{"reasoning_then_answer", []string{
			`{"delta":{"reasoning_content":"step 1, "}}`,
			`{"delta":{"reasoning_content":"step 2"}}`,
			`{"delta":{"content":"Hello"}}`,
			`{"delta":{"content":" world"},"finish_reason":"stop"}`,
			"DONE",
		}, "step 1, step 2", "Hello world"},
		{"reasoning_only", []string{
			`{"delta":{"reasoning_content":"only thinking"}}`,
			`{"delta":{},"finish_reason":"length"}`,
			"DONE",
		}, "only thinking", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streamContent, streamReasoning := collectWire(t, &ChatInbound{thinkingToContent: true}, tc.chunks)
			if streamReasoning != "" {
				t.Fatalf("stream kept reasoning_content: %q", streamReasoning)
			}

			reasoning := tc.reasoning
			msg := &model.Message{Role: "assistant", ReasoningContent: &reasoning}
			if tc.answer != "" {
				answer := tc.answer
				msg.Content = model.MessageContent{Content: &answer}
			}
			resp := &model.InternalLLMResponse{Object: "chat.completion", Choices: []model.Choice{{Message: msg}}}
			in := &ChatInbound{thinkingToContent: true}
			body, err := in.TransformResponse(context.Background(), resp)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Choices []struct {
					Message map[string]any `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			got, _ := wire.Choices[0].Message["content"].(string)
			if got != streamContent {
				t.Fatalf("parity broken:\n stream=%q\nnonstrm=%q", streamContent, got)
			}
			if _, ok := wire.Choices[0].Message["reasoning_content"]; ok {
				t.Fatal("non-stream kept reasoning_content")
			}
			want := "<think>\n" + tc.reasoning + "\n</think>\n" + tc.answer
			if got != want {
				t.Fatalf("content = %q, want %q", got, want)
			}
			// stored response (billing / logs) untouched
			if in.storedResponse != resp || resp.Choices[0].Message.ReasoningContent == nil || *resp.Choices[0].Message.ReasoningContent != tc.reasoning {
				t.Fatal("stored response was mutated")
			}
		})
	}
}

// Non-stream tool-call turn: message untouched (no <think>, reasoning kept).
func TestThinkingToContentNonStreamSkipsToolCallTurn(t *testing.T) {
	var resp model.InternalLLMResponse
	if err := json.Unmarshal([]byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"need lookup","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), &resp); err != nil {
		t.Fatal(err)
	}
	if out := applyThinkingToContentNonStream(&resp, true); out != &resp {
		t.Fatal("tool-call turn must be returned unchanged")
	}
}

// Stream tool-call turn: reasoning that arrives together with / after tool_calls
// stays reasoning_content; an already-open <think> is still closed on finish.
func TestThinkingToContentStreamSkipsToolCallTurn(t *testing.T) {
	tool := `"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]`

	// 1) tool_calls from the first chunk: nothing folded at all.
	content, reasoning := collectWire(t, &ChatInbound{thinkingToContent: true}, []string{
		`{"delta":{"reasoning_content":"need lookup",` + tool + `}}`,
		`{"delta":{"reasoning_content":" more"}}`,
		`{"delta":{},"finish_reason":"tool_calls"}`,
		"DONE",
	})
	if content != "" || reasoning != "need lookup more" {
		t.Fatalf("tool-call turn folded: content=%q reasoning=%q", content, reasoning)
	}

	// 2) reasoning first (already streamed, cannot be recalled), then tool_calls:
	// later reasoning is not folded, and the open <think> closes exactly once.
	in := &ChatInbound{thinkingToContent: true}
	content, reasoning = collectWire(t, in, []string{
		`{"delta":{"reasoning_content":"plan"}}`,
		`{"delta":{` + tool + `}}`,
		`{"delta":{"reasoning_content":" after"}}`,
		`{"delta":{},"finish_reason":"tool_calls"}`,
		"DONE",
	})
	if content != "<think>\nplan\n</think>\n" || reasoning != " after" {
		t.Fatalf("content=%q reasoning=%q", content, reasoning)
	}

	// tool_calls survive on the wire unchanged
	in = &ChatInbound{thinkingToContent: true}
	chunk := thinkingChoice(t, `{"delta":{`+tool+`}}`)
	events := thinkingWire(t, in, chunk)
	if len(events) != 1 || !reflect.DeepEqual(events[0].Choices[0].Delta.ToolCalls, chunk.Choices[0].Delta.ToolCalls) {
		t.Fatal("tool_calls changed on the wire")
	}
}
