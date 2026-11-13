package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
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
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: key}},
		Model:    upstreamModel,
		Priority: 1,
	}
	if requestModel != upstreamModel {
		channel.ModelMapping = map[string]string{requestModel: upstreamModel}
	}
	if channelType == outbound.OutboundTypeCustomOpenAIChat {
		parsed, err := url.Parse(upstreamURL)
		if err != nil || parsed.Host == "" {
			t.Fatalf("custom images base %q: %v", upstreamURL, err)
		}
		parsed.Path = "/v1/images"
		parsed.RawPath = ""
		parsed.RawQuery = ""
		channel.OpenAIImagesPath = parsed.String()
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channel
}

// TestImagesHandlerGeminiGenerateContentBridgeImageSize 盯住分辨率档位真的进了出站
// 请求体。它补的是一处空白：原先的 generateContent 桥测试只断言 aspectRatio，从没断言
// imageSize，于是"4K 无法表达""image_config.image_size 被丢"这类回归一直无人拦截。
func TestImagesHandlerGeminiGenerateContentBridgeImageSize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "quality 4K is expressible",
			payload: `{"model":"%s","prompt":"draw","quality":"4K"}`,
			want:    "4K",
		},
		{
			name:    "lowercase quality still maps",
			payload: `{"model":"%s","prompt":"draw","quality":"2k"}`,
			want:    "2K",
		},
		{
			name:    "image_config.image_size wins over quality",
			payload: `{"model":"%s","prompt":"draw","quality":"1K","image_config":{"aspect_ratio":"16:9","image_size":"4K"}}`,
			want:    "4K",
		},
		{
			name:    "no size hint sends no imageSize",
			payload: `{"model":"%s","prompt":"draw"}`,
			want:    "",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				gotBody map[string]any
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream body: %v", err)
				}
				mu.Lock()
				gotBody = body
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"candidates":[{
						"index":0,
						"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"c2l6ZS1wcm9iZQ=="}}]},
						"finishReason":"STOP"
					}],
					"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}
				}`))
			}))
			t.Cleanup(upstream.Close)

			requestModel := fmt.Sprintf("gemini-size-%d", i)
			createImageBridgeGroup(t, ctx, upstream.URL, requestModel, "gemini-3-pro-image", outbound.OutboundTypeGemini)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
				strings.NewReader(fmt.Sprintf(tc.payload, requestModel)))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			ImagesHandler("/images/generations", c)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
			}

			mu.Lock()
			body := gotBody
			mu.Unlock()
			cfg, _ := body["generationConfig"].(map[string]any)
			imageCfg, _ := cfg["imageConfig"].(map[string]any)
			gotSize, _ := imageCfg["imageSize"].(string)
			if gotSize != tc.want {
				t.Fatalf("imageConfig.imageSize = %q, want %q (generationConfig: %#v)", gotSize, tc.want, cfg)
			}
			if tc.want == "" && imageCfg != nil {
				if _, exists := imageCfg["imageSize"]; exists {
					t.Fatalf("expected no imageSize field, got %#v", imageCfg)
				}
			}
		})
	}
}

// TestImagesHandlerGeminiGenerateContentBridgeKeepsFinalResolution 盯住"thought 图不能
// 顶替成品"。实测一个 candidate 里有四个 part：thinking 文本、thought 图（1K）、thinking
// 文本、最终图（4K，带 thoughtSignature）。thought 图是思考过程，不是交付物；把它算成
// data[] 的一项，只取 data[0] 的客户端就只拿到那张 1K 的思考图。
func TestImagesHandlerGeminiGenerateContentBridgeKeepsFinalResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	// 故意让 thought 图**面积更大、编码更长**，成品更小：这样"按面积挑""按体积挑"
	// 都会选错，只有按 thought 语义过滤才选对——这条用例钉的是语义，不是启发式。
	thoughtB64 := fakePNGBase64(t, 8192, 8192, 9000)
	finalB64 := fakePNGBase64(t, 5504, 3072, 32)
	if len(thoughtB64) <= len(finalB64) {
		t.Fatalf("test fixture broken: thought image should be the longer one, got %d vs %d", len(thoughtB64), len(finalB64))
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 真实结构（实测）：thinking 文本 / thought 图 / thinking 文本 / 最终图。
		_, _ = w.Write([]byte(`{
			"candidates":[{
				"index":0,
				"content":{"role":"model","parts":[
					{"text":"planning the composition","thought":true},
					{"inlineData":{"mimeType":"image/png","data":"` + thoughtB64 + `"},"thought":true},
					{"text":"now render it at 4K","thought":true},
					{"inlineData":{"mimeType":"image/png","data":"` + finalB64 + `"},"thoughtSignature":"sig_final"}
				]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2000,"totalTokenCount":2004}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-multipart", "gemini-3-pro-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"gemini-multipart",
		"prompt":"draw a 4K apple",
		"quality":"4K"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected bridge to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode client response: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected exactly one image per candidate (preview dropped), got %d: %+v", len(resp.Data), resp.Data)
	}
	if resp.Data[0].B64JSON != finalB64 {
		t.Fatalf("expected the largest part (final resolution) to be returned, got %q", resp.Data[0].B64JSON)
	}
}

// fakePNGBase64 造一张只有头是真的、像素数据当作填充的 PNG：面积由 IHDR 决定，
// 填充长度用来模拟"编码体积"。桥梁只读头，不需要真像素。
func fakePNGBase64(t *testing.T, w, h, pad int) string {
	t.Helper()
	buf := &bytes.Buffer{}
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 25)
	binary.BigEndian.PutUint32(ihdr[0:4], 13)
	copy(ihdr[4:8], "IHDR")
	binary.BigEndian.PutUint32(ihdr[8:12], uint32(w))
	binary.BigEndian.PutUint32(ihdr[12:16], uint32(h))
	ihdr[16] = 8 // bit depth
	ihdr[17] = 6 // color type RGBA
	buf.Write(ihdr)
	buf.Write(make([]byte, pad))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestImagesHandlerGeminiGenerateContentBridgeFallsBackToThoughtImage 守住保底语义：
// 若上游只回了 thought 图（没有最终图），不能让整单变成"上游没有图片数据"，应退回
// 像素面积最大的那张 thought 图，让客户端至少拿到东西。
func TestImagesHandlerGeminiGenerateContentBridgeFallsBackToThoughtImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	small := fakePNGBase64(t, 512, 512, 16)
	big := fakePNGBase64(t, 1376, 768, 16)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates":[{
				"index":0,
				"content":{"role":"model","parts":[
					{"inlineData":{"mimeType":"image/png","data":"` + small + `"},"thought":true},
					{"inlineData":{"mimeType":"image/png","data":"` + big + `"},"thought":true}
				]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1120,"totalTokenCount":1124}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-thought-only", "gemini-3-pro-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"gemini-thought-only",
		"prompt":"draw something",
		"quality":"2K"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode client response: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected exactly one fallback image, got %d", len(resp.Data))
	}
	if resp.Data[0].B64JSON != big {
		t.Fatalf("expected the largest thought image as fallback")
	}
}
