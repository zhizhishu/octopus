package relay

import (
	"context"
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

// 渠道级路径逃生口（OpenAIImagesPath / OpenAIVideosPath / OpenAITasksPath）的
// 端到端证据。契约第四节那 7 条，能走真实调用路径的一律走真实调用路径：
// 真正把请求打到 httptest 上游，然后看上游收到的 path。
//
// 测试域名一律 upstream.example 系（公开仓铁律）。

type pathHit struct {
	Method string
	Path   string
}

// recordingUpstream 记录上游收到的每一次 (method, path)，并回固定 JSON。
func recordingUpstream(t *testing.T, body string) (*httptest.Server, func() []pathHit) {
	t.Helper()
	var (
		mu   sync.Mutex
		hits []pathHit
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, pathHit{Method: r.Method, Path: r.URL.Path})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() []pathHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]pathHit(nil), hits...)
	}
}

// createPathTestChannel 建一条真实落库的渠道，mutate 用来挂路径逃生口。
func createPathTestChannel(t *testing.T, ctx context.Context, name string, channelType outbound.OutboundType, upstreamURL, modelName string, mutate func(*dbmodel.Channel)) dbmodel.Channel {
	t.Helper()
	channel := dbmodel.Channel{
		Name:     name,
		Type:     channelType,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-path-test"}},
	}
	if mutate != nil {
		mutate(&channel)
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	return channel
}

func newPathTestGin(method, target, body string) (*httptest.ResponseRecorder, *gin.Context) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return rec, c
}

func newVideoPollGin(taskID string) (*httptest.ResponseRecorder, *gin.Context) {
	rec, c := newPathTestGin(http.MethodGet, "/v1/videos/"+taskID, "")
	c.Params = []gin.Param{{Key: "id", Value: taskID}}
	return rec, c
}

// ---------------------------------------------------------------------------
// 契约第四节第 1 条：默认行为零变化。三个字段都空时，打出去的上游 URL 与改动前
// 逐字节相同。这条最重要 —— 存量渠道不能因为这次改动有任何行为漂移。
// ---------------------------------------------------------------------------

// 图片：空字段 → POST /v1/images/generations（改动前是 "/v1"+endpoint）。
func TestImagesUpstreamPathUnchangedWhenOverrideEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream, hits := recordingUpstream(t, `{"created":1,"data":[{"url":"https://upstream.example/img.png"}]}`)
	createPathTestChannel(t, ctx, "images-default-path", outbound.OutboundTypeOpenAIChat, upstream.URL, "img-default-model", nil)

	rec, c := newPathTestGin(http.MethodPost, "/v1/images/generations",
		`{"model":"img-default-model","prompt":"a cat","n":1,"response_format":"url"}`)
	ImagesHandler("/images/generations", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("images request failed: %d %s", rec.Code, rec.Body.String())
	}
	got := hits()
	want := []pathHit{{Method: http.MethodPost, Path: "/v1/images/generations"}}
	assertPathHits(t, got, want)
}

// 视频：空字段 → 创建打 /v1/videos/edits，轮询打 /v1/videos/edits/{id}。
// 两边都必须与改动前逐字节相同。
func TestVideosUpstreamPathsUnchangedWhenOverrideEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream, hits := recordingUpstream(t, `{"id":"vid_default_1","status":"queued"}`)
	createPathTestChannel(t, ctx, "videos-default-path", outbound.OutboundTypeOpenAIChat, upstream.URL, "video-default-model", nil)

	rec, c := newPathTestGin(http.MethodPost, "/v1/videos/edits", `{"model":"video-default-model","prompt":"a slow sunrise"}`)
	VideosHandlerKind(c, VideoMediaKindEdits)
	if rec.Code != http.StatusOK {
		t.Fatalf("video create failed: %d %s", rec.Code, rec.Body.String())
	}

	pollRec, pollCtx := newVideoPollGin("vid_default_1")
	VideoPollHandler(pollCtx)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("video poll failed: %d %s", pollRec.Code, pollRec.Body.String())
	}

	assertPathHits(t, hits(), []pathHit{
		{Method: http.MethodPost, Path: "/v1/videos/edits"},
		{Method: http.MethodGet, Path: "/v1/videos/edits/vid_default_1"},
	})
}

