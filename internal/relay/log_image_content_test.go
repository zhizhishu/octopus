package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/logimage"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 「请求内容 / 响应内容」两列的图片另存（2026-10-31）。

func latestRelayLogFull(t *testing.T, ctx context.Context) *dbmodel.RelayLog {
	t.Helper()
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 5, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("list logs: %v (n=%d)", err, len(logs))
	}
	full, err := op.RelayLogGetByID(ctx, logs[0].ID, nil)
	if err != nil {
		t.Fatalf("get log: %v", err)
	}
	return full
}

type capturedWire struct {
	body   []byte
	header http.Header
}

// runVisionChat 发一次带图的 chat 请求；上游回一张图。返回上游收到的字节、客户端收到的响应。
func runVisionChat(t *testing.T, ctx context.Context, model, reqImgB64, respImgB64 string) (capturedWire, string) {
	t.Helper()
	var mu sync.Mutex
	var wire capturedWire
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = capturedWire{body: b, header: r.Header.Clone()}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"here","images":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + respImgB64 + `"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, model, model, outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+model+`","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`+reqImgB64+`"}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay failed: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	return wire, rec.Body.String()
}

func TestLogImageContentColumnsEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	dir := t.TempDir()
	t.Cleanup(logimage.SetDir(dir))
	t.Cleanup(logimage.WaitIdle)
	if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, 1); err != nil {
		t.Fatal(err)
	}

	inImg := fakeImage(t, 6000)
	outImg := fakeImage(t, 7000)
	wire, clientResp := runVisionChat(t, ctx, "vision-content", b64(inImg), b64(outImg))

	if !strings.Contains(string(wire.body), b64(inImg)) {
		t.Fatal("outbound request lost or altered the image bytes")
	}
	if !strings.Contains(clientResp, b64(outImg)) {
		t.Fatalf("client must still receive the generated image in full, got %.300s", clientResp)
	}

	full := latestRelayLogFull(t, ctx)
	for name, col := range map[string]string{
		"request_content":       full.RequestContent,
		"response_content":      full.ResponseContent,
		"upstream_request_body": full.UpstreamRequestBody,
	} {
		if strings.Contains(col, b64(inImg)[:200]) || strings.Contains(col, b64(outImg)[:200]) {
			t.Fatalf("%s still carries base64: %.300s", name, col)
		}
		if strings.Contains(col, "id=[redacted]") {
			t.Fatalf("%s: image id was masked by redaction: %s", name, col)
		}
	}
	if !strings.Contains(full.RequestContent, "id="+sha(inImg)+"]") {
		t.Fatalf("request_content should reference the input image: %s", full.RequestContent)
	}
	if !strings.Contains(full.UpstreamRequestBody, "id="+sha(inImg)+"]") {
		t.Fatalf("upstream body should reference the input image: %s", full.UpstreamRequestBody)
	}
	if !strings.Contains(full.ResponseContent, "id="+sha(outImg)+"]") {
		t.Fatalf("response_content should reference the generated image: %s", full.ResponseContent)
	}
	if !json.Valid([]byte(full.RequestContent)) || !json.Valid([]byte(full.ResponseContent)) {
		t.Fatal("stored columns must stay valid JSON")
	}

	logimage.WaitIdle()
	files, _ := logimage.List()
	got := map[string]bool{}
	for _, f := range files {
		got[f.Hash] = true
	}
	if len(files) != 2 || !got[sha(inImg)] || !got[sha(outImg)] {
		t.Fatalf("expected exactly the two images on disk (deduped), got %+v", files)
	}

	if err := op.RelayLogSaveDBTask(ctx); err != nil {
		t.Fatal(err)
	}
	var refs []dbmodel.RelayLogImage
	// 内存日志缓存是进程级的，前面用例的日志也会在这一次一起落库：只看这条日志的引用。
	if err := db.GetDB().Where("log_id = ?", full.ID).Order("hash").Find(&refs).Error; err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, r := range refs {
		hashes = append(hashes, r.Hash)
	}
	want := []string{sha(inImg), sha(outImg)}
	sort.Strings(want)
	if fmt.Sprint(hashes) != fmt.Sprint(want) {
		t.Fatalf("refs = %v, want %v (one row per image per log)", hashes, want)
	}
}

// 图片另存开或关，上游收到的 body 与请求头必须逐字节一致。
func TestLogImageCaptureKeepsOutboundBytesIdentical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	inImg := fakeImage(t, 9000)
	outImg := fakeImage(t, 3000)

	run := func(capture bool) capturedWire {
		ctx := setupRelayErrorDB(t)
		t.Cleanup(logimage.SetDir(t.TempDir()))
		t.Cleanup(logimage.WaitIdle)
		if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, 1); err != nil {
			t.Fatal(err)
		}
		prev := logImageCaptureEnabled
		logImageCaptureEnabled = func() bool { return capture }
		defer func() { logImageCaptureEnabled = prev }()
		wire, _ := runVisionChat(t, ctx, "vision-identity", b64(inImg), b64(outImg))
		return wire
	}
	on := run(true)
	off := run(false)
	if string(on.body) != string(off.body) {
		t.Fatalf("outbound body differs with image capture on/off:\non=%.400s\noff=%.400s", on.body, off.body)
	}
	if !strings.Contains(string(on.body), "data:image/png;base64,"+b64(inImg)) {
		t.Fatal("outbound body must carry the original data URL")
	}
	if fmt.Sprint(on.header) != fmt.Sprint(off.header) {
		t.Fatalf("outbound headers differ:\non=%v\noff=%v", on.header, off.header)
	}
}

