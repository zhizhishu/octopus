package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestImagesHandlerGeminiEditMultipartResolutionAndImagePreservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	type seenBody struct {
		Path string
		Body map[string]any
	}
	var seen seenBody
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Gemini edit body: %v", err)
		}
		seen = seenBody{Path: r.URL.Path, Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"ZWRpdGVkLWltYWdl"}}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-edit-request", "gemini-3.1-flash-image", outbound.OutboundTypeGemini)

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x00}
	for _, imageSize := range []string{"2K", "4K"} {
		t.Run(imageSize, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			_ = writer.WriteField("model", "gemini-edit-request")
			_ = writer.WriteField("prompt", "change only the circle color")
			_ = writer.WriteField("image_config", `{"image_size":"`+imageSize+`","aspect_ratio":"16:9"}`)
			part, err := writer.CreateFormFile("image", "reference.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(png); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			ImagesHandler("/images/edits", c)
			if rec.Code != http.StatusOK {
				t.Fatalf("image edit returned %d %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ZWRpdGVkLWltYWdl") {
				t.Fatalf("OpenAI-style edited image response missing: %s", rec.Body.String())
			}
			if seen.Path != "/v1beta/models/gemini-3.1-flash-image:generateContent" {
				t.Fatalf("wrong Gemini image edit path %q", seen.Path)
			}
			contents, ok := seen.Body["contents"].([]any)
			if !ok || len(contents) != 1 {
				t.Fatalf("missing Gemini content: %#v", seen.Body)
			}
			parts, ok := contents[0].(map[string]any)["parts"].([]any)
			if !ok || len(parts) != 2 {
				t.Fatalf("expected image and text parts, got %#v", contents)
			}
			inline := parts[0].(map[string]any)["inlineData"].(map[string]any)
			if inline["mimeType"] != "image/png" || inline["data"] != base64.StdEncoding.EncodeToString(png) {
				t.Fatalf("source image changed: %#v", inline)
			}
			if parts[1].(map[string]any)["text"] != "change only the circle color" {
				t.Fatalf("edit prompt not preserved: %#v", parts[1])
			}
			cfg := seen.Body["generationConfig"].(map[string]any)["imageConfig"].(map[string]any)
			if cfg["imageSize"] != imageSize || cfg["aspectRatio"] != "16:9" {
				t.Fatalf("Gemini %s imageConfig lost: %#v", imageSize, cfg)
			}
		})
	}
}

func TestGeminiEditJSONDataURLAndMasks(t *testing.T) {
	payload := map[string]any{
		"prompt": "make the square green",
		"images": []any{map[string]any{"image_url": "data:image/png;base64,aW1hZ2U="}},
	}
	gotPayload, parts, err := geminiImageEditInputs(false, nil, "", payload)
	if err != nil || gotPayload["prompt"] != payload["prompt"] {
		t.Fatalf("image JSON payload parse: err=%v", err)
	}
	if len(parts) != 1 || parts[0].InlineData == nil || parts[0].InlineData.Data != "aW1hZ2U=" {
		t.Fatalf("source data URL was not preserved: %#v", parts)
	}
	payload["mask"] = "data:image/png;base64,bWFzaw=="
	if _, _, err := geminiImageEditInputs(false, nil, "", payload); err == nil {
		t.Fatal("Gemini mask must fail explicitly, not silently discard the mask")
	}
}