// 组合层逐字节对照：空字段时 videoUpstreamPathForChannel 必须与纯函数
// videoUpstreamPath 完全一致，图片 getter 必须与 "/v1"+endpoint 完全一致。
// （e2e 只覆盖两条路径，这里把三种 kind、取成品、未知 kind 全部钉死。）
func TestPathResolutionIsByteIdenticalToLegacyWhenOverridesEmpty(t *testing.T) {
	for _, channel := range []*dbmodel.Channel{nil, {}, {OpenAIImagesPath: "", OpenAIVideosPath: "", OpenAITasksPath: ""}} {
		for _, endpoint := range []string{"/images/generations", "/images/edits", "/images/variations"} {
			if got, want := channel.GetOpenAIImagesPath(endpoint), "/v1"+endpoint; got != want {
				t.Fatalf("images path (channel %+v, endpoint %q) = %q, want legacy %q", channel, endpoint, got, want)
			}
		}
		for _, kind := range []string{"", VideoMediaKindGenerations, VideoMediaKindEdits, VideoMediaKindExtensions, "unknown-kind"} {
			for _, id := range []string{"", "vid_1", "a/b c"} {
				for _, wantContent := range []bool{false, true} {
					legacy := videoUpstreamPath(kind, id, wantContent)
					if got := videoUpstreamPathForChannel(channel, kind, id, wantContent); got != legacy {
						t.Fatalf("video path (channel %+v, kind %q, id %q, content %v) = %q, want legacy %q",
							channel, kind, id, wantContent, got, legacy)
					}
				}
			}
		}
	}

	// 图片取成品 / 视频取成品也走同一套 getter，默认值必须还是老样子。
	channel := &dbmodel.Channel{}
	if got, want := videoUpstreamPathForChannel(channel, VideoMediaKindEdits, "vid_1", true), "/v1/videos/edits/vid_1/content"; got != want {
		t.Fatalf("video content path = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 契约第四节第 2 条：图片逃生口
// ---------------------------------------------------------------------------

// 填 /openai/images 后，/v1/images/generations 的请求打到 /openai/images/generations。
func TestImagesPathOverrideReplacesV1ImagesPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream, hits := recordingUpstream(t, `{"created":1,"data":[{"url":"https://upstream.example/img.png"}]}`)
	createPathTestChannel(t, ctx, "images-custom-path", outbound.OutboundTypeOpenAIChat, upstream.URL, "img-custom-model",
		func(ch *dbmodel.Channel) { ch.OpenAIImagesPath = "/openai/images" })

	rec, c := newPathTestGin(http.MethodPost, "/v1/images/generations",
		`{"model":"img-custom-model","prompt":"a cat","n":1,"response_format":"url"}`)
	ImagesHandler("/images/generations", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("images request failed: %d %s", rec.Code, rec.Body.String())
	}
	assertPathHits(t, hits(), []pathHit{{Method: http.MethodPost, Path: "/openai/images/generations"}})

	// 操作段必须跟着走：edits 不能被打回 generations。
	if got, want := (&dbmodel.Channel{OpenAIImagesPath: "/openai/images"}).GetOpenAIImagesPath("/images/edits"), "/openai/images/edits"; got != want {
		t.Fatalf("images edits path = %q, want %q", got, want)
	}
	if got, want := (&dbmodel.Channel{OpenAIImagesPath: "/openai/images"}).GetOpenAIImagesPath("/images/variations"), "/openai/images/variations"; got != want {
		t.Fatalf("images variations path = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 契约第四节第 3 条：视频逃生口保留 kind
// ---------------------------------------------------------------------------

// 填 /kling/v1/videos 后，edits 任务创建打到 /kling/v1/videos/edits，轮询打到
// /kling/v1/videos/edits/{id}，**不是**打回 generations。
func TestVideosPathOverrideKeepsMediaKind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream, hits := recordingUpstream(t, `{"id":"vid_kling_1","status":"queued"}`)
	createPathTestChannel(t, ctx, "videos-custom-path", outbound.OutboundTypeOpenAIChat, upstream.URL, "video-custom-model",
		func(ch *dbmodel.Channel) { ch.OpenAIVideosPath = "/kling/v1/videos" })

	rec, c := newPathTestGin(http.MethodPost, "/v1/videos/edits", `{"model":"video-custom-model","prompt":"a slow sunrise"}`)
	VideosHandlerKind(c, VideoMediaKindEdits)
	if rec.Code != http.StatusOK {
		t.Fatalf("video create failed: %d %s", rec.Code, rec.Body.String())
	}

	pollRec, pollCtx := newVideoPollGin("vid_kling_1")
	VideoPollHandler(pollCtx)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("video poll failed: %d %s", pollRec.Code, pollRec.Body.String())
	}

	got := hits()
	assertPathHits(t, got, []pathHit{
		{Method: http.MethodPost, Path: "/kling/v1/videos/edits"},
		{Method: http.MethodGet, Path: "/kling/v1/videos/edits/vid_kling_1"},
	})
	// 显式反向断言：kind 丢了的话这里会变成 .../generations（上一轮刚修好的 bug）。
	for _, hit := range got {
		if strings.HasSuffix(hit.Path, "/generations") {
			t.Fatalf("edits 任务被打到 generations 上去了: %+v", got)
		}
	}
}

// ---------------------------------------------------------------------------
// 契约第四节第 4 条：任务查询路径独立
// ---------------------------------------------------------------------------

// OpenAIVideosPath=/v1/videos + OpenAITasksPath=/v1/tasks 时，
// 创建走 /v1/videos/edits，查询走 /v1/tasks/{id}。
func TestTasksPathOverrideIsIndependentOfVideosPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream, hits := recordingUpstream(t, `{"id":"vid_task_1","status":"queued"}`)
	createPathTestChannel(t, ctx, "videos-tasks-path", outbound.OutboundTypeOpenAIChat, upstream.URL, "video-tasks-model",
		func(ch *dbmodel.Channel) {
			ch.OpenAIVideosPath = "/v1/videos"
			ch.OpenAITasksPath = "/v1/tasks"
		})

	rec, c := newPathTestGin(http.MethodPost, "/v1/videos/edits", `{"model":"video-tasks-model","prompt":"a slow sunrise"}`)
	VideosHandlerKind(c, VideoMediaKindEdits)
	if rec.Code != http.StatusOK {
		t.Fatalf("video create failed: %d %s", rec.Code, rec.Body.String())
	}

	pollRec, pollCtx := newVideoPollGin("vid_task_1")
	VideoPollHandler(pollCtx)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("video poll failed: %d %s", pollRec.Code, pollRec.Body.String())
	}

	assertPathHits(t, hits(), []pathHit{
		{Method: http.MethodPost, Path: "/v1/videos/edits"},
		{Method: http.MethodGet, Path: "/v1/tasks/vid_task_1"},
	})
}

// ---------------------------------------------------------------------------
// 契约第四节第 5 条：空字符串与未设置等价
// ---------------------------------------------------------------------------

func TestPathOverrideEmptyStringEqualsUnset(t *testing.T) {
	unset := &dbmodel.Channel{}
	explicitEmpty := &dbmodel.Channel{OpenAIImagesPath: "", OpenAIVideosPath: "", OpenAITasksPath: ""}
	whitespace := &dbmodel.Channel{OpenAIImagesPath: "  ", OpenAIVideosPath: " ", OpenAITasksPath: "\t"}

	for _, channel := range []*dbmodel.Channel{unset, explicitEmpty, whitespace} {
		if got, want := channel.GetOpenAIImagesPath("/images/generations"), "/v1/images/generations"; got != want {
			t.Fatalf("images path (channel %+v) = %q, want %q", channel, got, want)
		}
		if got, want := channel.GetOpenAIVideosPath(), "/v1/videos"; got != want {
			t.Fatalf("videos prefix (channel %+v) = %q, want %q", channel, got, want)
		}
		if got := channel.GetOpenAITasksPath(); got != "" {
			t.Fatalf("tasks prefix (channel %+v) = %q, want empty", channel, got)
		}
		if got, want := videoUpstreamPathForChannel(channel, VideoMediaKindEdits, "vid_1", false), "/v1/videos/edits/vid_1"; got != want {
			t.Fatalf("video poll path (channel %+v) = %q, want %q", channel, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 契约第四节第 7 条：未知渠道类型（退役的 4 号位）不崩、不被当成别的类型发出去
// ---------------------------------------------------------------------------

// type=4 的渠道在图片/视频/chat 三条路径上都必须被 Skip：客户端拿到错误，
// 上游一个请求都收不到（尤其不能当成 chat 渠道发出去）。
func TestRetiredChannelTypeFourIsSkippedOnEveryPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream, hits := recordingUpstream(t, `{"id":"vid_should_not_exist","status":"queued"}`)
	// 退役类型：4 号位。库里可能还留着这样的行。
	createPathTestChannel(t, ctx, "retired-type-4", outbound.OutboundType(4), upstream.URL, "retired-model", nil)

	t.Run("images", func(t *testing.T) {
		rec, c := newPathTestGin(http.MethodPost, "/v1/images/generations",
			`{"model":"retired-model","prompt":"a cat","n":1,"response_format":"url"}`)
		ImagesHandler("/images/generations", c)
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("type 4 must not serve images, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("videos", func(t *testing.T) {
		rec, c := newPathTestGin(http.MethodPost, "/v1/videos", `{"model":"retired-model","prompt":"a cat"}`)
		VideosHandler(c)
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("type 4 must not serve videos, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("chat", func(t *testing.T) {
		rec, c := newPathTestGin(http.MethodPost, "/v1/chat/completions",
			`{"model":"retired-model","messages":[{"role":"user","content":"hi"}]}`)
		Handler(inbound.InboundTypeOpenAIChat, c)
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("type 4 must not serve chat, got %d %s", rec.Code, rec.Body.String())
		}
	})

	if got := hits(); len(got) != 0 {
		t.Fatalf("a retired type-4 channel reached the upstream %d time(s): %+v", len(got), got)
	}
}

func assertPathHits(t *testing.T, got, want []pathHit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("upstream hits = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("upstream hit #%d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}
