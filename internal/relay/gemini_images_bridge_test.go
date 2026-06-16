package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestImagesHandlerGeminiImagenPredictBridge(t *testing.T) {
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
			"predictions":[{"mimeType":"image/png","bytesBase64Encoded":"aW1hZ2VuLWltZw=="}]
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL+"/v1beta/models/old:generateContent", "imagen-request", "imagen-4.0-generate-001", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"imagen-request",
		"prompt":"draw an imagen octopus",
		"n":2,
		"size":"1792x1024",
		"quality":"high"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gemini imagen bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Octopus-Image-Bridge") != "gemini-images" {
		t.Fatalf("expected gemini bridge header, got %q", rec.Header().Get("X-Octopus-Image-Bridge"))
	}
	if !strings.Contains(rec.Body.String(), `"b64_json":"aW1hZ2VuLWltZw=="`) {
		t.Fatalf("expected OpenAI images response with b64_json, got %s", rec.Body.String())
	}

	mu.Lock()
	path, key, body := gotPath, gotKey, gotBody
	mu.Unlock()
	if path != "/v1beta/models/imagen-4.0-generate-001:predict" {
		t.Fatalf("expected Imagen predict path, got %q", path)
	}
	if key != "image-key" {
		t.Fatalf("expected Gemini key query, got %q", key)
	}
	params, _ := body["parameters"].(map[string]any)
	if params["sampleCount"] != float64(2) || params["aspectRatio"] != "16:9" || params["imageSize"] != "2K" {
		t.Fatalf("unexpected imagen parameters: %#v", params)
	}
}

func TestImagesHandlerGeminiGenerateContentBridge(t *testing.T) {
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
			"candidates":[{
				"index":0,
				"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"Z2VtaW5pLWRpcmVjdA=="}}]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-direct-image", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"gemini-direct-image",
		"prompt":"draw a direct gemini image",
		"response_format":"url",
		"image_config":{"aspect_ratio":"3:2"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gemini generateContent image bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"url":"data:image/webp;base64,Z2VtaW5pLWRpcmVjdA=="`) {
		t.Fatalf("expected OpenAI images response with data URL, got %s", rec.Body.String())
	}

	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Fatalf("expected Gemini generateContent path, got %q", path)
	}
	cfg, _ := body["generationConfig"].(map[string]any)
	modalities, _ := cfg["responseModalities"].([]any)
	imageCfg, _ := cfg["imageConfig"].(map[string]any)
	if len(modalities) != 2 || modalities[0] != "TEXT" || modalities[1] != "IMAGE" || imageCfg["aspectRatio"] != "3:2" {
		t.Fatalf("unexpected gemini generation config: %#v", cfg)
	}
}

func TestImagesHandlerCustomOpenAIGrokPayloadIsSanitized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"url":"https://example.test/grok.png"}]}`))
	}))
	t.Cleanup(upstream.Close)

	createRawImagesGroupWithType(t, ctx, upstream.URL+"/v1/chat/completions", "grok-image-request", "grok-imagine-image-pro", "grok-key", outbound.OutboundTypeCustomOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"grok-image-request",
		"prompt":"draw grok image",
		"n":1,
		"size":"1024x1024",
		"quality":"high",
		"style":"vivid",
		"user":"client-user",
		"response_format":"url"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected custom OpenAI Grok image request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	for _, forbidden := range []string{"size", "quality", "style", "user"} {
		if _, ok := gotBody[forbidden]; ok {
			t.Fatalf("expected Grok image payload to drop unsupported %q, got %#v", forbidden, gotBody)
		}
	}
	if gotBody["model"] != "grok-imagine-image-pro" || gotBody["prompt"] != "draw grok image" || gotBody["response_format"] != "url" {
		t.Fatalf("unexpected Grok image payload: %#v", gotBody)
	}
}

func createRawImagesGroupWithType(t *testing.T, ctx context.Context, upstreamURL, requestModel, upstreamModel, key string, channelType outbound.OutboundType) dbmodel.Channel {
	t.Helper()
	channel := dbmodel.Channel{
		Name:    requestModel + "-channel",
		Type:    channelType,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstreamURL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: key}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{
		Name: requestModel,
		Mode: dbmodel.GroupModeFailover,
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: upstreamModel,
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}
	return channel
}
