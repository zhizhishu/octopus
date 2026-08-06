package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestResponsesImageGenerationToolBridgesToImagesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu      sync.Mutex
		gotPath string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":123,
			"data":[{"b64_json":"aW1n"}],
			"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12,"input_tokens_details":{"cached_tokens":2}}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "image2", "gpt-image-2", outbound.OutboundTypeOpenAIResponse)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"image2",
		"input":"Draw a tiny octopus holding a wrench",
		"tools":[{
			"type":"image_generation",
			"background":"transparent",
			"input_fidelity":"high",
			"input_image_mask":{"file_id":"mask_123"},
			"moderation":"low",
			"output_compression":80,
			"partial_images":2,
			"quality":"low",
			"size":"1024x1024",
			"output_format":"webp",
			"watermark":true
		}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected responses image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Octopus-Image-Bridge") != "images-generations" {
		t.Fatalf("expected bridge header, got %q", rec.Header().Get("X-Octopus-Image-Bridge"))
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, `"object":"response"`) ||
		!strings.Contains(bodyText, `"type":"image_generation_call"`) ||
		!strings.Contains(bodyText, `"result":"aW1n"`) ||
		!strings.Contains(bodyText, `"input_tokens":5`) ||
		!strings.Contains(bodyText, `"output_tokens":7`) {
		t.Fatalf("expected responses-shaped image result, got %s", bodyText)
	}

	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/images/generations" {
		t.Fatalf("expected canonical images path, got %q", path)
	}
	if body["model"] != "gpt-image-2" || body["prompt"] != "Draw a tiny octopus holding a wrench" {
		t.Fatalf("unexpected upstream image payload: %#v", body)
	}
	if body["quality"] != "low" || body["size"] != "1024x1024" || body["output_format"] != "webp" {
		t.Fatalf("expected tool image params to survive, got %#v", body)
	}
	if body["background"] != "transparent" || body["input_fidelity"] != "high" || body["moderation"] != "low" {
		t.Fatalf("expected extended tool image params to survive, got %#v", body)
	}
	if body["output_compression"] != float64(80) || body["partial_images"] != float64(2) || body["watermark"] != true {
		t.Fatalf("expected numeric/bool tool image params to survive, got %#v", body)
	}
	if mask, ok := body["input_image_mask"].(map[string]any); !ok || mask["file_id"] != "mask_123" {
		t.Fatalf("expected input_image_mask to survive, got %#v", body)
	}
	if body["stream"] != false {
		t.Fatalf("responses/chat image bridge should use non-stream Images upstream for wrapping, got %#v", body["stream"])
	}
}

func TestChatImageModelBridgesToImagesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu      sync.Mutex
		gotPath string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":456,
			"data":[{"b64_json":"Y2hhdC1pbWc="}],
			"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-2", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"Create a pixel art crab"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected chat image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	bodyText := rec.Body.String()
	// A chat client's assistant content is a string: the image is rendered as a Markdown
	// image (like new-api), not an image_url multipart array standard clients can't parse.
	if !strings.Contains(bodyText, `"object":"chat.completion"`) ||
		!strings.Contains(bodyText, `![image](data:image/png;base64,Y2hhdC1pbWc=)`) {
		t.Fatalf("expected chat markdown image result, got %s", bodyText)
	}
	if strings.Contains(bodyText, `"type":"image_url"`) {
		t.Fatalf("chat image result should be a Markdown string, not an image_url array: %s", bodyText)
	}

	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/images/generations" {
		t.Fatalf("expected canonical images path, got %q", path)
	}
	if body["model"] != "gpt-image-2" || body["prompt"] != "Create a pixel art crab" {
		t.Fatalf("unexpected upstream image payload: %#v", body)
	}
}

