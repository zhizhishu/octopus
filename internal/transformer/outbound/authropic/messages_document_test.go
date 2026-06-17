package authropic

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestGetThinkingBudgetMax verifies the "max" effort level maps to a budget
// above "high" (fix A).
func TestGetThinkingBudgetMax(t *testing.T) {
	got := getThinkingBudget("max", nil, 0)
	if got == nil {
		t.Fatal("expected non-nil budget for max effort")
	}
	if *got <= 32768 {
		t.Fatalf("max budget = %d, want > 32768 (high)", *got)
	}
	if *got != 64000 {
		t.Fatalf("max budget = %d, want 64000", *got)
	}
}

// TestGetThinkingBudgetExplicitWins verifies an explicit budget always wins over
// the effort mapping (including max).
func TestGetThinkingBudgetExplicitWins(t *testing.T) {
	explicit := int64(12345)
	got := getThinkingBudget("max", &explicit, 0)
	if got == nil || *got != 12345 {
		t.Fatalf("explicit budget should win, got %v", got)
	}
}

// TestGetThinkingBudgetClampedToMaxTokens pins that the thinking budget never
// reaches/exceeds max_tokens (Anthropic requires budget_tokens < max_tokens),
// so a high/max effort with a small max_tokens does not produce an upstream 400.
func TestGetThinkingBudgetClampedToMaxTokens(t *testing.T) {
	// max effort (64000) with the default 8192 max_tokens -> clamp to 8191.
	if got := getThinkingBudget("max", nil, 8192); got == nil || *got != 8191 {
		t.Fatalf("max budget with maxTokens=8192 = %v, want 8191", got)
	}
	// high effort (32768) likewise clamps.
	if got := getThinkingBudget("high", nil, 8192); got == nil || *got != 8191 {
		t.Fatalf("high budget with maxTokens=8192 = %v, want 8191", got)
	}
	// explicit oversized budget also clamps.
	explicit := int64(99999)
	if got := getThinkingBudget("", &explicit, 8192); got == nil || *got != 8191 {
		t.Fatalf("explicit oversized budget with maxTokens=8192 = %v, want 8191", got)
	}
	// when budget already fits, it is untouched.
	if got := getThinkingBudget("high", nil, 200000); got == nil || *got != 32768 {
		t.Fatalf("high budget with large maxTokens = %v, want 32768", got)
	}
}

// TestConvertFileToDocumentBlockBase64 verifies an internal base64 "file" part is
// rebuilt into an Anthropic document block (fix D outbound).
func TestConvertFileToDocumentBlockBase64(t *testing.T) {
	part := model.MessageContentPart{
		Type: "file",
		File: &model.File{
			Filename: "doc.pdf",
			FileData: "data:application/pdf;base64,JVBERi0xLjQK",
		},
	}
	block := convertFileToDocumentBlock(part)
	if block == nil {
		t.Fatal("expected non-nil document block")
	}
	if block.Type != "document" {
		t.Fatalf("block.Type = %q, want document", block.Type)
	}
	if block.Source == nil || block.Source.Type != "base64" {
		t.Fatalf("expected base64 source, got %#v", block.Source)
	}
	if block.Source.MediaType != "application/pdf" {
		t.Fatalf("media type = %q", block.Source.MediaType)
	}
	if block.Source.Data != "JVBERi0xLjQK" {
		t.Fatalf("data = %q", block.Source.Data)
	}
}

// TestConvertFileToDocumentBlockURLAndID covers url and file_id sources.
func TestConvertFileToDocumentBlockURLAndID(t *testing.T) {
	urlPart := model.MessageContentPart{
		Type: "file",
		File: &model.File{FileURL: "https://example.com/a.pdf", MediaType: "application/pdf"},
	}
	urlBlock := convertFileToDocumentBlock(urlPart)
	if urlBlock == nil || urlBlock.Source == nil || urlBlock.Source.Type != "url" {
		t.Fatalf("expected url source, got %#v", urlBlock)
	}
	if urlBlock.Source.URL != "https://example.com/a.pdf" {
		t.Fatalf("url = %q", urlBlock.Source.URL)
	}

	idPart := model.MessageContentPart{
		Type: "file",
		File: &model.File{FileID: "file_xyz"},
	}
	idBlock := convertFileToDocumentBlock(idPart)
	if idBlock == nil || idBlock.Source == nil || idBlock.Source.Type != "file" {
		t.Fatalf("expected file source, got %#v", idBlock)
	}
	if idBlock.Source.FileID != "file_xyz" {
		t.Fatalf("file_id = %q", idBlock.Source.FileID)
	}
}

// TestConvertFileToDocumentBlockEmpty verifies unusable file parts are dropped.
func TestConvertFileToDocumentBlockEmpty(t *testing.T) {
	if convertFileToDocumentBlock(model.MessageContentPart{Type: "file"}) != nil {
		t.Fatal("nil file should produce nil block")
	}
	if convertFileToDocumentBlock(model.MessageContentPart{Type: "file", File: &model.File{Filename: "x"}}) != nil {
		t.Fatal("file with only filename should produce nil block")
	}
}

// TestDocumentRoundTripBase64 verifies internal file part -> document block keeps
// media type and base64 data intact (the inbound half is covered in the
// anthropic inbound package test; here we assert the rebuild faithfulness).
func TestDocumentRoundTripBase64(t *testing.T) {
	const mediaType = "application/pdf"
	const data = "JVBERi0xLjQKMSAwIG9iag=="

	// internal carrier as produced by the inbound anthropic transformer
	internal := model.MessageContentPart{
		Type: "file",
		File: &model.File{
			MediaType: mediaType,
			FileData:  "data:" + mediaType + ";base64," + data,
		},
	}

	block := convertFileToDocumentBlock(internal)
	if block == nil || block.Source == nil {
		t.Fatal("rebuild produced nil block")
	}
	if block.Source.MediaType != mediaType {
		t.Fatalf("round-trip media type = %q, want %q", block.Source.MediaType, mediaType)
	}
	if block.Source.Data != data {
		t.Fatalf("round-trip data = %q, want %q", block.Source.Data, data)
	}
}

// TestConvertImageURLStillWorks guards against regressing image passthrough.
func TestConvertImageURLStillWorks(t *testing.T) {
	part := model.MessageContentPart{
		Type:     "image_url",
		ImageURL: &model.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="},
	}
	block := convertImageURLToBlock(part)
	if block == nil || block.Type != "image" {
		t.Fatalf("expected image block, got %#v", block)
	}
	if block.Source == nil || block.Source.Type != "base64" || block.Source.MediaType != "image/png" {
		t.Fatalf("image source not preserved: %#v", block.Source)
	}
}
