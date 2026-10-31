package relay

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 契约第二节的路径表：4 种 kind × 3 种能力（创建 / 查询 / 取成品）= 12 个用例，
// 逐行对照。任何一格拼错，上游就是 404，而且只有在真跑到那个 kind 时才会暴露。
func TestVideoUpstreamPath(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		taskID      string
		wantContent bool
		want        string
	}{
		// kind = ""（兼容别名 /videos，也是存量任务的零值）
		{name: "alias_create", kind: "", taskID: "", wantContent: false, want: "/v1/videos"},
		{name: "alias_poll", kind: "", taskID: "vid-1", wantContent: false, want: "/v1/videos/vid-1"},
		{name: "alias_content", kind: "", taskID: "vid-1", wantContent: true, want: "/v1/videos/vid-1/content"},

		// kind = generations
		{name: "generations_create", kind: VideoMediaKindGenerations, taskID: "", wantContent: false, want: "/v1/videos/generations"},
		{name: "generations_poll", kind: VideoMediaKindGenerations, taskID: "vid-1", wantContent: false, want: "/v1/videos/generations/vid-1"},
		{name: "generations_content", kind: VideoMediaKindGenerations, taskID: "vid-1", wantContent: true, want: "/v1/videos/generations/vid-1/content"},

		// kind = edits
		{name: "edits_create", kind: VideoMediaKindEdits, taskID: "", wantContent: false, want: "/v1/videos/edits"},
		{name: "edits_poll", kind: VideoMediaKindEdits, taskID: "vid-1", wantContent: false, want: "/v1/videos/edits/vid-1"},
		{name: "edits_content", kind: VideoMediaKindEdits, taskID: "vid-1", wantContent: true, want: "/v1/videos/edits/vid-1/content"},

		// kind = extensions
		{name: "extensions_create", kind: VideoMediaKindExtensions, taskID: "", wantContent: false, want: "/v1/videos/extensions"},
		{name: "extensions_poll", kind: VideoMediaKindExtensions, taskID: "vid-1", wantContent: false, want: "/v1/videos/extensions/vid-1"},
		{name: "extensions_content", kind: VideoMediaKindExtensions, taskID: "vid-1", wantContent: true, want: "/v1/videos/extensions/vid-1/content"},
	}

	if len(cases) != 12 {
		t.Fatalf("the contract table is 4 kinds x 3 capabilities = 12 cases, got %d", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := videoUpstreamPath(tc.kind, tc.taskID, tc.wantContent); got != tc.want {
				t.Fatalf("videoUpstreamPath(%q, %q, %v) = %q, want %q", tc.kind, tc.taskID, tc.wantContent, got, tc.want)
			}
		})
	}
}

// 未知 kind 一律退回 "" 分支：库里的行可能被手工改过，一条看不懂的 kind
// 不许让整条轮询链拼出畸形 URL（/v1/videos/bogus/{id}），更不许直接报错。
func TestVideoUpstreamPathUnknownKindFallsBackToAlias(t *testing.T) {
	for _, kind := range []string{"bogus", "GENERATIONS", " generations", "edits/content", "videos"} {
		if got := videoUpstreamPath(kind, "vid-1", false); got != "/v1/videos/vid-1" {
			t.Fatalf("kind %q: poll path = %q, want the alias fallback /v1/videos/vid-1", kind, got)
		}
		if got := videoUpstreamPath(kind, "", false); got != "/v1/videos" {
			t.Fatalf("kind %q: create path = %q, want the alias fallback /v1/videos", kind, got)
		}
		if got := videoUpstreamPath(kind, "vid-1", true); got != "/v1/videos/vid-1/content" {
			t.Fatalf("kind %q: content path = %q, want the alias fallback /v1/videos/vid-1/content", kind, got)
		}
	}
}

// 【存量兼容】零值必须是老行为：改动前创建的任务（内存条目 / 库里那一行都没有
// kind 这个字段）重启后查询路径不能变，否则一次部署就让所有在途任务全错。
func TestVideoUpstreamPathLegacyZeroValue(t *testing.T) {
	entry := videoTaskEntry{}
	if entry.mediaKind != "" {
		t.Fatalf("zero value mediaKind must be %q, got %q", "", entry.mediaKind)
	}
	if got := videoUpstreamPath(entry.mediaKind, "vid-legacy", false); got != "/v1/videos/vid-legacy" {
		t.Fatalf("legacy zero-value poll path = %q, want /v1/videos/vid-legacy", got)
	}
	var row dbmodel.VideoTask
	if row.MediaKind != "" {
		t.Fatalf("zero value model.VideoTask.MediaKind must be %q, got %q", "", row.MediaKind)
	}
}

