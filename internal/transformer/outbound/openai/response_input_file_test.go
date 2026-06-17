package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertFileToInputFileBase64 verifies an internal base64 "file" part is
// rebuilt into a Codex Responses input_file item (fix D outbound).
func TestConvertFileToInputFileBase64(t *testing.T) {
	part := model.MessageContentPart{
		Type: "file",
		File: &model.File{
			Filename: "report.pdf",
			FileData: "data:application/pdf;base64,JVBERi0xLjQK",
		},
	}
	item := convertFileToInputFile(part)
	if item == nil {
		t.Fatal("expected non-nil input_file item")
	}
	if item.Type != "input_file" {
		t.Fatalf("item.Type = %q, want input_file", item.Type)
	}
	if item.Filename == nil || *item.Filename != "report.pdf" {
		t.Fatalf("filename not preserved: %#v", item.Filename)
	}
	if item.FileData == nil || *item.FileData != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("file_data not preserved: %#v", item.FileData)
	}
}

// TestConvertFileToInputFileURLAndID covers url and id references.
func TestConvertFileToInputFileURLAndID(t *testing.T) {
	urlItem := convertFileToInputFile(model.MessageContentPart{
		Type: "file",
		File: &model.File{FileURL: "https://example.com/a.pdf"},
	})
	if urlItem == nil || urlItem.FileURL == nil || *urlItem.FileURL != "https://example.com/a.pdf" {
		t.Fatalf("file_url not preserved: %#v", urlItem)
	}

	idItem := convertFileToInputFile(model.MessageContentPart{
		Type: "file",
		File: &model.File{FileID: "file_123"},
	})
	if idItem == nil || idItem.FileID == nil || *idItem.FileID != "file_123" {
		t.Fatalf("file_id not preserved: %#v", idItem)
	}
}

// TestConvertFileToInputFileEmpty verifies a nil/empty file drops, while a
// filename-only part is now preserved (faithful passthrough — no silent loss).
func TestConvertFileToInputFileEmpty(t *testing.T) {
	if convertFileToInputFile(model.MessageContentPart{Type: "file"}) != nil {
		t.Fatal("nil file should produce nil item")
	}
	item := convertFileToInputFile(model.MessageContentPart{Type: "file", File: &model.File{Filename: "x.pdf"}})
	if item == nil || item.Filename == nil || *item.Filename != "x.pdf" {
		t.Fatalf("filename-only file should be preserved, got %#v", item)
	}
}

// TestConvertUserMessageToResponsesMixed verifies a user message carrying text,
// image and document parts rebuilds all three Responses content items, with the
// image still passed through (no regression).
func TestConvertUserMessageToResponsesMixed(t *testing.T) {
	text := "see attached"
	msg := model.Message{
		Role: "user",
		Content: model.MessageContent{
			MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: &text},
				{Type: "image_url", ImageURL: &model.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
				{Type: "file", File: &model.File{FileData: "data:application/pdf;base64,AAAA"}},
			},
		},
	}
	item := convertUserMessageToResponses(msg)
	if item.Content == nil {
		t.Fatal("expected content items")
	}
	items := item.Content.Items
	if len(items) != 3 {
		t.Fatalf("expected 3 content items, got %d", len(items))
	}
	if items[0].Type != "input_text" || items[1].Type != "input_image" || items[2].Type != "input_file" {
		t.Fatalf("unexpected item types: %s %s %s", items[0].Type, items[1].Type, items[2].Type)
	}
	if items[2].FileData == nil || *items[2].FileData != "data:application/pdf;base64,AAAA" {
		t.Fatalf("document file_data not preserved: %#v", items[2].FileData)
	}
}

// TestResponsesInputFileRoundTrip verifies the internal "file" carrier survives a
// rebuild back into an input_file item with all fields intact.
func TestResponsesInputFileRoundTrip(t *testing.T) {
	const dataURL = "data:application/pdf;base64,JVBERi0xLjQKMSAwIG9iag=="
	internal := model.MessageContentPart{
		Type: "file",
		File: &model.File{Filename: "r.pdf", FileData: dataURL},
	}
	item := convertFileToInputFile(internal)
	if item == nil || item.FileData == nil {
		t.Fatal("round-trip produced nil item/file_data")
	}
	if *item.FileData != dataURL {
		t.Fatalf("round-trip file_data = %q, want %q", *item.FileData, dataURL)
	}
	if item.Filename == nil || *item.Filename != "r.pdf" {
		t.Fatalf("round-trip filename = %#v", item.Filename)
	}
}
