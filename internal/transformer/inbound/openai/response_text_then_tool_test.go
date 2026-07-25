package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestResponseInboundTextThenToolClosesMessageFirst verifies the sequential
// Responses ordering: when a model produces text and THEN a tool call in the same
// turn, the message item is fully finalized (output_item.done) BEFORE the
// function_call item is announced (output_item.added). Emitting the function_call
// while the message item is still open interleaves two output items — a codex
// client's item state machine expects each item's done before the next item's
// added. Mirrors CLIProxyAPI's "finalize the message to match Codex expected
// ordering" / "Responses streaming requires message done events before the next
// output_item.added".
func TestResponseInboundTextThenToolClosesMessageFirst(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_seq", Model: "claude-opus-4-8", Object: "chat.completion.chunk", Created: 1}
	}
	text := func(s string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr(s)}}}}
		return c
	}
	toolCall := func(index int, id, name, args string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args},
		}}}}}
		return c
	}

	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// text -> tool call -> finish(tool_calls)
	feed(text("Let me check the weather. "))
	feed(toolCall(0, "call_w", "get_weather", `{"city":"Tokyo"}`))
	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)
	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())

	// Still lifecycle-valid.
	assertResponsesStreamItemLifecycle(t, events)

	msgDoneIdx, fcAddedIdx := -1, -1
	for i, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "message" {
			msgDoneIdx = i
		}
		if ev.Type == "response.output_item.added" && ev.Item != nil && ev.Item.Type == "function_call" && fcAddedIdx == -1 {
			fcAddedIdx = i
		}
	}
	if msgDoneIdx == -1 {
		t.Fatal("no message output_item.done emitted")
	}
	if fcAddedIdx == -1 {
		t.Fatal("no function_call output_item.added emitted")
	}
	// Sequential: the message must finalize before the function_call opens.
	if msgDoneIdx > fcAddedIdx {
		t.Errorf("message item must finalize BEFORE function_call opens (interleaved): output_item.done(message)@%d vs output_item.added(function_call)@%d", msgDoneIdx, fcAddedIdx)
	}
	// No message text events may leak out after the function_call opened.
	for i := fcAddedIdx; i < len(events); i++ {
		if events[i].Type == "response.output_text.delta" || events[i].Type == "response.output_text.done" {
			t.Errorf("message text event %s@%d appears after function_call opened@%d", events[i].Type, i, fcAddedIdx)
		}
	}
}
