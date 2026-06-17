package openai

import (
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertAudioToInputAudio verifies an internal "input_audio" part is rebuilt
// into an OpenAI Responses input_audio item, preserving base64 data and format.
func TestConvertAudioToInputAudio(t *testing.T) {
	part := model.MessageContentPart{
		Type:  "input_audio",
		Audio: &model.Audio{Data: "AAECaXVkaW8=", Format: "wav"},
	}
	item := convertAudioToInputAudio(part)
	if item == nil {
		t.Fatal("expected non-nil input_audio item")
	}
	if item.Type != "input_audio" {
		t.Fatalf("item.Type = %q, want input_audio", item.Type)
	}
	if item.InputAudio == nil {
		t.Fatal("expected non-nil InputAudio payload")
	}
	if item.InputAudio.Data != "AAECaXVkaW8=" {
		t.Fatalf("audio data not preserved: %q", item.InputAudio.Data)
	}
	if item.InputAudio.Format != "wav" {
		t.Fatalf("audio format not preserved: %q", item.InputAudio.Format)
	}
}

// TestConvertAudioToInputAudioNil verifies missing/empty audio yields nil so the
// item is not emitted.
func TestConvertAudioToInputAudioNil(t *testing.T) {
	if convertAudioToInputAudio(model.MessageContentPart{Type: "input_audio"}) != nil {
		t.Fatal("nil Audio should yield nil item")
	}
	if convertAudioToInputAudio(model.MessageContentPart{
		Type:  "input_audio",
		Audio: &model.Audio{Format: "mp3"},
	}) != nil {
		t.Fatal("empty audio data should yield nil item")
	}
}

// TestConvertUserMessageAudioRoundTrip verifies that an internal user message
// carrying an Audio part (as produced by chat/gemini inbounds) survives the
// outbound conversion to a Responses message item and marshals to the expected
// wire shape: {type:"input_audio", input_audio:{data, format}}.
func TestConvertUserMessageAudioRoundTrip(t *testing.T) {
	msg := model.Message{
		Role: "user",
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: stringPtr("transcribe this")},
				{Type: "input_audio", Audio: &model.Audio{Data: "Zm9vYmFy", Format: "mp3"}},
			},
		},
	}

	item := convertUserMessageToResponses(msg)
	if item.Content == nil {
		t.Fatal("expected content items")
	}
	if len(item.Content.Items) != 2 {
		t.Fatalf("expected 2 content items, got %d", len(item.Content.Items))
	}

	audio := item.Content.Items[1]
	if audio.Type != "input_audio" || audio.InputAudio == nil {
		t.Fatalf("audio item not built: %#v", audio)
	}
	if audio.InputAudio.Data != "Zm9vYmFy" || audio.InputAudio.Format != "mp3" {
		t.Fatalf("audio payload mismatch: %#v", audio.InputAudio)
	}

	// Confirm the marshaled JSON nests the audio under "input_audio".
	raw, err := json.Marshal(audio)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Type       string `json:"type"`
		InputAudio struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "input_audio" {
		t.Fatalf("wire type = %q, want input_audio (json: %s)", decoded.Type, raw)
	}
	if decoded.InputAudio.Data != "Zm9vYmFy" || decoded.InputAudio.Format != "mp3" {
		t.Fatalf("wire input_audio = %+v (json: %s)", decoded.InputAudio, raw)
	}
}