func TestChatGeminiImageModelRoutesToGeminiGenerateContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu      sync.Mutex
		gotPath string
		gotKey  string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("key")
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"modelVersion":"gemini-2.5-flash-image",
			"candidates":[{
				"index":0,
				"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"Z2VtaW5pLWltZw=="}}]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-image-chat", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-image-chat",
		"messages":[{"role":"user","content":"Create a gemini image"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gemini image chat route to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if bodyText := rec.Body.String(); !strings.Contains(bodyText, `"object":"chat.completion"`) ||
		!strings.Contains(bodyText, `data:image/png;base64,Z2VtaW5pLWltZw==`) {
		t.Fatalf("expected chat-shaped gemini image result, got %s", bodyText)
	}

	mu.Lock()
	path, key, body := gotPath, gotKey, gotBody
	mu.Unlock()
	if path != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Fatalf("expected gemini generateContent path, got %q", path)
	}
	if key != "image-key" {
		t.Fatalf("expected gemini key query, got %q", key)
	}
	generationConfig, _ := body["generationConfig"].(map[string]any)
	modalities, _ := generationConfig["responseModalities"].([]any)
	if len(modalities) != 2 || modalities[0] != "TEXT" || modalities[1] != "IMAGE" {
		t.Fatalf("expected synthesized gemini responseModalities, got %#v in %#v", modalities, body)
	}
}

func TestResponsesGeminiImageToolRoutesToGeminiGenerateContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates":[{
				"index":0,
				"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"cmVzcC1nZW1pbmktaW1n"}}]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-image-response", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gemini-image-response",
		"input":"Create a gemini image through Responses",
		"tools":[{"type":"image_generation"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gemini image responses route to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Fatalf("expected gemini generateContent path, got %q", gotPath)
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, `"object":"response"`) ||
		!strings.Contains(bodyText, `"type":"image_generation_call"`) ||
		!strings.Contains(bodyText, `"result":"cmVzcC1nZW1pbmktaW1n"`) {
		t.Fatalf("expected responses-shaped gemini image result, got %s", bodyText)
	}
}

func TestResponsesGrokImageGenerationBridgeSanitizesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu      sync.Mutex
		gotPath string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"url":"https://example.test/grok-bridge.png"}]}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL+"/v1/chat/completions", "grok-image-responses", "grok-imagine-image-pro", outbound.OutboundTypeCustomOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"grok-image-responses",
		"input":"Draw a sanitized grok image",
		"tools":[{
			"type":"image_generation",
			"background":"transparent",
			"quality":"high",
			"size":"1024x1024",
			"style":"vivid",
			"output_format":"webp",
			"watermark":true
		}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected grok image responses bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/images/generations" {
		t.Fatalf("expected canonical images path, got %q", path)
	}
	for _, forbidden := range []string{"background", "quality", "size", "style", "output_format", "watermark", "stream"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("expected Grok bridge payload to drop unsupported %q, got %#v", forbidden, body)
		}
	}
	if body["model"] != "grok-imagine-image-pro" || body["prompt"] != "Draw a sanitized grok image" {
		t.Fatalf("unexpected Grok bridge payload: %#v", body)
	}
}

// TestChatImageBridgeThreadsN guards that a client's image count survives to the upstream
// /v1/images/generations request instead of always being dropped to 1.
func TestChatImageBridgeThreadsN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	var (
		mu      sync.Mutex
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1n"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-2", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"three crabs"}],
		"n":3
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected chat image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if body["n"] != float64(3) {
		t.Fatalf("expected upstream image payload n=3, got %#v (full %#v)", body["n"], body)
	}
}

// TestResponsesImageBridgeDownloadsURLToBase64 guards that a URL-returning image upstream
// is fetched and re-encoded to base64 for a Responses image_generation_call.result (which
// must be base64 bytes, not a URL).
func TestResponsesImageBridgeDownloadsURLToBase64(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	imgBytes := []byte("PNG-BYTES-abc123")
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgBytes)
	}))
	t.Cleanup(imgServer.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"` + imgServer.URL + `/pic.png"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-2", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-image-2",
		"input":"a fish"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIResponse, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected responses image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	wantB64 := base64.StdEncoding.EncodeToString(imgBytes)
	if !strings.Contains(body, `"type":"image_generation_call"`) || !strings.Contains(body, wantB64) {
		t.Fatalf("expected downloaded base64 result %q in responses body, got %s", wantB64, body)
	}
	if strings.Contains(body, imgServer.URL) {
		t.Fatalf("responses result should carry base64 bytes, not a raw URL: %s", body)
	}
}

