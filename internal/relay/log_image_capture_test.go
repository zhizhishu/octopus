package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/logimage"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// fakeImage 返回以 PNG 文件头开头、总长 n 字节的随机数据（Sniff 只看文件头）。
func fakeImage(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	copy(b, "\x89PNG\r\n\x1a\n")
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func chatImageBody(urls ...string) string {
	var parts []string
	parts = append(parts, `{"type":"text","text":"看看这些图 `+logCanaryKey+`"}`)
	for _, u := range urls {
		parts = append(parts, `{"type":"image_url","image_url":{"url":"`+u+`","detail":"auto"}}`)
	}
	return `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
}

// withLogImages 打开图片另存（不依赖设置缓存），并把写盘指到这个用例自己的目录。
func withLogImages(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(logimage.SetDir(dir))
	t.Cleanup(logimage.WaitIdle) // 先等写完，再让 TempDir 清目录
	prev := logImageCaptureEnabled
	logImageCaptureEnabled = func() bool { return true }
	t.Cleanup(func() { logImageCaptureEnabled = prev })
	return dir
}

func countFiles(t *testing.T) int {
	t.Helper()
	logimage.WaitIdle()
	files, err := logimage.List()
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func TestUpstreamBodyImagePlaceholderCarriesContentHash(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 3000)
	h := sha(img)

	raw := []byte(chatImageBody("data:image/png;base64," + b64(img)))
	before := append([]byte(nil), raw...)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody(raw)

	// 🔴 出站字节一个都不能动。
	if !bytes.Equal(raw, before) {
		t.Fatal("SetUpstreamRequestBody mutated the outbound buffer")
	}
	want := fmt.Sprintf(`"[image: image/png, 2KB, id=%s]"`, h)
	if !strings.Contains(m.upstreamRequestBody, want) {
		t.Fatalf("placeholder missing, want %s in %s", want, m.upstreamRequestBody)
	}
	if strings.Contains(m.upstreamRequestBody, b64(img)[:64]) {
		t.Fatal("base64 payload leaked into the log text")
	}
	// 文字原样：凭据不打码。
	if !strings.Contains(m.upstreamRequestBody, logCanaryKey) || strings.Contains(m.upstreamRequestBody, "[redacted]") {
		t.Fatalf("text must stay verbatim and unredacted: %s", m.upstreamRequestBody)
	}
	if len(m.upstreamImageHashes) != 1 || m.upstreamImageHashes[0] != h {
		t.Fatalf("hashes = %v", m.upstreamImageHashes)
	}
	logimage.WaitIdle()
	p, ext, ok := logimage.Find(h)
	if !ok || ext != "png" {
		t.Fatalf("image file not saved (ok=%v ext=%s)", ok, ext)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, img) {
		t.Fatal("saved bytes differ from the original image")
	}
	// 第二遍剥离必须原样保留新占位符（幂等）。
	if again := stripInlineBinaryForLog(m.upstreamRequestBody); again != m.upstreamRequestBody {
		t.Fatalf("strip is not idempotent on the id placeholder:\n%s\n%s", m.upstreamRequestBody, again)
	}
}

func TestUpstreamBodyImageCaptureCoversProviderShapes(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 2048)
	enc := b64(img)
	h := sha(img)
	bodies := map[string]string{
		"anthropic":       `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + enc + `"}}]}]}`,
		"gemini":          `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + enc + `"}}]}]}`,
		"responses-raw":   `{"input":[{"type":"input_image","image_url":"` + enc + `"}]}`,
		"responses-data":  `{"input":[{"type":"input_image","image_url":"data:image/png;base64,` + enc + `"}]}`,
		"jpeg-as-png-lie": `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + enc + `"}}]}]}`,
	}
	for name, body := range bodies {
		m := NewRelayMetrics(0, 0, "", "m", nil)
		m.SetUpstreamRequestBody([]byte(body))
		// mime 取文件头嗅探结果，不信客户端声明（jpeg-as-png-lie）。
		want := "[image: image/png, 2KB, id=" + h + "]"
		if !strings.Contains(m.upstreamRequestBody, want) {
			t.Fatalf("%s: placeholder missing: %s", name, m.upstreamRequestBody)
		}
	}
	if n := countFiles(t); n != 1 {
		t.Fatalf("same image across requests must be stored once, got %d files", n)
	}
}

func TestUpstreamBodyImageDedupeWithinRequest(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 1500)
	u := "data:image/png;base64," + b64(img)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody([]byte(chatImageBody(u, u, u)))
	if c := strings.Count(m.upstreamRequestBody, "id="+sha(img)); c != 3 {
		t.Fatalf("each occurrence should keep its own placeholder, got %d", c)
	}
	if len(m.upstreamImageHashes) != 1 {
		t.Fatalf("one unique image expected, got %v", m.upstreamImageHashes)
	}
	if n := countFiles(t); n != 1 {
		t.Fatalf("files = %d", n)
	}
}

