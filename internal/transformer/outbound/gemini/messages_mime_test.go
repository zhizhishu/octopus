package gemini

import (
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func geminiUserPartsRequest(parts []transformerModel.MessageContentPart) *transformerModel.InternalLLMRequest {
	stream := false
	return &transformerModel.InternalLLMRequest{
		Model:  "gemini-pro",
		Stream: &stream,
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				MultipleContent: parts,
			},
		}},
	}
}

// Remote image URLs must reach Gemini fileData with a mimeType inferred from the
// URL extension; an empty mimeType can be rejected upstream.
func TestRemoteImageURLInfersMimeFromExtension(t *testing.T) {
	const remote = "https://example.com/photo.png"
	body := decodeGeminiRequest(t, geminiUserPartsRequest([]transformerModel.MessageContentPart{
		{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: remote}},
	}))
	parts := body.Contents[0].Parts
	if len(parts) != 1 || parts[0].FileData == nil {
		t.Fatalf("expected one fileData part, got %#v", parts)
	}
	if parts[0].FileData.FileURI != remote {
		t.Fatalf("unexpected fileUri %q", parts[0].FileData.FileURI)
	}
	if parts[0].FileData.MimeType != "image/png" {
		t.Fatalf("expected image/png, got %q", parts[0].FileData.MimeType)
	}
}

// Query strings before the extension must not defeat inference.
func TestRemoteImageURLWithQueryInfersMime(t *testing.T) {
	const remote = "https://example.com/a/b.jpeg?sig=abc&x=1"
	body := decodeGeminiRequest(t, geminiUserPartsRequest([]transformerModel.MessageContentPart{
		{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: remote}},
	}))
	parts := body.Contents[0].Parts
	if parts[0].FileData == nil || parts[0].FileData.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %#v", parts[0].FileData)
	}
}

// Unknown extensions must leave mimeType empty rather than guessing.
func TestRemoteImageURLUnknownExtensionLeavesMimeEmpty(t *testing.T) {
	const remote = "https://example.com/asset"
	body := decodeGeminiRequest(t, geminiUserPartsRequest([]transformerModel.MessageContentPart{
		{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: remote}},
	}))
	parts := body.Contents[0].Parts
	if parts[0].FileData == nil {
		t.Fatalf("expected fileData part, got %#v", parts)
	}
	if parts[0].FileData.MimeType != "" {
		t.Fatalf("expected empty mimeType for unknown extension, got %q", parts[0].FileData.MimeType)
	}
}

// Remote file URL with no explicit MediaType infers from the URL (.pdf here).
func TestRemoteFileURLInfersMimeFromExtension(t *testing.T) {
	const docURL = "https://example.com/report.pdf"
	body := decodeGeminiRequest(t, geminiUserPartsRequest([]transformerModel.MessageContentPart{
		{Type: "file", File: &transformerModel.File{FileURL: docURL}},
	}))
	parts := body.Contents[0].Parts
	if parts[0].FileData == nil || parts[0].FileData.FileURI != docURL {
		t.Fatalf("expected fileData with doc uri, got %#v", parts[0])
	}
	if parts[0].FileData.MimeType != "application/pdf" {
		t.Fatalf("expected application/pdf, got %q", parts[0].FileData.MimeType)
	}
}

// An explicit File.MediaType wins over URL-extension inference.
func TestRemoteFileURLMediaTypeTakesPrecedence(t *testing.T) {
	// URL extension says png, but the caller declared a different media type.
	const docURL = "https://example.com/data.png"
	body := decodeGeminiRequest(t, geminiUserPartsRequest([]transformerModel.MessageContentPart{
		{Type: "file", File: &transformerModel.File{FileURL: docURL, MediaType: "application/octet-stream"}},
	}))
	parts := body.Contents[0].Parts
	if parts[0].FileData == nil || parts[0].FileData.MimeType != "application/octet-stream" {
		t.Fatalf("expected MediaType to win, got %#v", parts[0].FileData)
	}
}

// Direct unit coverage of the helper's branch table.
func TestMimeFromURL(t *testing.T) {
	cases := map[string]string{
		"https://h/x.png":        "image/png",
		"https://h/x.JPG":        "image/jpeg",
		"https://h/x.jpeg":       "image/jpeg",
		"https://h/x.webp":       "image/webp",
		"https://h/x.gif":        "image/gif",
		"https://h/x.heic":       "image/heic",
		"https://h/x.pdf":        "application/pdf",
		"https://h/x.mp4":        "video/mp4",
		"https://h/x.mp3":        "audio/mpeg",
		"https://h/x.wav":        "audio/wav",
		"https://h/x.unknownext": "",
		"https://h/noext":        "",
		"https://a.b/c":          "", // last dot precedes a slash -> no extension
		"":                       "",
	}
	for in, want := range cases {
		if got := mimeFromURL(in); got != want {
			t.Fatalf("mimeFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
