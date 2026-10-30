package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 「真实出站请求体」在聊天主路径之外的各条独立出站路径上的端到端测试。
// 每条都走真实 handler → httptest 上游 → metrics.Save → 日志落库，断言：
//   - 开关打开：日志里的快照与上游**实际收到的字节**完全一致（JSON），不打码；
//   - 开关关闭：快照为空，且上游收到的东西与打开时一样（快照不改出站）。

const pathsBodySecret = "sk-live-path-secret-0123456789"

// seenRequest 是上游实际收到的请求（只读副本）。
type seenRequest struct {
	body          []byte
	contentLength int64
	contentType   string
	chunked       bool
}

type upstreamSeen struct {
	mu   sync.Mutex
	last seenRequest
}

func (s *upstreamSeen) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = seenRequest{
		body:          b,
		contentLength: r.ContentLength,
		contentType:   r.Header.Get("Content-Type"),
		chunked:       len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked",
	}
}

func (s *upstreamSeen) snapshot() seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func setupBodyLogPathDB(t *testing.T, enabled bool) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	_ = op.RelayLogClear(context.Background(), nil)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })
	v := 0
	if enabled {
		v = 1
	}
	if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, v); err != nil {
		t.Fatalf("set body log: %v", err)
	}
	return ctx
}

func newBodyLogGinContext(method, path string, body io.Reader, contentType string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", contentType)
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return c, rec
}

func assertSnapshotIsSentBytes(t *testing.T, path string, seen seenRequest, logged string) {
	t.Helper()
	if len(seen.body) == 0 {
		t.Fatalf("%s: upstream received no body", path)
	}
	if logged != string(seen.body) {
		t.Fatalf("%s: snapshot must equal the bytes the upstream received\nlogged: %s\nsent:   %s", path, logged, seen.body)
	}
	if !strings.Contains(logged, pathsBodySecret) || strings.Contains(logged, "[redacted]") {
		t.Fatalf("%s: snapshot must be stored verbatim (unredacted), got %s", path, logged)
	}
}

// ---- raw_protocol (JSON) ----

func runRawJSONPath(t *testing.T, enabled bool) (seenRequest, string) {
	ctx := setupBodyLogPathDB(t, enabled)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "body-rerank", "upstream-body-rerank", "raw-key")

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"body-rerank","query":"`+pathsBodySecret+`","documents":["a"]}`), "application/json")
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw request failed: %d %s", rec.Code, rec.Body.String())
	}
	return seen.snapshot(), latestUpstreamRequestBody(t, ctx)
}

func TestRawProtocolJSONSnapshotsUpstreamBody(t *testing.T) {
	seen, logged := runRawJSONPath(t, true)
	assertSnapshotIsSentBytes(t, "raw_protocol", seen, logged)
	if !strings.Contains(logged, `"upstream-body-rerank"`) {
		t.Fatalf("snapshot must carry the mapped upstream model, got %s", logged)
	}
}

func TestRawProtocolJSONNoSnapshotWhenDisabled(t *testing.T) {
	off, logged := runRawJSONPath(t, false)
	if logged != "" {
		t.Fatalf("disabled toggle must not store a body, got %s", logged)
	}
	on, _ := runRawJSONPath(t, true)
	if !bytes.Equal(off.body, on.body) || off.contentLength != on.contentLength || off.chunked != on.chunked {
		t.Fatalf("snapshot must not change the outbound request\noff=%s cl=%d\non =%s cl=%d", off.body, off.contentLength, on.body, on.contentLength)
	}
}

// ---- raw_protocol (multipart audio) ----

func runRawMultipartAudio(t *testing.T, enabled bool, audio []byte) (seenRequest, string) {
	ctx := setupBodyLogPathDB(t, enabled)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "body-transcribe", "upstream-body-transcribe", "raw-mp-key")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "body-transcribe")
	_ = mw.WriteField("prompt", pathsBodySecret)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="voice.mp3"`}
	h["Content-Type"] = []string{"audio/mpeg"}
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(audio)
	_ = mw.Close()

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/audio/transcriptions", &body, mw.FormDataContentType())
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/audio/transcriptions", Name: "audio_transcriptions"}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart raw request failed: %d %s", rec.Code, rec.Body.String())
	}
	return seen.snapshot(), latestUpstreamRequestBody(t, ctx)
}

