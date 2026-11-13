package gemini

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

func fakeGeminiPNG(w, h uint32, pad int) string {
	buf := make([]byte, 33+pad)
	copy(buf, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(buf[8:12], 13)
	copy(buf[12:16], "IHDR")
	binary.BigEndian.PutUint32(buf[16:20], w)
	binary.BigEndian.PutUint32(buf[20:24], h)
	buf[24] = 8
	buf[25] = 6
	return base64.StdEncoding.EncodeToString(buf)
}

func geminiImageResponse(parts []*model.GeminiPart) *model.GeminiGenerateContentResponse {
	return &model.GeminiGenerateContentResponse{
		Candidates: []*model.GeminiCandidate{{
			Index:   0,
			Content: &model.GeminiContent{Role: "model", Parts: parts},
		}},
	}
}

func imageURLsOf(t *testing.T, resp *model.InternalLLMResponse) []string {
	t.Helper()
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		t.Fatal("response has no message")
	}
	var urls []string
	for _, part := range resp.Choices[0].Message.Content.MultipleContent {
		if part.Type == "image_url" && part.ImageURL != nil {
			urls = append(urls, part.ImageURL.URL)
		}
	}
	return urls
}

// 实测结构：thinking 文本 / thought 图(1K) / thinking 文本 / 成品图(4K)。
// chat 形状只能拿到成品；thought 图是草稿，混进去客户端就会"同时收到两张、其中一张 1K"。
func TestConvertGeminiResponseDropsThoughtImage(t *testing.T) {
	// 故意让 thought 图面积更大、编码更长，确认判据是 thought 语义而不是尺寸
	thoughtImage := fakeGeminiPNG(8192, 8192, 8192)
	finalImage := fakeGeminiPNG(5504, 3072, 16)

	resp := convertGeminiToLLMResponse(geminiImageResponse([]*model.GeminiPart{
		{Text: "planning", Thought: true},
		{InlineData: &model.GeminiBlob{MimeType: "image/png", Data: thoughtImage}, Thought: true},
		{Text: "rendering", Thought: true},
		{InlineData: &model.GeminiBlob{MimeType: "image/png", Data: finalImage}, ThoughtSignature: "sig"},
	}), false)

	urls := imageURLsOf(t, resp)
	if len(urls) != 1 {
		t.Fatalf("expected exactly one delivered image, got %d", len(urls))
	}
	if !strings.Contains(urls[0], finalImage) {
		t.Fatal("delivered image must be the non-thought final image")
	}
}

// 只有 thought 图时不能变成"没有图"：退回面积最大的一张。
func TestConvertGeminiResponseFallsBackToThoughtImage(t *testing.T) {
	small := fakeGeminiPNG(512, 512, 8)
	big := fakeGeminiPNG(1376, 768, 8)

	resp := convertGeminiToLLMResponse(geminiImageResponse([]*model.GeminiPart{
		{InlineData: &model.GeminiBlob{MimeType: "image/png", Data: small}, Thought: true},
		{InlineData: &model.GeminiBlob{MimeType: "image/png", Data: big}, Thought: true},
	}), false)

	urls := imageURLsOf(t, resp)
	if len(urls) != 1 || !strings.Contains(urls[0], big) {
		t.Fatalf("expected the largest thought image as fallback, got %d image(s)", len(urls))
	}
}

// 普通（非 thought）图必须照旧交付，不能因为过滤把正常图片吃掉。
func TestConvertGeminiResponseKeepsNormalImage(t *testing.T) {
	image := fakeGeminiPNG(2048, 2048, 32)
	resp := convertGeminiToLLMResponse(geminiImageResponse([]*model.GeminiPart{
		{InlineData: &model.GeminiBlob{MimeType: "image/png", Data: image}},
	}), false)

	urls := imageURLsOf(t, resp)
	if len(urls) != 1 || !strings.Contains(urls[0], image) {
		t.Fatalf("expected the plain image to be delivered, got %d image(s)", len(urls))
	}
}

// 客户端把 aspect_ratio 写错时忽略该字段，不要原样发给上游换 400。
func TestGeminiValidAspectRatio(t *testing.T) {
	valid := map[string]string{"16:9": "16:9", " 1:1 ": "1:1", "21:9": "21:9"}
	for raw, want := range valid {
		if got := geminiValidAspectRatio(raw); got != want {
			t.Fatalf("geminiValidAspectRatio(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "16x9", "16:", ":9", "a:b", "16:9:1"} {
		if got := geminiValidAspectRatio(raw); got != "" {
			t.Fatalf("geminiValidAspectRatio(%q) = %q, want empty", raw, got)
		}
	}
}
