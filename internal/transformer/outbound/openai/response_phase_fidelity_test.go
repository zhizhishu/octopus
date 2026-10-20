package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samber/lo"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// A Responses input/output message item carrying `phase` (the codex CLI emits e.g.
// "commentary") must survive an unmarshal -> marshal round trip, with `phase`
// emitted after `content`. Regression: ResponsesItem had no Phase field, so a
// same-protocol relay dropped it and the client echoed a phase-less item back on
// the following turn.
func TestResponsesItemPhaseRoundTrips(t *testing.T) {
	src := `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}],"phase":"commentary"}`

	var item ResponsesItem
	if err := json.Unmarshal([]byte(src), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.Phase == nil || *item.Phase != "commentary" {
		t.Fatalf("phase not decoded: %+v", item.Phase)
	}

	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"phase":"commentary"`) {
		t.Fatalf("phase dropped on re-encode: %s", got)
	}
	ci, pi := strings.Index(got, `"content"`), strings.Index(got, `"phase"`)
	if ci < 0 || pi < 0 || pi < ci {
		t.Fatalf("phase must be emitted after content, got %s", got)
	}
}

// convertAssistantMessageToResponses (the rebuilt-input path) must re-emit a
// carried phase on the assistant message item so a bridged/replayed history keeps
// it.
func TestConvertAssistantMessageToResponsesPreservesPhase(t *testing.T) {
	phase := "commentary"
	text := "hello"
	msg := model.Message{
		Role:           "assistant",
		Content:        model.MessageContent{Content: &text},
		ResponsesPhase: &phase,
	}

	items := convertAssistantMessageToResponses(msg)
	var messageItem *ResponsesItem
	for i := range items {
		if items[i].Type == "message" {
			messageItem = &items[i]
		}
	}
	if messageItem == nil {
		t.Fatalf("no message item rebuilt: %+v", items)
	}
	if messageItem.Phase == nil || *messageItem.Phase != "commentary" {
		t.Fatalf("rebuilt message item lost phase: %+v", messageItem)
	}
}

// The upstream->internal hop (non-stream) must lift a message item's phase onto
// the internal message so the inbound rebuild can re-emit it to the client.
func TestConvertToLLMResponseFromResponsesLiftsMessagePhase(t *testing.T) {
	resp := &ResponsesResponse{
		Output: []ResponsesItem{
			{
				Type:    "message",
				Role:    "assistant",
				Phase:   lo.ToPtr("commentary"),
				Content: &ResponsesInput{Items: []ResponsesItem{{Type: "output_text", Text: lo.ToPtr("hi")}}},
			},
		},
	}
	converted := convertToLLMResponseFromResponses(resp)
	if converted == nil || len(converted.Choices) == 0 || converted.Choices[0].Message == nil {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
	got := converted.Choices[0].Message.ResponsesPhase
	if got == nil || *got != "commentary" {
		t.Fatalf("message phase not lifted: %+v", got)
	}
}

// The streaming outbound must carry a message item's phase onto the chunk so the
// inbound streaming rebuild can emit it on the client-facing message item.
func TestResponseOutboundStreamCarriesMessagePhase(t *testing.T) {
	o := &ResponseOutbound{}
	event := []byte(`{"type":"response.output_item.added","output_index":0,` +
		`"item":{"id":"msg_1","type":"message","role":"assistant","content":[],"phase":"commentary"}}`)

	resp, err := o.TransformStream(context.Background(), event)
	if err != nil {
		t.Fatalf("TransformStream: %v", err)
	}
	if resp == nil || len(resp.Choices) == 0 || resp.Choices[0].Delta == nil {
		t.Fatalf("no chunk emitted for message phase: %+v", resp)
	}
	got := resp.Choices[0].Delta.ResponsesPhase
	if got == nil || *got != "commentary" {
		t.Fatalf("stream chunk lost phase: %+v", got)
	}
}