// taskID 必须 url.PathEscape：上游 id 里带 '/'、空格或 '%' 时，未转义会把一个
// 查询打到一个不存在的子路径上（甚至换一条路由）。
func TestVideoUpstreamPathEscapesTaskID(t *testing.T) {
	if got := videoUpstreamPath(VideoMediaKindEdits, "a/b c", false); got != "/v1/videos/edits/a%2Fb%20c" {
		t.Fatalf("escaped poll path = %q, want /v1/videos/edits/a%%2Fb%%20c", got)
	}
}

// 【路径一致性】创建时用的 kind，必须能被轮询原样取回：这是「查询回到同一个
// 子路径」在内存这条路径上的唯一依据。
func TestVideoTaskOwnerRoundTripsMediaKind(t *testing.T) {
	for _, kind := range []string{"", VideoMediaKindGenerations, VideoMediaKindEdits, VideoMediaKindExtensions} {
		t.Run("kind="+kind, func(t *testing.T) {
			resetVideoTaskMemory(t)
			recordVideoTaskOwner([]string{"vid_kind"}, 11, 22, "video-model", 33, 44, kind)
			entry, ok := videoTaskOwner("vid_kind")
			if !ok {
				t.Fatalf("owner for vid_kind not found")
			}
			if entry.mediaKind != kind {
				t.Fatalf("recorded kind came back as %q, want %q", entry.mediaKind, kind)
			}
			// 一致性：创建那一步和轮询那一步算出来的上游路径必须是同一个子路径。
			createPath := videoUpstreamPath(kind, "", false)
			pollPath := videoUpstreamPath(entry.mediaKind, "vid_kind", false)
			if !strings.HasPrefix(pollPath, createPath+"/") {
				t.Fatalf("create path %q and poll path %q disagree on the sub-path", createPath, pollPath)
			}
		})
	}
}

