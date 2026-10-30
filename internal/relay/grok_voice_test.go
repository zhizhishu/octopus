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
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
)

// 这组用例走真实 RawProtocolHandler + httptest 假上游，守 Grok 语音两条端点的
// 出站正文形态、二进制透传、失败转移与按次计费调用点。选项值与
// internal/server/handlers/raw_protocol.go 的 grokTTS / grokSTT 保持一致。
func grokTTSOptions() RawProtocolOptions {
	return RawProtocolOptions{
		Endpoint:        "/tts",
		Name:            "audio_tts",
		DefaultModel:    "grok-tts",
		BinaryResponse:  true,
		ResponseLogNote: "audio response omitted for storage",
		OmitAbsentModel: true,
		PerCallBilling:  true,
	}
}

func grokSTTOptions() RawProtocolOptions {
	return RawProtocolOptions{
		Endpoint:        "/stt",
		Name:            "audio_stt",
		DefaultModel:    "grok-voice-transcribe-2.0",
		OmitAbsentModel: true,
		PerCallBilling:  true,
	}
}

func newGrokVoiceContext(method, path string, body io.Reader, contentType string, userID int) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", userID)
	c.Set("request_ip", "127.0.0.1")
	return c, rec
}

func TestGrokTTSOmitsAbsentModelAndPassesAudioThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	audio := []byte{0xFF, 0xFB, 0x90, 0x00, 'I', 'D', '3', 0x00, 0x01}
	var (
		mu      sync.Mutex
		gotPath string
		gotAuth string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "grok-tts", "grok-tts", "xai-tts-key")

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts",
		strings.NewReader(`{"text":"Hello [pause] world","voice_id":"eve","language":"en","output_format":{"codec":"mp3"}}`), "application/json", 0)
	RawProtocolHandler(grokTTSOptions(), c)

	if rec.Code != http.StatusOK {
		t.Fatalf("tts status %d body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), audio) {
		t.Fatalf("audio bytes not passed through verbatim: %x", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content-type %q, want audio/mpeg", ct)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/tts" {
		t.Fatalf("upstream path %q, want /v1/tts", gotPath)
	}
	if gotAuth != "Bearer xai-tts-key" {
		t.Fatalf("upstream auth %q", gotAuth)
	}
	if _, has := gotBody["model"]; has {
		t.Fatalf("上游正文被补了 model 字段（xAI TTS 协议里没有它）: %v", gotBody)
	}
	if gotBody["text"] != "Hello [pause] world" || gotBody["voice_id"] != "eve" || gotBody["language"] != "en" {
		t.Fatalf("tts body fields not preserved: %v", gotBody)
	}
	if of, _ := gotBody["output_format"].(map[string]any); of["codec"] != "mp3" {
		t.Fatalf("nested output_format not preserved: %v", gotBody)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("relay log missing: %v", err)
	}
	entry := logs[0]
	if entry.RequestEndpoint != "audio_tts" || entry.RequestPath != "/v1/tts" || entry.RequestModelName != "grok-tts" {
		t.Fatalf("unexpected log: endpoint=%q path=%q model=%q", entry.RequestEndpoint, entry.RequestPath, entry.RequestModelName)
	}
	detail, err := op.RelayLogGetByID(ctx, entry.ID, nil)
	if err != nil {
		t.Fatalf("get relay log detail: %v", err)
	}
	if strings.Contains(detail.ResponseContent, "ID3") || !strings.Contains(detail.ResponseContent, "audio response omitted") {
		t.Fatalf("stored tts response must be the omission note, got %s", detail.ResponseContent)
	}
	if !strings.Contains(detail.RequestContent, "Hello [pause] world") {
		t.Fatalf("tts request text should be in the admin log, got %s", detail.RequestContent)
	}
}

func TestGrokTTSClientModelIsStillMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, _ := body["model"].(string)
		gotModel.Store(m)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF"))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "tts-alias", "grok-voice-tts-1.0", "k")

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts",
		strings.NewReader(`{"model":"tts-alias","text":"hi","language":"auto"}`), "application/json", 0)
	RawProtocolHandler(grokTTSOptions(), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := gotModel.Load().(string); got != "grok-voice-tts-1.0" {
		t.Fatalf("client-supplied model must still go through model_mapping, upstream saw %q", got)
	}
}

// 守「零值 = 改动前行为」：没开 OmitAbsentModel 的存量端点，正文缺 model 时
// 依旧会把 DefaultModel 写进上游正文（/v1/moderations 一直如此）。
func TestRawProtocolDefaultStillInjectsAbsentModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, _ := body["model"].(string)
		gotModel.Store(m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "omni-moderation-latest", "omni-moderation-latest", "k")

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"hi"}`), "application/json", 0)
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/moderations", Name: "moderations", DefaultModel: "omni-moderation-latest"}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := gotModel.Load().(string); got != "omni-moderation-latest" {
		t.Fatalf("existing endpoint behaviour changed: upstream model %q", got)
	}
}

func TestGrokTTSUpstreamErrorFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	// 429 → 同渠道换 key；500 → 换渠道。两种都必须最终成功、且不把失败体当音频回。
	var badCalls, goodCalls atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer limited-key" {
			badCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("mp3"))
	}))
	t.Cleanup(limited.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	t.Cleanup(broken.Close)
	createRawProtocolChannelWithModel(t, ctx, "grok-voice-broken", broken.URL, []string{"broken-key"}, "grok-tts", nil)
	createRawProtocolChannelWithModel(t, ctx, "grok-voice-limited", limited.URL, []string{"limited-key", "good-key"}, "grok-tts", nil)

	for i := 0; i < 3; i++ {
		c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"text":"hi","language":"en"}`), "application/json", 0)
		RawProtocolHandler(grokTTSOptions(), c)
		if rec.Code != http.StatusOK || rec.Body.String() != "mp3" {
			t.Fatalf("round %d: expected failover success, got %d %q", i, rec.Code, rec.Body.String())
		}
	}
	if goodCalls.Load() != 3 {
		t.Fatalf("good key calls=%d, want 3", goodCalls.Load())
	}
	if badCalls.Load() == 0 {
		t.Fatalf("test did not exercise any failing attempt")
	}
}

