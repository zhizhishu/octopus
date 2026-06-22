package authropic

import (
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertAudioToPlaceholderBlock verifies an OpenAI input_audio part degrades
// to a visible text placeholder (rather than being silently dropped) when routed
// over an Anthropic channel, which has no audio input block.
func TestConvertAudioToPlaceholderBlock(t *testing.T) {
	part := model.MessageContentPart{
		Type:  "input_audio",
		Audio: &model.Audio{Format: "mp3", Data: "AAAA"},
	}
	block := convertAudioToPlaceholderBlock(part)
	if block == nil {
		t.Fatal("expected non-nil placeholder block")
	}
	if block.Type != "text" {
		t.Fatalf("block.Type = %q, want text", block.Type)
	}
	if block.Text == nil || !strings.Contains(*block.Text, "audio input") || !strings.Contains(*block.Text, "mp3") {
		t.Fatalf("placeholder text not informative: %#v", block.Text)
	}
}

// TestConvertAudioToPlaceholderBlockUnknownFormat verifies a missing format is
// labeled rather than producing an empty placeholder.
func TestConvertAudioToPlaceholderBlockUnknownFormat(t *testing.T) {
	part := model.MessageContentPart{
		Type:  "input_audio",
		Audio: &model.Audio{Data: "AAAA"},
	}
	block := convertAudioToPlaceholderBlock(part)
	if block == nil || block.Text == nil || !strings.Contains(*block.Text, "unknown") {
		t.Fatalf("expected unknown-format placeholder, got %#v", block)
	}
}

// TestConvertAudioToPlaceholderBlockNil verifies a part with no audio payload
// produces no block.
func TestConvertAudioToPlaceholderBlockNil(t *testing.T) {
	if convertAudioToPlaceholderBlock(model.MessageContentPart{Type: "input_audio"}) != nil {
		t.Fatal("nil audio should produce nil block")
	}
}

// TestUserMessageAudioDegradesNotDropped verifies the end-to-end user-message
// path keeps the audio turn as a visible block instead of dropping it entirely.
func TestUserMessageAudioDegradesNotDropped(t *testing.T) {
	text := "describe this clip"
	msg := model.Message{
		Role: "user",
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: &text},
				{Type: "input_audio", Audio: &model.Audio{Format: "wav", Data: "AAAA"}},
			},
		},
	}
	content := convertMultiplePartContent(msg)
	if len(content.MultipleContent) != 2 {
		t.Fatalf("expected 2 blocks (text + audio placeholder), got %d: %#v", len(content.MultipleContent), content.MultipleContent)
	}
	// The second block must be the audio placeholder (text type, descriptive).
	last := content.MultipleContent[1]
	if last.Type != "text" || last.Text == nil || !strings.Contains(*last.Text, "audio input") {
		t.Fatalf("audio turn was not degraded to a placeholder: %#v", last)
	}
}

// TestDocumentURLInfersMediaTypeWhenMissing verifies a remote document URL with
// no explicit MediaType gets one inferred from the URL extension, so the
// Anthropic document block is not left with an empty media type.
func TestDocumentURLInfersMediaTypeWhenMissing(t *testing.T) {
	part := model.MessageContentPart{
		Type: "file",
		File: &model.File{FileURL: "https://example.com/report.pdf"},
	}
	block := convertFileToDocumentBlock(part)
	if block == nil || block.Source == nil || block.Source.Type != "url" {
		t.Fatalf("expected url document block, got %#v", block)
	}
	if block.Source.MediaType != "application/pdf" {
		t.Fatalf("expected inferred application/pdf, got %q", block.Source.MediaType)
	}
}

// TestDocumentURLExplicitMediaTypeWins verifies an explicit MediaType is not
// overridden by URL-extension inference.
func TestDocumentURLExplicitMediaTypeWins(t *testing.T) {
	part := model.MessageContentPart{
		Type: "file",
		File: &model.File{FileURL: "https://example.com/data.pdf", MediaType: "application/octet-stream"},
	}
	block := convertFileToDocumentBlock(part)
	if block == nil || block.Source == nil || block.Source.MediaType != "application/octet-stream" {
		t.Fatalf("explicit media type should win, got %#v", block)
	}
}
