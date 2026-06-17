package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// TestConvertInputFileToPartBase64 verifies a Codex Responses input_file with
// base64 file_data maps to the internal "file" part preserving data + filename.
func TestConvertInputFileToPartBase64(t *testing.T) {
	item := ResponsesItem{
		Type:     "input_file",
		Filename: lo.ToPtr("report.pdf"),
		FileData: lo.ToPtr("data:application/pdf;base64,JVBERi0xLjQK"),
	}
	part := convertInputFileToPart(item)
	if part == nil || part.File == nil {
		t.Fatal("expected non-nil file part")
	}
	if part.Type != "file" {
		t.Fatalf("part.Type = %q, want file", part.Type)
	}
	if part.File.Filename != "report.pdf" {
		t.Fatalf("filename = %q", part.File.Filename)
	}
	if part.File.FileData != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("file_data = %q", part.File.FileData)
	}
}

// TestConvertInputFileToPartURLAndID covers file_url and file_id references.
func TestConvertInputFileToPartURLAndID(t *testing.T) {
	urlPart := convertInputFileToPart(ResponsesItem{
		Type:    "input_file",
		FileURL: lo.ToPtr("https://example.com/a.pdf"),
	})
	if urlPart == nil || urlPart.File == nil || urlPart.File.FileURL != "https://example.com/a.pdf" {
		t.Fatalf("file_url not preserved: %#v", urlPart)
	}

	idPart := convertInputFileToPart(ResponsesItem{
		Type:   "input_file",
		FileID: lo.ToPtr("file_123"),
	})
	if idPart == nil || idPart.File == nil || idPart.File.FileID != "file_123" {
		t.Fatalf("file_id not preserved: %#v", idPart)
	}
}

// TestConvertInputFileToPartEmpty verifies a truly empty item drops, while a
// filename-only item is now preserved (faithful passthrough — no silent loss).
func TestConvertInputFileToPartEmpty(t *testing.T) {
	if convertInputFileToPart(ResponsesItem{Type: "input_file"}) != nil {
		t.Fatal("input_file with no data/url/id/filename should produce nil part")
	}
	part := convertInputFileToPart(ResponsesItem{Type: "input_file", Filename: lo.ToPtr("x.pdf")})
	if part == nil || part.File == nil || part.File.Filename != "x.pdf" {
		t.Fatalf("filename-only input_file should be preserved, got %#v", part)
	}
}

// TestConvertInputToMessageContentMixed verifies input_file coexists with text
// and image content (image passthrough must not regress).
func TestConvertInputToMessageContentMixed(t *testing.T) {
	input := ResponsesInput{
		Items: []ResponsesItem{
			{Type: "input_text", Text: lo.ToPtr("see attached")},
			{Type: "input_image", ImageURL: lo.ToPtr("data:image/png;base64,iVBORw0KGgo=")},
			{Type: "input_file", FileData: lo.ToPtr("data:application/pdf;base64,AAAA")},
		},
	}
	content := convertInputToMessageContent(input)
	if len(content.MultipleContent) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(content.MultipleContent))
	}
	var types []string
	for _, p := range content.MultipleContent {
		types = append(types, p.Type)
	}
	if types[0] != "text" || types[1] != "image_url" || types[2] != "file" {
		t.Fatalf("unexpected part types: %v", types)
	}
}

var _ = model.File{}
