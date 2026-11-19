package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func imageInputTestData() (string, []byte) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), png
}

func imageInputTestRequest(t *testing.T, path string, payload any) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return rec, c
}

func TestChatImageReferenceBridgesToOpenAIImagesEditsMultipart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	imageURL, png := imageInputTestData()
	var seenPath, seenType, seenPrompt, seenModel, seenN string
	var seenImage []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenType = r.URL.Path, r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("multipart edit parse: %v", err)
		} else {
			seenPrompt, seenModel, seenN = r.FormValue("prompt"), r.FormValue("model"), r.FormValue("n")
			file, _, err := r.FormFile("image")
			if err != nil {
				t.Errorf("image form file missing: %v", err)
			} else {
				seenImage, _ = io.ReadAll(file)
				_ = file.Close()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"created\":123,\"data\":[{\"b64_json\":\"ZWRpdGVkLWltYWdl\"}]}"))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "image-edit-chat", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec, c := imageInputTestRequest(t, "/v1/chat/completions", map[string]any{
		"model": "image-edit-chat",
		"n":     2,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "change blue to green"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
			},
		}},
	})
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat edit failed %d %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/v1/images/edits" || !strings.HasPrefix(seenType, "multipart/form-data;") {
		t.Fatalf("expected OpenAI edits multipart; got %q, %q", seenPath, seenType)
	}
	if seenModel != "gpt-image-2" || seenPrompt != "change blue to green" || seenN != "2" || !bytes.Equal(seenImage, png) {
		t.Fatalf("edits input not preserved: model=%q prompt=%q n=%q image=%v", seenModel, seenPrompt, seenN, seenImage)
	}
	if !strings.Contains(rec.Body.String(), "![image](data:image/png;base64,ZWRpdGVkLWltYWdl)") {
		t.Fatalf("Chat output must stay Markdown string, got %s", rec.Body.String())
	}
}

func TestResponsesImageReferenceBridgesToGeminiNativeEdit4K(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	imageURL, png := imageInputTestData()
	var path string
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("Gemini body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"Z2VtaW5pLWVkaXRlZA==\"}}]},\"finishReason\":\"STOP\"}]}"))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-edit-response", "gemini-3.1-flash-image", outbound.OutboundTypeGemini)

	rec, c := imageInputTestRequest(t, "/v1/responses", map[string]any{
		"model":        "gemini-edit-response",
		"image_config": map[string]any{"image_size": "4K", "aspect_ratio": "16:9"},
		"tools":        []any{map[string]any{"type": "image_generation"}},
		"input": []any{map[string]any{
			"role": "user",
			"type": "message",
			"content": []any{
				map[string]any{"type": "input_text", "text": "turn the circle green"},
				map[string]any{"type": "input_image", "image_url": imageURL},
			},
		}},
	})
	Handler(inbound.InboundTypeOpenAIResponse, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("Responses Gemini image edit failed %d %s", rec.Code, rec.Body.String())
	}
	if path != "/v1beta/models/gemini-3.1-flash-image:generateContent" {
		t.Fatalf("image edit must use native generateContent, got %q", path)
	}
	contents, ok := received["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("missing Gemini contents: %#v", received)
	}
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("Gemini image and text parts expected, got %#v", parts)
	}
	blob := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if blob["mimeType"] != "image/png" || blob["data"] != base64.StdEncoding.EncodeToString(png) ||
		parts[1].(map[string]any)["text"] != "turn the circle green" {
		t.Fatalf("Gemini edit lost source image or prompt: %#v", parts)
	}
	config := received["generationConfig"].(map[string]any)
	imageConfig := config["imageConfig"].(map[string]any)
	if imageConfig["imageSize"] != "4K" || imageConfig["aspectRatio"] != "16:9" {
		t.Fatalf("Responses 4K image_config lost: %#v", imageConfig)
	}
	if !strings.Contains(rec.Body.String(), "\"type\":\"image_generation_call\"") ||
		!strings.Contains(rec.Body.String(), "\"result\":\"Z2VtaW5pLWVkaXRlZA==\"") {
		t.Fatalf("Responses output shape changed: %s", rec.Body.String())
	}
}

