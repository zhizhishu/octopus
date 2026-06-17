package relay

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestMessageToSyntheticDeltasPreservesSignature pins the fix for the non-stream
// -> stream fallback path (used when an Anthropic streaming upstream returns 5xx
// and octopus retries non-stream, then re-synthesizes a stream for the client).
// The reasoning delta must carry the Anthropic thinking signature in the SAME
// delta as the reasoning text, because the Anthropic inbound stream synthesizer
// only emits a signature_delta while the thinking content block is still open.
// Losing the signature gets the rebuilt thinking block rejected on the next turn.
func TestMessageToSyntheticDeltasPreservesSignature(t *testing.T) {
	reasoning := "let me think about it"
	signature := "sig-abc-123"
	msg := &model.Message{
		Role:               "assistant",
		ReasoningContent:   &reasoning,
		ReasoningSignature: &signature,
		Content:            model.MessageContent{Content: strPtr("final answer")},
	}

	deltas := messageToSyntheticDeltas(0, msg)

	var reasoningDelta *model.Message
	for _, d := range deltas {
		if d.Delta != nil && d.Delta.ReasoningContent != nil {
			reasoningDelta = d.Delta
			break
		}
	}
	if reasoningDelta == nil {
		t.Fatalf("expected a reasoning delta, got deltas=%+v", deltas)
	}
	if reasoningDelta.ReasoningContent == nil || *reasoningDelta.ReasoningContent != reasoning {
		t.Fatalf("reasoning content lost: %+v", reasoningDelta.ReasoningContent)
	}
	if reasoningDelta.ReasoningSignature == nil || *reasoningDelta.ReasoningSignature != signature {
		t.Fatalf("reasoning signature must be carried in the same delta as reasoning content, got %+v", reasoningDelta.ReasoningSignature)
	}
}

// TestMessageToSyntheticDeltasSignatureWithoutReasoning ensures a signature is
// not silently dropped even if reasoning text is absent.
func TestMessageToSyntheticDeltasSignatureWithoutReasoning(t *testing.T) {
	signature := "sig-only"
	msg := &model.Message{
		Role:               "assistant",
		ReasoningSignature: &signature,
	}

	deltas := messageToSyntheticDeltas(0, msg)

	found := false
	for _, d := range deltas {
		if d.Delta != nil && d.Delta.ReasoningSignature != nil && *d.Delta.ReasoningSignature == signature {
			found = true
		}
	}
	if !found {
		t.Fatalf("signature-only message must still emit a signature delta, got %+v", deltas)
	}
}

// TestMessageToSyntheticDeltasPreservesImagesField checks that images carried in
// msg.Images survive the non-stream -> stream synthesis. The downstream OpenAI
// inbound merges Delta.Images back into the aggregated message; dropping them
// here means an image response is lost after the stream fallback.
func TestMessageToSyntheticDeltasPreservesImagesField(t *testing.T) {
	img := model.MessageContentPart{
		Type:     "image_url",
		ImageURL: &model.ImageURL{URL: "data:image/png;base64,AAAA"},
	}
	msg := &model.Message{
		Role:    "assistant",
		Images:  []model.MessageContentPart{img},
		Content: model.MessageContent{Content: strPtr("here is your image")},
	}

	deltas := messageToSyntheticDeltas(0, msg)

	if !hasImageDelta(deltas, "data:image/png;base64,AAAA") {
		t.Fatalf("expected an image delta carrying msg.Images, got %+v", deltas)
	}
}

// TestMessageToSyntheticDeltasPreservesInlineImageParts checks image parts inlined
// in Content.MultipleContent (not in msg.Images) are also preserved, since the old
// messageText() path only kept text parts and dropped images.
func TestMessageToSyntheticDeltasPreservesInlineImageParts(t *testing.T) {
	text := "look at this"
	msg := &model.Message{
		Role: "assistant",
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: &text},
				{Type: "image_url", ImageURL: &model.ImageURL{URL: "data:image/jpeg;base64,BBBB"}},
			},
		},
	}

	deltas := messageToSyntheticDeltas(0, msg)

	// Text must still be present.
	gotText := false
	for _, d := range deltas {
		if d.Delta != nil && d.Delta.Content.Content != nil && *d.Delta.Content.Content == text {
			gotText = true
		}
	}
	if !gotText {
		t.Fatalf("text part lost during synthesis, got %+v", deltas)
	}
	if !hasImageDelta(deltas, "data:image/jpeg;base64,BBBB") {
		t.Fatalf("inline image part must be preserved, got %+v", deltas)
	}
}

// TestMessageToSyntheticDeltasRegularPathUnchanged guards the common text+tool path
// so the fix does not regress non-reasoning, non-image responses (e.g. codex).
func TestMessageToSyntheticDeltasRegularPathUnchanged(t *testing.T) {
	msg := &model.Message{
		Role:    "assistant",
		Content: model.MessageContent{Content: strPtr("plain reply")},
		ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "do_thing", Arguments: "{}"}},
		},
	}

	deltas := messageToSyntheticDeltas(0, msg)

	for _, d := range deltas {
		if d.Delta == nil {
			continue
		}
		if d.Delta.ReasoningSignature != nil {
			t.Fatalf("regular path must not emit a signature delta, got %+v", d.Delta)
		}
		if len(d.Delta.Images) > 0 {
			t.Fatalf("regular path must not emit an image delta, got %+v", d.Delta)
		}
	}

	gotText, gotTool := false, false
	for _, d := range deltas {
		if d.Delta == nil {
			continue
		}
		if d.Delta.Content.Content != nil && *d.Delta.Content.Content == "plain reply" {
			gotText = true
		}
		if len(d.Delta.ToolCalls) > 0 && d.Delta.ToolCalls[0].ID == "call_1" {
			gotTool = true
		}
	}
	if !gotText || !gotTool {
		t.Fatalf("regular text+tool path changed: text=%v tool=%v deltas=%+v", gotText, gotTool, deltas)
	}
}

func hasImageDelta(deltas []model.Choice, wantURL string) bool {
	for _, d := range deltas {
		if d.Delta == nil {
			continue
		}
		for _, part := range d.Delta.Images {
			if part.ImageURL != nil && part.ImageURL.URL == wantURL {
				return true
			}
		}
	}
	return false
}