// 【走真实调用路径·创建 + 轮询】VideosHandlerKind(edits) 创建，然后 VideoPollHandler
// 轮询：发往上游的两条 URL 都必须落在 /v1/videos/edits 下面。
//
// 只测 videoUpstreamPath 纯函数不算数 —— 这个仓库栽过三次「测了 helper 没测调用点」。
func TestVideoMediaKindCreateAndPollHitMatchingSubPath(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		index int
	}{
		{kind: VideoMediaKindGenerations, index: 0},
		{kind: VideoMediaKindEdits, index: 1},
		{kind: VideoMediaKindExtensions, index: 2},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			resetVideoTaskMemory(t)

			var (
				mu         sync.Mutex
				createPath string
				pollPath   string
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if r.Method == http.MethodPost {
					createPath = r.URL.Path
				} else {
					pollPath = r.URL.Path
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"id":"vid_kind_1","status":"queued"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"vid_kind_1","status":"completed","video_url":"https://upstream.example/v.mp4"}`))
			}))
			t.Cleanup(upstream.Close)

			alias := "alias-" + tc.kind
			upstreamModel := "upstream-" + tc.kind
			ch := dbmodel.Channel{
				Name:     "video-kind-" + tc.kind,
				Type:     outbound.OutboundTypeOpenAIChat,
				Enabled:  true,
				Model:    upstreamModel,
				Priority: tc.index + 1,
				ModelMapping: map[string]string{
					alias: upstreamModel,
				},
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "kind-secret"}},
			}
			if err := op.ChannelCreate(&ch, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			// 创建：客户端的请求路径就是那个子路径。
			recCreate := httptest.NewRecorder()
			cCreate, _ := gin.CreateTestContext(recCreate)
			reqCreate := httptest.NewRequest(http.MethodPost, "/v1/videos/"+tc.kind, strings.NewReader(`{"model":"`+alias+`","prompt":"a cat"}`))
			reqCreate.Header.Set("Content-Type", "application/json")
			cCreate.Request = reqCreate
			cCreate.Set("api_key_id", 0)
			cCreate.Set("user_id", videoTestOwnerUserID)
			cCreate.Set("request_ip", "127.0.0.1")

			VideosHandlerKind(cCreate, tc.kind)

			if recCreate.Code != http.StatusOK {
				t.Fatalf("create via %s = %d %s", tc.kind, recCreate.Code, recCreate.Body.String())
			}

			// 轮询：客户端仍然打同一个子路径（这也是路由注册的那条）。
			recPoll := httptest.NewRecorder()
			cPoll, _ := gin.CreateTestContext(recPoll)
			cPoll.Params = []gin.Param{{Key: "id", Value: "vid_kind_1"}}
			reqPoll := httptest.NewRequest(http.MethodGet, "/v1/videos/"+tc.kind+"/vid_kind_1", nil)
			cPoll.Request = reqPoll
			cPoll.Set("api_key_id", 0)
			cPoll.Set("user_id", videoTestOwnerUserID)
			cPoll.Set("request_ip", "127.0.0.1")

			VideoPollHandler(cPoll)

			if recPoll.Code != http.StatusOK {
				t.Fatalf("poll via %s = %d %s", tc.kind, recPoll.Code, recPoll.Body.String())
			}

			mu.Lock()
			gotCreate, gotPoll := createPath, pollPath
			mu.Unlock()

			wantCreate := "/v1/videos/" + tc.kind
			wantPoll := "/v1/videos/" + tc.kind + "/vid_kind_1"
			if gotCreate != wantCreate {
				t.Fatalf("upstream create path = %q, want %q", gotCreate, wantCreate)
			}
			if gotPoll != wantPoll {
				t.Fatalf("upstream poll path = %q, want %q (a task created on one sub-path must be polled on the same one)", gotPoll, wantPoll)
			}

			// 落库那一行也必须记着 kind：重启后内存没了，轮询只能靠它。
			row, err := op.VideoTaskGetByUpstreamID(ctx, "vid_kind_1")
			if err != nil {
				t.Fatalf("read back row: %v", err)
			}
			if row == nil {
				t.Fatal("create did not persist a video_tasks row")
			}
			if row.MediaKind != tc.kind {
				t.Fatalf("row media_kind = %q, want %q", row.MediaKind, tc.kind)
			}
		})
	}
}

// 【走真实调用路径·重启】内存归属清空后，轮询只能靠库里那一行的 media_kind：
// 一条在 /videos/edits 创建的任务，重启后必须仍然回到 /v1/videos/edits/{id}。
func TestVideoMediaKindPollAfterRestartUsesRowKind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	var (
		mu       sync.Mutex
		pollPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pollPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_edit_db","status":"completed"}`))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-kind-restart",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "restart-upstream-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "restart-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	// 「上一个进程」留下的那一行。
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_edit_db",
		OwnerUserID:    videoTestOwnerUserID,
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "restart-upstream-model",
		Status:         dbmodel.VideoTaskSubmitted,
		MediaKind:      VideoMediaKindEdits,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}
	if _, ok := videoTaskOwner("vid_edit_db"); ok {
		t.Fatal("memory must be empty for this test to prove the row path")
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "vid_edit_db"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/edits/vid_edit_db", nil)
	c.Set("api_key_id", 0)
	c.Set("user_id", videoTestOwnerUserID)
	c.Set("request_ip", "127.0.0.1")

	VideoPollHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("poll after restart = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := pollPath
	mu.Unlock()
	if got != "/v1/videos/edits/vid_edit_db" {
		t.Fatalf("upstream poll path after restart = %q, want /v1/videos/edits/vid_edit_db (the row's media_kind was lost)", got)
	}
}

// 【走真实调用路径·取成品】content 走同一条 kind，并且必须**流式**透传：
// 上游给一个超过 videoResponseBodyLimit（8MB）的二进制，客户端必须一个字节不少地拿到。
// 这里刻意不去看 videoForward 那条「读进内存 + 8MB 截断」的路径 —— 套上去就是坏文件。
func TestVideoContentHandlerStreamsUpstreamContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	// 比 8MB 的 JSON 上限更大：任何「先 ReadAll 再写」的实现都会在这里截断。
	payload := bytes.Repeat([]byte("mp4payload"), (9<<20)/len("mp4payload")+1)

	var (
		mu          sync.Mutex
		contentPath string
		contentAuth string
		contentHits int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		contentPath = r.URL.Path
		contentAuth = r.Header.Get("Authorization")
		contentHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-content-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "content-upstream-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "content-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_content_1",
		OwnerUserID:    videoTestOwnerUserID,
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "content-upstream-model",
		Status:         dbmodel.VideoTaskSubmitted,
		MediaKind:      VideoMediaKindEdits,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}

	// 归属内存清空：顺带证明 content 也走「内存 → 库」的两级查找。
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "vid_content_1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/edits/vid_content_1/content", nil)
	c.Set("api_key_id", 0)
	c.Set("user_id", videoTestOwnerUserID)
	c.Set("request_ip", "127.0.0.1")

	VideoContentHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("content = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content type = %q, want the upstream one (video/mp4)", ct)
	}
	if got, want := rec.Body.Len(), len(payload); got != want {
		t.Fatalf("streamed %d bytes, want %d: the content was truncated (the 8MB JSON ceiling must not apply here)", got, want)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("streamed body differs from the upstream body")
	}

	mu.Lock()
	gotPath, gotAuth, hits := contentPath, contentAuth, contentHits
	mu.Unlock()
	if gotPath != "/v1/videos/edits/vid_content_1/content" {
		t.Fatalf("upstream content path = %q, want /v1/videos/edits/vid_content_1/content", gotPath)
	}
	if gotAuth != "Bearer content-secret" {
		t.Fatalf("content used the wrong key: %q", gotAuth)
	}
	if hits != 1 {
		t.Fatalf("upstream content fetched %d time(s), want 1", hits)
	}

	// 取成品不计费，但必须留下一条日志痕迹（否则用户拿不到成品时查不到任何东西）。
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	found := false
	for i := range logs {
		if logs[i].RequestEndpoint == "videos_content" {
			found = true
			if logs[i].FinalOutputCost != 0 || logs[i].FinalInputCost != 0 || logs[i].Cost != 0 {
				t.Fatalf("content relay log carries a cost (in=%v out=%v total=%v): fetching the result must not bill twice",
					logs[i].FinalInputCost, logs[i].FinalOutputCost, logs[i].Cost)
			}
		}
	}
	if !found {
		t.Fatalf("no videos_content relay log was written: %+v", logs)
	}
}