func TestChatGeminiImageReferenceStreamPreservesSSEAnd2K(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	imageURL, _ := imageInputTestData()
	var gotSize, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			GenerationConfig struct {
				ImageConfig struct {
					ImageSize string `json:"imageSize"`
				} `json:"imageConfig"`
			} `json:"generationConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotSize = body.GenerationConfig.ImageConfig.ImageSize
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"aW1hZ2U=\"}}]},\"finishReason\":\"STOP\"}]}"))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-image-stream-edit", "gemini-3.1-flash-image", outbound.OutboundTypeGemini)
	rec, c := imageInputTestRequest(t, "/v1/chat/completions", map[string]any{
		"model":        "gemini-image-stream-edit",
		"stream":       true,
		"image_config": map[string]any{"image_size": "2K"},
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "edit this image"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
			},
		}},
	})
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream failed: code %d headers %#v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-3.1-flash-image:generateContent" || gotSize != "2K" {
		t.Fatalf("Gemini 2K streaming edit lost: %q %q", gotPath, gotSize)
	}
	if !strings.Contains(rec.Body.String(), "chat.completion.chunk") ||
		!strings.Contains(rec.Body.String(), "data: [DONE]") ||
		!strings.Contains(rec.Body.String(), "![image](data:image/png;base64,aW1hZ2U=)") {
		t.Fatalf("Chat image SSE shape broken: %s", rec.Body.String())
	}
}

func TestResponsesMultiImageInputUsesEditFilesAndSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	url1, png1 := imageInputTestData()
	url2 := "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte("webp-reference"))
	var seenFiles int
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("multipart: %v", err)
		} else {
			images := r.MultipartForm.File["image[]"]
			seenFiles = len(images)
			if len(images) == 2 {
				f, err := images[0].Open()
				if err == nil {
					got, _ := io.ReadAll(f)
					_ = f.Close()
					if !bytes.Equal(got, png1) {
						t.Errorf("first input image byte mismatch")
					}
				}
				if images[1].Header.Get("Content-Type") != "image/webp" {
					t.Errorf("second image MIME lost: %#v", images[1].Header)
				}
			}
			if r.FormValue("model") != "gpt-image-2" || r.FormValue("prompt") != "combine these" {
				t.Errorf("model/prompt mismatch: %q %q", r.FormValue("model"), r.FormValue("prompt"))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"data\":[{\"b64_json\":\"bXVsdGktaW1hZ2U=\"}]}"))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "multi-image-response", "gpt-image-2", outbound.OutboundTypeOpenAIResponse)
	rec, c := imageInputTestRequest(t, "/v1/responses", map[string]any{
		"model":  "multi-image-response",
		"stream": true,
		"tools":  []any{map[string]any{"type": "image_generation"}},
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "combine these"},
				map[string]any{"type": "input_image", "image_url": url1},
				map[string]any{"type": "input_image", "image_url": url2},
			},
		}},
	})
	Handler(inbound.InboundTypeOpenAIResponse, c)
	if rec.Code != http.StatusOK || seenPath != "/v1/images/edits" || seenFiles != 2 {
		t.Fatalf("expected 2-image Responses edits, code=%d path=%q count=%d body=%s", rec.Code, seenPath, seenFiles, rec.Body.String())
	}
	for _, must := range []string{"response.output_item.added", "image_generation_call", "bXVsdGktaW1hZ2U=", "response.completed", "data: [DONE]"} {
		if !strings.Contains(rec.Body.String(), must) {
			t.Fatalf("Responses SSE lost %s: %s", must, rec.Body.String())
		}
	}
}

func TestImageBridgeInvalidSourceRejectsBeforeSending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = fmt.Fprint(w, "{}")
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "bad-image-reference", "gpt-image-2", outbound.OutboundTypeOpenAIChat)
	rec, c := imageInputTestRequest(t, "/v1/chat/completions", map[string]any{
		"model": "bad-image-reference",
		"messages": []any{map[string]any{
			"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "edit"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,NOT-BASE64"}},
			},
		}},
	})
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code == http.StatusOK || attempts != 0 {
		t.Fatalf("invalid image must not be silently turned into text generation: code=%d upstream=%d", rec.Code, attempts)
	}
}
