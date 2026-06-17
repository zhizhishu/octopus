package gemini

import (
	"context"
	"testing"
)

// Fix 4: non-image fileData parts (e.g. application/pdf) must convert to an
// internal file part instead of being dropped; image fileData stays image_url.
func TestGenerateContentInboundNonImageFileData(t *testing.T) {
	ctx := WithRequestOptions(context.Background(), "gemini-request", false)
	inbound := &GenerateContentInbound{}

	req, err := inbound.TransformRequest(ctx, []byte(`{
		"contents":[
			{"role":"user","parts":[
				{"text":"summarize this"},
				{"fileData":{"mimeType":"application/pdf","fileUri":"https://example.com/report.pdf"}},
				{"fileData":{"mimeType":"image/png","fileUri":"https://example.com/cat.png"}}
			]}
		]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	parts := req.Messages[0].Content.MultipleContent
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts (text + file + image), got %d: %#v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "summarize this" {
		t.Fatalf("unexpected text part: %#v", parts[0])
	}
	if parts[1].Type != "file" || parts[1].File == nil {
		t.Fatalf("expected non-image fileData -> file part, got %#v", parts[1])
	}
	if parts[1].File.FileURL != "https://example.com/report.pdf" || parts[1].File.MediaType != "application/pdf" {
		t.Fatalf("unexpected file part contents: %#v", parts[1].File)
	}
	if parts[2].Type != "image_url" || parts[2].ImageURL == nil || parts[2].ImageURL.URL != "https://example.com/cat.png" {
		t.Fatalf("expected image fileData -> image_url, got %#v", parts[2])
	}
}
