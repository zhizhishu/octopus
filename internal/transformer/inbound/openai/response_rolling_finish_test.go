package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// buildRollingFinishChunk mimics the antigravity-backed gemini-3.7-flash channel,
// which stamps EVERY streamed chunk with finish_reason="stop" plus a rolling usage
// block instead of reserving them for the terminal chunk.
func buildRollingFinishChunk(text string, completionTokens int64) *model.InternalLLMResponse {
	return &model.InternalLLMResponse{
		ID:    "resp_rolling_finish",
		Model: "gemini-3.7-flash-high",
		Choices: []model.Choice{
			{
				Index:        0,
				FinishReason: lo.ToPtr("stop"),
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: lo.ToPtr(text)},
				},
			},
		},
		Usage: &model.Usage{
			PromptTokens:     186,
			CompletionTokens: completionTokens,
			TotalTokens:      186 + completionTokens,
		},
	}
}

func collectStreamedText(t *testing.T, rawEvents string) string {
	t.Helper()

	var streamedText strings.Builder
	for _, line := range strings.Split(rawEvents, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.Type == "response.output_text.delta" {
			streamedText.WriteString(event.Delta)
		}
	}
	return streamedText.String()
}

// TestResponseInboundKeepsStreamingWhenEveryChunkCarriesFinishAndUsage locks the fix
// for "gemini only answers a few words in Cursor".
//
// The upstream repeats finish_reason="stop" AND a usage block on every chunk. The old
// transformer treated the first such chunk as terminal: it set hasFinished, emitted
// response.completed, and the `!responseCompleted` guard then discarded every later
// chunk. Downstream that looked like the model produced a handful of characters and
// stopped, while the non-streaming path on the very same channel returned the full
// answer.
func TestResponseInboundKeepsStreamingWhenEveryChunkCarriesFinishAndUsage(t *testing.T) {
	inbound := &ResponseInbound{model: "gemini-3.7-flash-high"}
	ctx := context.Background()

	fragments := []string{
		"第一段：Config 结构体定义了服务运行期配置。",
		"第二段：ListenAddr 指定监听地址。",
		"第三段：MaxRetries 控制重试上限。",
		"第四段：EnableTLS 决定是否启用传输加密。",
		"第五段：以上是完整回答的结尾。",
	}

	var allEvents strings.Builder
	for fragmentIndex, fragment := range fragments {
		producedEvents, err := inbound.TransformStream(
			ctx,
			buildRollingFinishChunk(fragment, int64(20*(fragmentIndex+1))),
		)
		if err != nil {
			t.Fatalf("chunk %d failed: %v", fragmentIndex, err)
		}
		allEvents.Write(producedEvents)
	}

	doneEvents, err := inbound.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE] failed: %v", err)
	}
	allEvents.Write(doneEvents)

	rawEvents := allEvents.String()
	streamedText := collectStreamedText(t, rawEvents)

	for _, fragment := range fragments {
		if !strings.Contains(streamedText, fragment) {
			t.Fatalf(
				"fragment %q was dropped from the stream.\nstreamed text: %q\nraw events:\n%s",
				fragment, streamedText, rawEvents,
			)
		}
	}

	expectedFullText := strings.Join(fragments, "")
	if streamedText != expectedFullText {
		t.Fatalf("streamed text does not match upstream text.\n got: %q\nwant: %q", streamedText, expectedFullText)
	}

	if terminalEventCount := strings.Count(rawEvents, `"type":"response.completed"`); terminalEventCount != 1 {
		t.Fatalf("expected exactly one response.completed event, got %d.\nraw events:\n%s", terminalEventCount, rawEvents)
	}

	// The terminal event must arrive after the last text delta, never before it.
	// Compared against the last delta event rather than the last occurrence of the
	// final fragment, because response.completed itself echoes the full text inside
	// its output array.
	terminalEventOffset := strings.Index(rawEvents, `"type":"response.completed"`)
	lastTextDeltaOffset := strings.LastIndex(rawEvents, `"type":"response.output_text.delta"`)
	if lastTextDeltaOffset == -1 {
		t.Fatalf("no text delta events were emitted at all:\n%s", rawEvents)
	}
	if terminalEventOffset < lastTextDeltaOffset {
		t.Fatalf("response.completed was emitted before the final text delta (offset %d < %d)",
			terminalEventOffset, lastTextDeltaOffset)
	}
}

