package anthropic

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestConvertDocumentBlockToPartBase64 verifies an Anthropic base64 document
// source is mapped to the internal "file" part as a data URL preserving media
// type and data.
func TestConvertDocumentBlockToPartBase64(t *testing.T) {
	src := &ImageSource{
		Type:      "base64",
		MediaType: "application/pdf",
		Data:      "JVBERi0xLjQK",
	}
	part := convertDocumentBlockToPart(src, nil)
	if part == nil {
		t.Fatal("expected non-nil part for base64 document")
	}
	if part.Type != "file" {
		t.Fatalf("part.Type = %q, want file", part.Type)
	}
	if part.File == nil {
		t.Fatal("expected part.File to be set")
	}
	if part.File.MediaType != "application/pdf" {
		t.Fatalf("media type = %q, want application/pdf", part.File.MediaType)
	}
	if want := "data:application/pdf;base64,JVBERi0xLjQK"; part.File.FileData != want {
		t.Fatalf("file_data = %q, want %q", part.File.FileData, want)
	}
}

// TestConvertDocumentBlockToPartBase64NoMediaType verifies a base64 document
// source missing media_type defaults to application/pdf instead of emitting an
// invalid empty media type upstream.
func TestConvertDocumentBlockToPartBase64NoMediaType(t *testing.T) {
	src := &ImageSource{Type: "base64", Data: "JVBERi0xLjQK"}
	part := convertDocumentBlockToPart(src, nil)
	if part == nil || part.File == nil {
		t.Fatal("expected non-nil file part")
	}
	if part.File.MediaType != "application/pdf" {
		t.Fatalf("media type = %q, want application/pdf default", part.File.MediaType)
	}
	if want := "data:application/pdf;base64,JVBERi0xLjQK"; part.File.FileData != want {
		t.Fatalf("file_data = %q, want %q", part.File.FileData, want)
	}
}

// TestConvertDocumentBlockToPartURL verifies a url document source preserves the
// remote URL and media type.
func TestConvertDocumentBlockToPartURL(t *testing.T) {
	src := &ImageSource{
		Type:      "url",
		MediaType: "application/pdf",
		URL:       "https://example.com/doc.pdf",
	}
	part := convertDocumentBlockToPart(src, nil)
	if part == nil || part.File == nil {
		t.Fatal("expected non-nil file part for url document")
	}
	if part.File.FileURL != "https://example.com/doc.pdf" {
		t.Fatalf("file_url = %q", part.File.FileURL)
	}
	if part.File.FileData != "" {
		t.Fatalf("file_data should be empty for url source, got %q", part.File.FileData)
	}
}

// TestConvertDocumentBlockToPartFileID verifies a file id source preserves the id.
func TestConvertDocumentBlockToPartFileID(t *testing.T) {
	src := &ImageSource{
		Type:   "file",
		FileID: "file_abc123",
	}
	part := convertDocumentBlockToPart(src, nil)
	if part == nil || part.File == nil {
		t.Fatal("expected non-nil file part for file_id document")
	}
	if part.File.FileID != "file_abc123" {
		t.Fatalf("file_id = %q", part.File.FileID)
	}
}

// TestConvertDocumentBlockToPartEmpty verifies empty/unusable sources are dropped.
func TestConvertDocumentBlockToPartEmpty(t *testing.T) {
	if convertDocumentBlockToPart(nil, nil) != nil {
		t.Fatal("nil source should produce nil part")
	}
	if convertDocumentBlockToPart(&ImageSource{Type: "base64"}, nil) != nil {
		t.Fatal("base64 source without data should produce nil part")
	}
}

// TestConvertToolResultContentDocument verifies a document inside a tool_result
// is carried through to an internal "file" part.
func TestConvertToolResultContentDocument(t *testing.T) {
	content := &MessageContent{
		MultipleContent: []MessageContentBlock{
			{
				Type: "document",
				Source: &ImageSource{
					Type:      "base64",
					MediaType: "application/pdf",
					Data:      "AAAA",
				},
			},
		},
	}
	result := convertToolResultContent(content, "claude-sonnet-4-5", nil)
	if len(result.MultipleContent) != 1 {
		t.Fatalf("expected 1 part, got %d", len(result.MultipleContent))
	}
	part := result.MultipleContent[0]
	if part.Type != "file" || part.File == nil {
		t.Fatalf("expected file part, got type=%q file=%v", part.Type, part.File)
	}
	if part.File.FileData != "data:application/pdf;base64,AAAA" {
		t.Fatalf("unexpected file_data %q", part.File.FileData)
	}
}

// sanity: ensure model.File extension compiles with all carrier fields.
var _ = model.File{MediaType: "", FileURL: "", FileID: ""}