func TestRawProtocolMultipartAudioSnapshotIsSummaryWithPlaceholder(t *testing.T) {
	audio := append([]byte("ID3AUDIO-CANARY-"), bytes.Repeat([]byte{0xff, 0xfb, 0x90}, 3000)...)
	seen, logged := runRawMultipartAudio(t, true, audio)

	if strings.Contains(logged, "AUDIO-CANARY") {
		t.Fatalf("audio bytes must not be dumped into the log: %.300s", logged)
	}
	var summary multipartLogSummary
	if err := json.Unmarshal([]byte(logged), &summary); err != nil {
		t.Fatalf("multipart snapshot must be a JSON summary: %v\n%s", err, logged)
	}
	// 摘要是从与线上同一 boundary、同一份字节生成的：总长与 boundary 都对得上。
	_, params, _ := mime.ParseMediaType(seen.contentType)
	if !strings.Contains(summary.ContentType, params["boundary"]) || params["boundary"] == "" {
		t.Fatalf("summary boundary %q does not match the sent one %q", summary.ContentType, seen.contentType)
	}
	if summary.SizeBytes != int64(len(seen.body)) {
		t.Fatalf("summary size_bytes = %d, upstream received %d", summary.SizeBytes, len(seen.body))
	}
	values := map[string]string{}
	var fileData string
	for _, p := range summary.Parts {
		if p.Value != nil {
			values[p.Name] = *p.Value
		}
		if p.Name == "file" {
			fileData = string(p.Data)
			if p.Size != int64(len(audio)) || p.Filename != "voice.mp3" {
				t.Fatalf("file part summary wrong: %+v", p)
			}
		}
	}
	if values["model"] != "upstream-body-transcribe" || values["prompt"] != pathsBodySecret {
		t.Fatalf("text fields must be verbatim (model mapped, prompt unredacted): %v", values)
	}
	if !strings.Contains(fileData, "[audio omitted: audio/mpeg, ") {
		t.Fatalf("audio part must be a short placeholder, got %s", fileData)
	}
	// 出站仍是流式 multipart（chunked），快照不许把它变成定长 body。
	if !seen.chunked {
		t.Fatalf("multipart outbound must stay streamed (chunked) with the toggle on")
	}
}

// ---- images (JSON + multipart) ----

func runImagesJSON(t *testing.T, enabled bool) (seenRequest, string) {
	ctx := setupBodyLogPathDB(t, enabled)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://example.com/i.png"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "body-image", "upstream-body-image", "image-key")

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"body-image","prompt":"draw `+pathsBodySecret+`"}`), "application/json")
	ImagesHandler("/images/generations", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("images request failed: %d %s", rec.Code, rec.Body.String())
	}
	return seen.snapshot(), latestUpstreamRequestBody(t, ctx)
}

func TestImagesJSONSnapshotsUpstreamBody(t *testing.T) {
	seen, logged := runImagesJSON(t, true)
	assertSnapshotIsSentBytes(t, "images", seen, logged)
}

func TestImagesJSONNoSnapshotWhenDisabled(t *testing.T) {
	off, logged := runImagesJSON(t, false)
	if logged != "" {
		t.Fatalf("disabled toggle must not store a body, got %s", logged)
	}
	on, _ := runImagesJSON(t, true)
	if !bytes.Equal(off.body, on.body) || off.contentLength != on.contentLength {
		t.Fatalf("snapshot must not change the outbound request\noff=%s\non =%s", off.body, on.body)
	}
}

func TestImagesMultipartEditSavesUploadedImageAsFile(t *testing.T) {
	withLogImages(t)
	ctx := setupBodyLogPathDB(t, true)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://example.com/i.png"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "body-edit", "upstream-body-edit", "image-key")

	img := fakeImage(t, 4096)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "body-edit")
	_ = mw.WriteField("prompt", "make it blue "+pathsBodySecret)
	fw, err := mw.CreateFormFile("image", "in.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(img)
	_ = mw.Close()

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/images/edits", &body, mw.FormDataContentType())
	ImagesHandler("/images/edits", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("images edit failed: %d %s", rec.Code, rec.Body.String())
	}
	logged := latestUpstreamRequestBody(t, ctx)
	if !strings.Contains(logged, "[image: image/png, 4KB, id="+sha(img)+"]") {
		t.Fatalf("uploaded image must be saved as a file with a link placeholder, got %s", logged)
	}
	if strings.Contains(logged, b64(img)[:64]) || strings.Contains(logged, string(img[8:40])) {
		t.Fatalf("image bytes must not be dumped into the log")
	}
	if !strings.Contains(logged, `"upstream-body-edit"`) || !strings.Contains(logged, pathsBodySecret) {
		t.Fatalf("text fields must be verbatim, got %s", logged)
	}
	if countFiles(t) != 1 {
		t.Fatalf("expected exactly one saved image file")
	}
	if got := seen.snapshot(); !bytes.Contains(got.body, img) {
		t.Fatalf("upstream must still receive the full image")
	}
}

// ---- gemini_images_bridge (ImagesHandler → Gemini channel) ----

func TestImagesGeminiBridgeSnapshotsUpstreamBody(t *testing.T) {
	ctx := setupBodyLogPathDB(t, true)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"Z2VtaW5pLWltZw=="}}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "body-gemini-img", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"body-gemini-img","prompt":"paint `+pathsBodySecret+`"}`), "application/json")
	ImagesHandler("/images/generations", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini images bridge failed: %d %s", rec.Code, rec.Body.String())
	}
	logged := latestUpstreamRequestBody(t, ctx)
	assertSnapshotIsSentBytes(t, "gemini_images_bridge", seen.snapshot(), logged)
	if strings.Contains(logged, "image-key") {
		t.Fatalf("the channel key travels in the URL, it must not appear in the body snapshot: %s", logged)
	}
}