func TestGrokTTSUpstreamClientErrorIsSurfaced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"language is required","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "grok-tts", "grok-tts", "k")

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"text":"hi"}`), "application/json", 0)
	RawProtocolHandler(grokTTSOptions(), c)
	if rec.Code < 400 {
		t.Fatalf("upstream 400 must not be reported as success, got %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "audio/") {
		t.Fatalf("error response must not masquerade as audio: %q", ct)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("relay log missing: %v", err)
	}
	if logs[0].Error == "" {
		t.Fatalf("failed tts must be logged with an error")
	}
}

func TestGrokTTSRejectsMissingDefaultChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"text":"hi","language":"en"}`), "application/json", 0)
	RawProtocolHandler(grokTTSOptions(), c)
	if rec.Code < 400 {
		t.Fatalf("no grok-tts channel configured must fail, got %d", rec.Code)
	}
}

func TestGrokSTTMultipartKeepsFileLastAndAddsNoModelPart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	var (
		mu        sync.Mutex
		gotPath   string
		partNames []string
		fileBytes []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("upstream content-type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		mu.Lock()
		gotPath = r.URL.Path
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			partNames = append(partNames, p.FormName())
			b, _ := io.ReadAll(p)
			if p.FormName() == "file" {
				fileBytes = b
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world","language":"en","duration":1.25}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "grok-voice-transcribe-2.0", "grok-voice-transcribe-2.0", "xai-stt-key")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("language", "en")
	_ = mw.WriteField("diarize", "true")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("RIFF....WAVEfmt "))
	_ = mw.Close()

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/stt", &buf, mw.FormDataContentType(), 0)
	RawProtocolHandler(grokSTTOptions(), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("stt status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"hello world"`) {
		t.Fatalf("stt json not passed through: %s", rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/stt" {
		t.Fatalf("upstream path %q", gotPath)
	}
	want := []string{"language", "diarize", "file"}
	if strings.Join(partNames, ",") != strings.Join(want, ",") {
		t.Fatalf("multipart parts %v, want %v（xAI 要求 file 是最后一段，且不许凭空加 model 段）", partNames, want)
	}
	if string(fileBytes) != "RIFF....WAVEfmt " {
		t.Fatalf("file bytes changed: %q", fileBytes)
	}
}

// 走真实 handler → 上游 → metrics.Save → op.UserRecordUsage 全链路：删掉
// raw_protocol.go 里 PerCallBilling 的调用点，这条就红。
func TestGrokTTSPerCallBillingChargesUserBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("mp3"))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "grok-tts", "grok-tts", "k")

	const perCall = 0.05
	price.SetLLMPrice("grok-tts", dbmodel.LLMPrice{PerCall: perCall})
	t.Cleanup(func() { price.SetLLMPrice("grok-tts", dbmodel.LLMPrice{}) })

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "grok-tts-billing-user",
		Password: "test-password-123",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  1,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"text":"hi","language":"en"}`), "application/json", user.ID)
	RawProtocolHandler(grokTTSOptions(), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	after, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if charged := user.Balance - after.Balance; charged < perCall-1e-9 || charged > perCall+1e-9 {
		t.Fatalf("charged %v, want per_call %v", charged, perCall)
	}

	// 失败的调用不计费。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	t.Cleanup(failing.Close)
	price.SetLLMPrice("grok-tts-fail", dbmodel.LLMPrice{PerCall: perCall})
	t.Cleanup(func() { price.SetLLMPrice("grok-tts-fail", dbmodel.LLMPrice{}) })
	createRawProtocolGroup(t, ctx, failing.URL, "grok-tts-fail", "grok-tts-fail", "k2")
	c2, rec2 := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"model":"grok-tts-fail","text":"hi","language":"en"}`), "application/json", user.ID)
	RawProtocolHandler(grokTTSOptions(), c2)
	if rec2.Code < 400 {
		t.Fatalf("expected failure, got %d", rec2.Code)
	}
	again, _ := op.UserGet(user.ID)
	if again.Balance != after.Balance {
		t.Fatalf("failed tts was billed: %v -> %v", after.Balance, again.Balance)
	}
}