func TestChatAndResponsesGeminiImagenRoutesToPredict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		gotPaths []string
		gotKeys  []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		gotKeys = append(gotKeys, r.URL.Query().Get("key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"predictions":[{"mimeType":"image/png","bytesBase64Encoded":"aW1hZ2VuLWJyaWRnZQ=="}]
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL+"/v1beta/models/old:generateContent", "imagen-chat-response", "imagen-4.0-generate-001", outbound.OutboundTypeGemini)

	chatRec := httptest.NewRecorder()
	chatCtx, _ := gin.CreateTestContext(chatRec)
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"imagen-chat-response",
		"messages":[{"role":"user","content":"Create imagen from chat"}]
	}`))
	chatReq.Header.Set("Content-Type", "application/json")
	chatCtx.Request = chatReq
	chatCtx.Set("api_key_id", 0)
	chatCtx.Set("user_id", 0)
	chatCtx.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, chatCtx)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("expected chat imagen bridge to succeed, got %d body %s", chatRec.Code, chatRec.Body.String())
	}
	if bodyText := chatRec.Body.String(); !strings.Contains(bodyText, `"object":"chat.completion"`) ||
		!strings.Contains(bodyText, `data:image/png;base64,aW1hZ2VuLWJyaWRnZQ==`) {
		t.Fatalf("expected chat-shaped imagen result, got %s", bodyText)
	}

	respRec := httptest.NewRecorder()
	respCtx, _ := gin.CreateTestContext(respRec)
	respReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"imagen-chat-response",
		"input":"Create imagen from responses",
		"tools":[{"type":"image_generation"}]
	}`))
	respReq.Header.Set("Content-Type", "application/json")
	respCtx.Request = respReq
	respCtx.Set("api_key_id", 0)
	respCtx.Set("user_id", 0)
	respCtx.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, respCtx)
	if respRec.Code != http.StatusOK {
		t.Fatalf("expected responses imagen bridge to succeed, got %d body %s", respRec.Code, respRec.Body.String())
	}
	if bodyText := respRec.Body.String(); !strings.Contains(bodyText, `"object":"response"`) ||
		!strings.Contains(bodyText, `"type":"image_generation_call"`) ||
		!strings.Contains(bodyText, `"result":"aW1hZ2VuLWJyaWRnZQ=="`) {
		t.Fatalf("expected responses-shaped imagen result, got %s", bodyText)
	}

	mu.Lock()
	paths := append([]string(nil), gotPaths...)
	keys := append([]string(nil), gotKeys...)
	mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("expected two upstream Imagen calls, got paths=%#v", paths)
	}
	for i, path := range paths {
		if path != "/v1beta/models/imagen-4.0-generate-001:predict" {
			t.Fatalf("expected Imagen predict path for call %d, got %q", i, path)
		}
		if keys[i] != "image-key" {
			t.Fatalf("expected Gemini key query for call %d, got %q", i, keys[i])
		}
	}
}

func TestResponsesImageGenerationStreamBridgesToSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":789,
			"data":[{"b64_json":"c3RyZWFtLWltZw=="}],
			"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "image-stream", "gpt-image-2", outbound.OutboundTypeOpenAIResponse)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"image-stream",
		"input":"Draw a streamed octopus",
		"stream":true,
		"tools":[{"type":"image_generation"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected streamed responses image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", rec.Header().Get("Content-Type"))
	}
	bodyText := rec.Body.String()
	for _, want := range []string{
		`response.output_item.added`,
		`response.output_item.done`,
		`image_generation_call`,
		`c3RyZWFtLWltZw==`,
		`response.completed`,
		`data: [DONE]`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected streamed responses output to contain %s, got %s", want, bodyText)
		}
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("expected canonical images path, got %q", gotPath)
	}
}

func TestChatImageModelStreamBridgesToSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":987,
			"data":[{"b64_json":"Y2hhdC1zdHJlYW0taW1n"}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-stream", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-image-stream",
		"stream":true,
		"messages":[{"role":"user","content":"Create a streamed crab"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected streamed chat image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", rec.Header().Get("Content-Type"))
	}
	bodyText := rec.Body.String()
	// Streamed chat image is a Markdown image string in delta.content (like new-api), not
	// an image_url multipart array.
	if !strings.Contains(bodyText, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(bodyText, `![image](data:image/png;base64,Y2hhdC1zdHJlYW0taW1n)`) ||
		!strings.Contains(bodyText, `data: [DONE]`) {
		t.Fatalf("expected chat SSE markdown image chunks, got %s", bodyText)
	}
	if strings.Contains(bodyText, `"type":"image_url"`) {
		t.Fatalf("streamed chat image should be a Markdown string, not an image_url array: %s", bodyText)
	}
}

func TestCustomOpenAIChatImageModelBridgesToImagesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":654,
			"data":[{"b64_json":"Y3VzdG9tLWltZw=="}]
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL+"/v1/chat/completions", "custom-gpt-image", "gpt-image-2", outbound.OutboundTypeCustomOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"custom-gpt-image",
		"messages":[{"role":"user","content":"Create custom endpoint image"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected custom chat image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("expected custom OpenAI chat base to canonicalize to images path, got %q", gotPath)
	}
}

func createImageBridgeGroup(t *testing.T, ctx context.Context, baseURL, requestModel, actualModel string, channelType outbound.OutboundType) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:    "image-bridge-" + requestModel,
		Type:    channelType,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: baseURL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "image-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: actualModel,
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}
}