// ---- image_generation_bridge (chat → Gemini image) ----

func runChatGeminiImageBridge(t *testing.T, enabled bool) (seenRequest, string) {
	ctx := setupBodyLogPathDB(t, enabled)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"Z2VtaW5pLWltZw=="}}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-image-chat", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemini-image-chat","messages":[{"role":"user","content":"Create `+pathsBodySecret+`"}]}`), "application/json")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat gemini image bridge failed: %d %s", rec.Code, rec.Body.String())
	}
	return seen.snapshot(), latestUpstreamRequestBody(t, ctx)
}

func TestChatGeminiImageBridgeSnapshotsUpstreamBody(t *testing.T) {
	seen, logged := runChatGeminiImageBridge(t, true)
	assertSnapshotIsSentBytes(t, "image_generation_bridge(gemini)", seen, logged)
}

func TestChatGeminiImageBridgeNoSnapshotWhenDisabled(t *testing.T) {
	off, logged := runChatGeminiImageBridge(t, false)
	if logged != "" {
		t.Fatalf("disabled toggle must not store a body, got %s", logged)
	}
	on, _ := runChatGeminiImageBridge(t, true)
	if !bytes.Equal(off.body, on.body) || off.contentLength != on.contentLength {
		t.Fatalf("snapshot must not change the outbound request\noff=%s\non =%s", off.body, on.body)
	}
}

// ---- videos (create) ----

func runVideosCreate(t *testing.T, enabled bool) (seenRequest, string) {
	ctx := setupBodyLogPathDB(t, enabled)
	var seen upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_body_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)
	ch := dbmodel.Channel{
		Name:         "video-body-channel",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		Model:        "upstream-body-video",
		ModelMapping: map[string]string{"body-video": "upstream-body-video"},
		Priority:     1,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "video-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	c, rec := newBodyLogGinContext(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"body-video","prompt":"a sunset `+pathsBodySecret+`"}`), "application/json")
	VideosHandler(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("video create failed: %d %s", rec.Code, rec.Body.String())
	}
	return seen.snapshot(), latestUpstreamRequestBody(t, ctx)
}

func TestVideosCreateSnapshotsUpstreamBody(t *testing.T) {
	seen, logged := runVideosCreate(t, true)
	assertSnapshotIsSentBytes(t, "videos", seen, logged)
	if !strings.Contains(logged, `"upstream-body-video"`) {
		t.Fatalf("snapshot must carry the mapped upstream model, got %s", logged)
	}
}

func TestVideosCreateNoSnapshotWhenDisabled(t *testing.T) {
	off, logged := runVideosCreate(t, false)
	if logged != "" {
		t.Fatalf("disabled toggle must not store a body, got %s", logged)
	}
	on, _ := runVideosCreate(t, true)
	if !bytes.Equal(off.body, on.body) || off.contentLength != on.contentLength {
		t.Fatalf("snapshot must not change the outbound request\noff=%s\non =%s", off.body, on.body)
	}
}

func TestRawProtocolMultipartNoSnapshotWhenDisabled(t *testing.T) {
	audio := append([]byte("ID3AUDIO-CANARY-"), bytes.Repeat([]byte{0xff, 0xfb, 0x90}, 300)...)
	seen, logged := runRawMultipartAudio(t, false, audio)
	if logged != "" {
		t.Fatalf("disabled toggle must not store a multipart summary, got %s", logged)
	}
	if !seen.chunked || !bytes.Contains(seen.body, audio) {
		t.Fatalf("multipart outbound must be unchanged with the toggle off")
	}
}