func TestUpstreamBodyImageNonImageAndHTTPURLsUntouched(t *testing.T) {
	withLogImages(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script>` + strings.Repeat(" ", 200) + `</svg>`)
	body := chatImageBody(
		"data:image/svg+xml;base64,"+b64(svg),
		"https://cdn.example.com/a.png",
	)
	body = strings.Replace(body, `]}]}`, `,{"type":"input_audio","input_audio":{"data":"`+b64(fakeImage(t, 900))+`","format":"wav"}}]}]}`, 1)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody([]byte(body))
	if !strings.Contains(m.upstreamRequestBody, "[image omitted: image/svg+xml,") {
		t.Fatalf("svg must keep the old omitted placeholder: %s", m.upstreamRequestBody)
	}
	if !strings.Contains(m.upstreamRequestBody, `"https://cdn.example.com/a.png"`) {
		t.Fatalf("http URL must be kept verbatim: %s", m.upstreamRequestBody)
	}
	if !strings.Contains(m.upstreamRequestBody, "[audio omitted: audio/wav,") {
		t.Fatalf("audio must not be saved: %s", m.upstreamRequestBody)
	}
	if strings.Contains(m.upstreamRequestBody, "id=") || len(m.upstreamImageHashes) != 0 {
		t.Fatalf("nothing should be saved: %s", m.upstreamRequestBody)
	}
	if n := countFiles(t); n != 0 {
		t.Fatalf("files = %d", n)
	}
}

func TestUpstreamBodyImageSizeLimits(t *testing.T) {
	withLogImages(t)

	t.Run("single image over 10MB", func(t *testing.T) {
		big := fakeImage(t, logimage.MaxImageBytes+1024)
		m := NewRelayMetrics(0, 0, "", "m", nil)
		m.SetUpstreamRequestBody([]byte(chatImageBody("data:image/png;base64," + b64(big))))
		if !strings.Contains(m.upstreamRequestBody, "[image omitted: image/png, 10MB, too large]") {
			t.Fatalf("too-large reason missing: %.300s", m.upstreamRequestBody)
		}
		if len(m.upstreamImageHashes) != 0 {
			t.Fatal("oversized image must not be saved")
		}
	})

	t.Run("more than 16 images", func(t *testing.T) {
		var urls []string
		for i := 0; i < logimage.MaxImagesPerRequest+2; i++ {
			urls = append(urls, "data:image/png;base64,"+b64(fakeImage(t, 600)))
		}
		m := NewRelayMetrics(0, 0, "", "m", nil)
		m.SetUpstreamRequestBody([]byte(chatImageBody(urls...)))
		if len(m.upstreamImageHashes) != logimage.MaxImagesPerRequest {
			t.Fatalf("saved %d, want %d", len(m.upstreamImageHashes), logimage.MaxImagesPerRequest)
		}
		if c := strings.Count(m.upstreamRequestBody, ", too many images]"); c != 2 {
			t.Fatalf("too-many reason count = %d: %s", c, m.upstreamRequestBody)
		}
	})

	t.Run("over 40MB per request", func(t *testing.T) {
		var urls []string
		for i := 0; i < 5; i++ { // 5 × 9MB：前 4 张 36MB 能存，第 5 张超总量
			urls = append(urls, "data:image/png;base64,"+b64(fakeImage(t, 9<<20)))
		}
		m := NewRelayMetrics(0, 0, "", "m", nil)
		m.SetUpstreamRequestBody([]byte(chatImageBody(urls...)))
		if len(m.upstreamImageHashes) != 4 {
			t.Fatalf("saved %d, want 4", len(m.upstreamImageHashes))
		}
		if !strings.Contains(m.upstreamRequestBody, "[image omitted: image/png, 9MB, request image limit reached]") {
			t.Fatalf("budget reason missing: %.2000s", m.upstreamRequestBody)
		}
	})
}

func TestUpstreamBodyImageQueueFullFallsBackToOmitted(t *testing.T) {
	withLogImages(t)
	prev := logImageEnqueue
	logImageEnqueue = func(string, string, []byte) bool { return false }
	t.Cleanup(func() { logImageEnqueue = prev })

	img := fakeImage(t, 4096)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody([]byte(chatImageBody("data:image/png;base64," + b64(img))))
	if !strings.Contains(m.upstreamRequestBody, `"[image omitted: image/png, 4KB, save queue full]"`) {
		t.Fatalf("queue-full fallback missing: %s", m.upstreamRequestBody)
	}
	if strings.Contains(m.upstreamRequestBody, "id=") || len(m.upstreamImageHashes) != 0 {
		t.Fatalf("a dropped image must not leave an id behind: %s", m.upstreamRequestBody)
	}
}

func TestUpstreamBodyImageRealQueueFullNeverBlocks(t *testing.T) {
	withLogImages(t)
	resume := logimage.PauseWorkerForTest()
	defer func() { resume(); logimage.WaitIdle() }()
	var urls []string
	for i := 0; i < logimage.MaxImagesPerRequest; i++ {
		urls = append(urls, "data:image/png;base64,"+b64(fakeImage(t, 800)))
	}
	saved, omitted := 0, 0
	for r := 0; r < 8; r++ { // 8×16 = 128 张，远超 64 格队列
		m := NewRelayMetrics(0, 0, "", "m", nil)
		var fresh []string
		for range urls {
			fresh = append(fresh, "data:image/png;base64,"+b64(fakeImage(t, 800)))
		}
		m.SetUpstreamRequestBody([]byte(chatImageBody(fresh...)))
		saved += len(m.upstreamImageHashes)
		omitted += strings.Count(m.upstreamRequestBody, "save queue full")
		// 每个留下的 id 都必须对应一张已入队的图。
		if c := strings.Count(m.upstreamRequestBody, "id="); c != len(m.upstreamImageHashes) {
			t.Fatalf("ids in text %d != saved %d", c, len(m.upstreamImageHashes))
		}
	}
	if omitted == 0 || saved == 0 {
		t.Fatalf("expected both saved and dropped images, saved=%d omitted=%d", saved, omitted)
	}
}

func TestUpstreamBodyImageCaptureOffSavesNothing(t *testing.T) {
	withLogImages(t)
	logImageCaptureEnabled = func() bool { return false }
	img := fakeImage(t, 2000)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody([]byte(chatImageBody("data:image/png;base64," + b64(img))))
	if !strings.Contains(m.upstreamRequestBody, "[image omitted: image/png, 1KB]") {
		t.Fatalf("capture off must keep the legacy placeholder: %s", m.upstreamRequestBody)
	}
	if n := countFiles(t); n != 0 {
		t.Fatalf("files = %d", n)
	}
}

func TestLogImageCaptureFollowsHistoryLogSetting(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	if defaultLogImageCaptureEnabled() {
		t.Fatal("history logging off must disable image capture")
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if !defaultLogImageCaptureEnabled() {
		t.Fatal("history logging on should enable image capture")
	}
}

func TestUpstreamBodyImageTruncatedAwayIsNotSaved(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 2000)
	pad := strings.Repeat("x", upstreamRequestBodyLogLimit)
	body := `{"model":"m","pad":"` + pad + `","messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64(img) + `"}}]}]}`
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.SetUpstreamRequestBody([]byte(body))
	if !strings.Contains(m.upstreamRequestBody, "truncated") {
		t.Fatal("expected truncation")
	}
	if len(m.upstreamImageHashes) != 0 || countFiles(t) != 0 {
		t.Fatal("an image whose placeholder was truncated away must not be saved")
	}
}

func TestRequestContentKeepsLegacyPlaceholder(t *testing.T) {
	withLogImages(t)
	img := fakeImage(t, 2000)
	got := stripInlineBinaryForLog(chatImageBody("data:image/png;base64," + b64(img)))
	if strings.Contains(got, "id=") || !strings.Contains(got, "[image omitted: image/png, 1KB]") {
		t.Fatalf("request_content path must not change: %s", got)
	}
	if n := countFiles(t); n != 0 {
		t.Fatalf("files = %d", n)
	}
}

var placeholderIDRe = regexp.MustCompile(`id=([0-9a-f]{64})\]`)

// 端到端：开关打开时出站字节与客户端发来的图片逐字节一致、图片落盘并写引用；
// 开关关闭时一张图都不存。
func TestUpstreamBodyImageEndToEnd(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("toggle=%v", enabled), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			dir := t.TempDir()
			t.Cleanup(logimage.SetDir(dir))
			t.Cleanup(logimage.WaitIdle)
			v := 0
			if enabled {
				v = 1
			}
			if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, v); err != nil {
				t.Fatal(err)
			}

			img := fakeImage(t, 5000)
			enc := b64(img)
			var mu sync.Mutex
			var received []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				received = b
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"vision-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			t.Cleanup(upstream.Close)
			createImageBridgeGroup(t, ctx, upstream.URL, "vision-chat", "vision-chat", outbound.OutboundTypeOpenAIChat)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"vision-chat","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`+enc+`"}}]}]}`))
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
			wire := string(received)
			mu.Unlock()
			if !strings.Contains(wire, enc) {
				t.Fatal("outbound request lost or altered the image bytes")
			}

			body := latestUpstreamRequestBody(t, ctx)
			logimage.WaitIdle()
			files, _ := logimage.List()
			if !enabled {
				if body != "" || len(files) != 0 {
					t.Fatalf("toggle off must store nothing (body=%q files=%d)", body, len(files))
				}
				return
			}
			match := placeholderIDRe.FindStringSubmatch(body)
			if match == nil || match[1] != sha(img) {
				t.Fatalf("expected id placeholder for the image, got %s", body)
			}
			if len(files) != 1 || files[0].Hash != sha(img) {
				t.Fatalf("expected the image on disk, got %+v", files)
			}
			if err := op.RelayLogSaveDBTask(ctx); err != nil {
				t.Fatal(err)
			}
			var refs []dbmodel.RelayLogImage
			if err := db.GetDB().Find(&refs).Error; err != nil {
				t.Fatal(err)
			}
			if len(refs) != 1 || refs[0].Hash != sha(img) || refs[0].LogID == 0 {
				t.Fatalf("expected one reference row, got %+v", refs)
			}
		})
	}
}