// 取成品也要过同一套租户校验：越权探针连上游都不该看见。
func TestVideoContentHandlerRejectsForeignTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	var (
		mu   sync.Mutex
		hits int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-content-tenant",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "content-tenant-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "content-tenant-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_content_tenant",
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "content-tenant-model",
		Status:         dbmodel.VideoTaskSubmitted,
		OwnerTokenID:   42,
		OwnerUserID:    7,
		MediaKind:      VideoMediaKindGenerations,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}

	fetch := func(tokenID, userID int) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = []gin.Param{{Key: "id", Value: "vid_content_tenant"}}
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/generations/vid_content_tenant/content", nil)
		c.Set("api_key_id", tokenID)
		c.Set("user_id", userID)
		c.Set("request_ip", "127.0.0.1")
		VideoContentHandler(c)
		return rec
	}

	if rec := fetch(999, 999); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign content fetch = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	mu.Lock()
	hitsAfterForeign := hits
	mu.Unlock()
	if hitsAfterForeign != 0 {
		t.Fatalf("a foreign content fetch reached the upstream %d time(s)", hitsAfterForeign)
	}

	rec := fetch(42, 7)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner content fetch = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// 上游对成品回非 2xx 时，客户端拿到的是上游的错误（而不是一个空 200）。
func TestVideoContentHandlerPassesUpstreamErrorThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such video"}}`))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-content-error",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "content-error-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "content-error-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_content_missing",
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "content-error-model",
		Status:         dbmodel.VideoTaskSubmitted,
		MediaKind:      VideoMediaKindExtensions,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "vid_content_missing"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/extensions/vid_content_missing/content", nil)
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	VideoContentHandler(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("content with an upstream 404 = %d (%s), want 404", rec.Code, rec.Body.String())
	}
}

// VideoTaskPollStatus（后台轮询）也必须按 kind 走：它是进程重启后唯一的推进者，
// 签名里少了 kind 就让 edits 任务在第一轮被上游 404 判死。
func TestVideoTaskPollStatusUsesMediaKind(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want string
	}{
		{kind: "", want: "/v1/videos/vid_poll_1"},
		{kind: VideoMediaKindGenerations, want: "/v1/videos/generations/vid_poll_1"},
		{kind: VideoMediaKindEdits, want: "/v1/videos/edits/vid_poll_1"},
		{kind: VideoMediaKindExtensions, want: "/v1/videos/extensions/vid_poll_1"},
	} {
		t.Run("kind="+tc.kind, func(t *testing.T) {
			var (
				mu   sync.Mutex
				path string
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				path = r.URL.Path
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"vid_poll_1","status":"in_progress"}`))
			}))
			t.Cleanup(upstream.Close)

			channel := &dbmodel.Channel{
				Name:     "video-poll-status-" + tc.kind,
				Type:     outbound.OutboundTypeOpenAIChat,
				Enabled:  true,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
			}
			status, _, err := VideoTaskPollStatus(context.Background(), channel, "poll-secret", "vid_poll_1", tc.kind)
			if err != nil {
				t.Fatalf("poll status failed: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			mu.Lock()
			got := path
			mu.Unlock()
			if got != tc.want {
				t.Fatalf("background poll path = %q, want %q", got, tc.want)
			}
		})
	}
}