// TestResponseInboundClosesImmediatelyOnCleanTerminalChunk guards the other side of
// the fix: a well-behaved upstream that sends finish_reason on an EMPTY terminal chunk
// must still complete right there, without waiting for [DONE]. Deferring finalization
// for every upstream would delay the terminal event for well-behaved channels.
func TestResponseInboundClosesImmediatelyOnCleanTerminalChunk(t *testing.T) {
	inbound := &ResponseInbound{model: "gpt-5.6_Reasoning"}
	ctx := context.Background()

	textChunk := &model.InternalLLMResponse{
		ID:    "resp_clean_terminal",
		Model: "gpt-5.6_Reasoning",
		Choices: []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: lo.ToPtr("完整回答内容。")},
				},
			},
		},
	}
	if _, err := inbound.TransformStream(ctx, textChunk); err != nil {
		t.Fatalf("text chunk failed: %v", err)
	}

	terminalChunk := &model.InternalLLMResponse{
		ID:    "resp_clean_terminal",
		Model: "gpt-5.6_Reasoning",
		Choices: []model.Choice{
			{
				Index:        0,
				FinishReason: lo.ToPtr("stop"),
				Delta:        &model.Message{},
			},
		},
		Usage: &model.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18},
	}
	terminalEvents, err := inbound.TransformStream(ctx, terminalChunk)
	if err != nil {
		t.Fatalf("terminal chunk failed: %v", err)
	}

	if !strings.Contains(string(terminalEvents), `"type":"response.completed"`) {
		t.Fatalf("a clean terminal chunk must complete the response immediately, got:\n%s", string(terminalEvents))
	}
}

// TestResponseInboundRollingFinishStillDeliversToolCalls checks that the deferred
// finalization path does not break tool calls: Cursor routes a tool call by the
// function_call item, and an upstream repeating finish_reason must not truncate the
// tool arguments either.
func TestResponseInboundRollingFinishStillDeliversToolCalls(t *testing.T) {
	inbound := &ResponseInbound{model: "gemini-3.7-flash-high"}
	ctx := context.Background()

	argumentFragments := []string{`{"path": "internal/`, `config/config.go"}`}

	var allEvents strings.Builder
	for fragmentIndex, argumentFragment := range argumentFragments {
		toolName := ""
		if fragmentIndex == 0 {
			toolName = "read_project_file"
		}
		chunk := &model.InternalLLMResponse{
			ID:    "resp_rolling_tool",
			Model: "gemini-3.7-flash-high",
			Choices: []model.Choice{
				{
					Index:        0,
					FinishReason: lo.ToPtr("tool_calls"),
					Delta: &model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{
								Index: 0,
								ID:    "call_rolling_1",
								Type:  "function",
								Function: model.FunctionCall{
									Name:      toolName,
									Arguments: argumentFragment,
								},
							},
						},
					},
				},
			},
			Usage: &model.Usage{PromptTokens: 50, CompletionTokens: int64(10 * (fragmentIndex + 1)), TotalTokens: 60},
		}
		producedEvents, err := inbound.TransformStream(ctx, chunk)
		if err != nil {
			t.Fatalf("tool chunk %d failed: %v", fragmentIndex, err)
		}
		allEvents.Write(producedEvents)
	}

	doneEvents, err := inbound.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("[DONE] failed: %v", err)
	}
	allEvents.Write(doneEvents)

	rawEvents := allEvents.String()
	if !strings.Contains(rawEvents, `"name":"read_project_file"`) {
		t.Fatalf("tool name missing from stream:\n%s", rawEvents)
	}
	if !strings.Contains(rawEvents, "config/config.go") {
		t.Fatalf("second argument fragment was dropped:\n%s", rawEvents)
	}
	if terminalEventCount := strings.Count(rawEvents, `"type":"response.completed"`); terminalEventCount != 1 {
		t.Fatalf("expected exactly one response.completed event, got %d", terminalEventCount)
	}
}