func TestRedactKeepingImagesPreservesIDButStillRedacts(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 4000)
	c := newImageCapture()
	body := chatImageBody("data:image/png;base64," + b64(img))
	out := redactKeepingImages(stripInlineBinaryWith(body, c), c)
	if !strings.Contains(out, "id="+sha(img)+"]") {
		t.Fatalf("image id lost: %s", out)
	}
	if strings.Contains(out, logCanaryKey) {
		t.Fatalf("secrets must still be redacted in request/response content: %s", out)
	}
	// 没登记任何图时与 redactLogBody 完全一致。
	if redactKeepingImages(body, nil) != redactLogBody(body) {
		t.Fatal("nil capture must equal redactLogBody")
	}
}

// 图片接口的请求内容：先剥图再截 8KB，图后面的参数仍然看得见。
func TestImagesRequestContentStripsBeforeTruncate(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 20000)
	payload := map[string]any{
		"model":  "gpt-image-1",
		"prompt": "edit it",
		"image":  "data:image/png;base64," + b64(img),
		"zsize":  "1024x1024",
	}
	m := newImagesRelayMetrics(0, 0, "", "gpt-image-1", "images", "/v1/images/edits")
	got := m.upstreamBodyLog.contentWithImages(func(c *imageCapture) string {
		return buildImagesRequestContentForLogWith(false, nil, payload, c)
	})
	if strings.Contains(got, b64(img)[:100]) || strings.Contains(got, "(truncated)") {
		t.Fatalf("base64 must be replaced before truncation: %.300s", got)
	}
	if !strings.Contains(got, "id="+sha(img)+"]") || !strings.Contains(got, `"zsize":"1024x1024"`) {
		t.Fatalf("want id placeholder and trailing params, got %s", got)
	}
	if len(m.contentImageHashes) != 1 || m.contentImageHashes[0] != sha(img) {
		t.Fatalf("hashes = %v", m.contentImageHashes)
	}
	var rl dbmodel.RelayLog
	m.upstreamBodyLog.applyTo(&rl)
	if len(rl.ImageHashes) != 1 || rl.UpstreamRequestBody != "" {
		t.Fatalf("applyTo must carry content hashes even without an upstream body: %+v", rl.ImageHashes)
	}
	// 关闭时（不落库）：仍然剥掉 base64，只是不带 id、不存文件。
	off := buildImagesRequestContentForLog(false, nil, payload)
	if strings.Contains(off, b64(img)[:100]) || strings.Contains(off, "id=") || !strings.Contains(off, "[image omitted: image/png") {
		t.Fatalf("nil capture: %s", off)
	}
}

func TestGenericImageKeys(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 3000)
	notImg := strings.Repeat("QUJD", 200) // "ABC…" — base64，但不是图片
	c := newImageCapture()
	body := `{"base64Array":["` + b64(img) + `","https://example.com/a.png"],` +
		`"image":"https://example.com/b.png","mask":"` + notImg + `",` +
		`"images":["file-abc123"],"prompt":"data:image/png;base64,` + b64(img) + `"}`
	out := stripInlineBinaryWith(body, c)
	for _, keep := range []string{`"https://example.com/a.png"`, `"image":"https://example.com/b.png"`, `"mask":"` + notImg + `"`, `"images":["file-abc123"]`, `"prompt":"data:image/png;base64,` + b64(img) + `"`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("must keep %q untouched, got %s", keep, out)
		}
	}
	if !strings.Contains(out, `"base64Array":["[image: image/png, 2KB, id=`+sha(img)+`]"`) {
		t.Fatalf("raw base64 image in base64Array should be referenced: %s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("not JSON: %s", out)
	}
	// 幂等
	if again := stripInlineBinaryWith(out, newImageCapture()); again != out {
		t.Fatalf("second pass changed the body:\n%s\n%s", out, again)
	}
}

func TestFilterResponseForLogImages(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 2500)
	resp := &transformerModel.InternalLLMResponse{Choices: []transformerModel.Choice{{
		Message: &transformerModel.Message{Role: "assistant", Content: transformerModel.MessageContent{MultipleContent: []transformerModel.MessageContentPart{
			{Type: "text", Text: ptrStr("hi")},
			{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: "data:image/png;base64," + b64(img)}},
			{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: "https://cdn.example.com/x.png"}},
		}}},
	}}}
	before, _ := json.Marshal(resp)

	legacy, _ := json.Marshal(filterResponseForLogWith(resp, nil))
	if strings.Count(string(legacy), "[image data omitted for storage]") != 2 {
		t.Fatalf("nil capture must keep the legacy output: %s", legacy)
	}
	c := newImageCapture()
	got, _ := json.Marshal(filterResponseForLogWith(resp, c))
	if !strings.Contains(string(got), "id="+sha(img)+"]") || strings.Count(string(got), "[image data omitted for storage]") != 1 {
		t.Fatalf("data URL should be referenced, URL image stays legacy: %s", got)
	}
	// Images 字段（生图回包）：nil capture 照旧丢掉，打开时留下带 id 的占位符。
	resp.Choices[0].Message.Images = []transformerModel.MessageContentPart{{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: "data:image/png;base64," + b64(img)}}}
	before, _ = json.Marshal(resp)
	if legacy, _ := json.Marshal(filterResponseForLogWith(resp, nil)); strings.Contains(string(legacy), `"images"`) {
		t.Fatalf("nil capture must drop images: %s", legacy)
	}
	if got, _ := json.Marshal(filterResponseForLogWith(resp, newImageCapture())); !strings.Contains(string(got), `"images":[{"type":"image_url","image_url":{"url":"[image: image/png, 2KB, id=`+sha(img)+`]"}}]`) {
		t.Fatalf("images should be referenced: %s", got)
	}
	after, _ := json.Marshal(resp)
	if string(before) != string(after) {
		t.Fatal("filterResponseForLogWith mutated the live response")
	}
}

func ptrStr(s string) *string { return &s }
