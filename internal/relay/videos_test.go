package relay

import (
	"encoding/json"
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

func TestExtractVideoTaskIDs(t *testing.T) {
	// task_id appears twice (also as id), video_id once -> two unique ids.
	ids := extractVideoTaskIDs([]byte(`{"id":"task_1","task_id":"task_1","video_id":"video_9","status":"queued"}`))
	if len(ids) != 2 {
		t.Fatalf("expected 2 unique ids, got %d: %v", len(ids), ids)
	}
	want := map[string]bool{"task_1": true, "video_9": true}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %q in %v", id, ids)
		}
	}

	if got := extractVideoTaskIDs([]byte(`not json`)); got != nil {
		t.Fatalf("expected nil for invalid json, got %v", got)
	}
	if got := extractVideoTaskIDs([]byte(`{"status":"queued"}`)); got != nil {
		t.Fatalf("expected nil when no ids present, got %v", got)
	}
}

func TestVideoTaskOwnerRoundTrip(t *testing.T) {
	videoTaskStore.Lock()
	videoTaskStore.items = make(map[string]videoTaskEntry)
	videoTaskStore.lastPruneAt = time.Time{}
	videoTaskStore.Unlock()

	recordVideoTaskOwner([]string{"video_abc", "task_abc"}, 7, 3, "agnes-video-v2.0", 42, 100, "")

	entry, ok := videoTaskOwner("video_abc")
	if !ok {
		t.Fatal("expected owner for video_abc")
	}
	if entry.channelID != 7 || entry.channelKeyID != 3 || entry.model != "agnes-video-v2.0" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	// alias id resolves to the same owner (create returned multiple ids).
	if e2, ok := videoTaskOwner("task_abc"); !ok || e2.channelID != 7 {
		t.Fatalf("alias id lookup failed: %+v ok=%v", e2, ok)
	}
	if _, ok := videoTaskOwner("nope"); ok {
		t.Fatal("expected miss for unknown id")
	}
}

func TestVideoTaskOwnerExpires(t *testing.T) {
	videoTaskStore.Lock()
	videoTaskStore.items = map[string]videoTaskEntry{
		"stale": {channelID: 1, channelKeyID: 1, expiresAt: time.Now().Add(-time.Minute)},
	}
	videoTaskStore.lastPruneAt = time.Time{}
	videoTaskStore.Unlock()

	if _, ok := videoTaskOwner("stale"); ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestVideoTaskOwnerMatches(t *testing.T) {
	if !videoTaskOwnerMatches(videoTaskEntry{}, 1, 2) {
		t.Fatal("0/0 owner should be unrestricted")
	}
	tokenScoped := videoTaskEntry{ownerTokenID: 5}
	if !videoTaskOwnerMatches(tokenScoped, 5, 0) {
		t.Fatal("same token should match")
	}
	if videoTaskOwnerMatches(tokenScoped, 6, 0) {
		t.Fatal("different token must not match")
	}
	userScoped := videoTaskEntry{ownerUserID: 9}
	if !videoTaskOwnerMatches(userScoped, 0, 9) {
		t.Fatal("same user should match")
	}
	if videoTaskOwnerMatches(userScoped, 0, 10) {
		t.Fatal("different user must not match")
	}
}

// 1. Client alias經 channel.ModelMapping 后，上游 video create/status/poll 使用 mapped model
// 同时校验 attempts 记录的 channel 和 model 正确
func TestVideosHandlerModelMappingCreateAndPoll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu             sync.Mutex
		createGotPath  string
		createGotAuth  string
		createGotModel string
		pollGotPath    string
		pollGotAuth    string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/videos" {
			createGotPath = r.URL.Path
			createGotAuth = r.Header.Get("Authorization")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upstream create body: %v", err)
			}
			createGotModel, _ = body["model"].(string)

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"vid_task_123","status":"queued"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/videos/vid_task_123" {
			pollGotPath = r.URL.Path
			pollGotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"vid_task_123","status":"completed","video_url":"https://example.com/v.mp4"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:    "video-channel-1",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "upstream-sora-v2",
		ModelMapping: map[string]string{
			"alias-sora": "upstream-sora-v2",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sora-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 1.1 Create video via VideosHandler
	recCreate := httptest.NewRecorder()
	cCreate, _ := gin.CreateTestContext(recCreate)
	reqCreate := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{
		"model":"alias-sora",
		"prompt":"generate a sunset video"
	}`))
	reqCreate.Header.Set("Content-Type", "application/json")
	cCreate.Request = reqCreate
	cCreate.Set("api_key_id", 0)
	cCreate.Set("user_id", 0)
	cCreate.Set("request_ip", "127.0.0.1")

	VideosHandler(cCreate)

	if recCreate.Code != http.StatusOK {
		t.Fatalf("expected video create to succeed, got %d body %s", recCreate.Code, recCreate.Body.String())
	}

	mu.Lock()
	cPath, cAuth, cModel := createGotPath, createGotAuth, createGotModel
	mu.Unlock()

	if cPath != "/v1/videos" {
		t.Fatalf("unexpected upstream create path: %s", cPath)
	}
	if cAuth != "Bearer sora-secret-key" {
		t.Fatalf("unexpected upstream auth header: %s", cAuth)
	}
	if cModel != "upstream-sora-v2" {
		t.Fatalf("expected upstream create model remapped to upstream-sora-v2, got %q", cModel)
	}

	// Check owner entry was recorded with mapped model
	entry, ok := videoTaskOwner("vid_task_123")
	if !ok {
		t.Fatalf("expected video task owner recorded for vid_task_123")
	}
	if entry.model != "upstream-sora-v2" {
		t.Fatalf("expected videoTaskEntry model to be upstream-sora-v2, got %q", entry.model)
	}
	if entry.channelID != ch.ID {
		t.Fatalf("expected videoTaskEntry channelID %d, got %d", ch.ID, entry.channelID)
	}

	// 1.2 Poll video via VideoPollHandler
	recPoll := httptest.NewRecorder()
	cPoll, _ := gin.CreateTestContext(recPoll)
	cPoll.Params = []gin.Param{{Key: "id", Value: "vid_task_123"}}
	reqPoll := httptest.NewRequest(http.MethodGet, "/v1/videos/vid_task_123", nil)
	cPoll.Request = reqPoll
	cPoll.Set("api_key_id", 0)
	cPoll.Set("user_id", 0)
	cPoll.Set("request_ip", "127.0.0.1")

	VideoPollHandler(cPoll)

	if recPoll.Code != http.StatusOK {
		t.Fatalf("expected video poll to succeed, got %d body %s", recPoll.Code, recPoll.Body.String())
	}

	mu.Lock()
	pPath, pAuth := pollGotPath, pollGotAuth
	mu.Unlock()

	if pPath != "/v1/videos/vid_task_123" {
		t.Fatalf("unexpected upstream poll path: %s", pPath)
	}
	if pAuth != "Bearer sora-secret-key" {
		t.Fatalf("unexpected upstream poll auth: %s", pAuth)
	}

	// 1.3 Verify attempts logged for create
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	// logs[0] is poll or create depending on order, find the create log
	var createLog *dbmodel.RelayLog
	var pollLog *dbmodel.RelayLog
	for i := range logs {
		if logs[i].RequestEndpoint == "videos" {
			createLog = &logs[i]
		} else if logs[i].RequestEndpoint == "videos_poll" {
			pollLog = &logs[i]
		}
	}
	if createLog == nil {
		t.Fatalf("expected videos create log in logs: %+v", logs)
	}
	if createLog.RequestModelName != "alias-sora" {
		t.Fatalf("expected request model alias-sora, got %q", createLog.RequestModelName)
	}
	if createLog.ActualModelName != "upstream-sora-v2" {
		t.Fatalf("expected actual model upstream-sora-v2, got %q", createLog.ActualModelName)
	}
	if len(createLog.Attempts) != 1 {
		t.Fatalf("expected 1 attempt in create log, got %d", len(createLog.Attempts))
	}
	att := createLog.Attempts[0]
	if att.ChannelID != ch.ID || att.ModelName != "upstream-sora-v2" {
		t.Fatalf("attempt channel/model mismatch: id=%d want=%d, model=%q want=upstream-sora-v2", att.ChannelID, ch.ID, att.ModelName)
	}

	if pollLog == nil {
		t.Fatalf("expected videos_poll log")
	}
	if pollLog.ActualModelName != "upstream-sora-v2" {
		t.Fatalf("expected poll log actual model upstream-sora-v2, got %q", pollLog.ActualModelName)
	}
}

// 2. access route 选中失败且 fallback_mode != none 时，回到 enabled channels 动态候选并成功；fallback none 不回退
// 3. attempts 记录 channel/model 正确
func TestVideosHandlerAccessRouteFallbackAndNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	routeFailUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"route video channel error"}}`))
	}))
	t.Cleanup(routeFailUpstream.Close)

	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fallback_vid_123","status":"queued"}`))
	}))
	t.Cleanup(fallbackUpstream.Close)

	// Route channel that will fail
	routeChannel := dbmodel.Channel{
		Name:    "route-video-fail-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "route-video-upstream",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: routeFailUpstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "route-video-key"}},
	}
	if err := op.ChannelCreate(&routeChannel, ctx); err != nil {
		t.Fatalf("create route channel: %v", err)
	}

	// Dynamic fallback channel in enabled channels pool
	fallbackChannel := dbmodel.Channel{
		Name:    "pool-video-fallback-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "pool-video-upstream",
		ModelMapping: map[string]string{
			"test-video-model": "pool-video-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: fallbackUpstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "fallback-video-key"}},
	}
	if err := op.ChannelCreate(&fallbackChannel, ctx); err != nil {
		t.Fatalf("create fallback channel: %v", err)
	}

	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list access plans: %v", err)
	}
	var vip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}

	// 2.1 Test fallback_mode = return_group (or failover): route fails -> falls back to pool -> succeeds
	ruleFallback := dbmodel.AccessRouteRule{
		RouteProfileID: vip.RouteProfileID,
		RequestModel:   "test-video-model",
		FallbackMode:   dbmodel.AccessRouteFallbackReturnGroup,
	}
	if err := op.AccessRouteRuleCreate(&ruleFallback, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	if err := op.AccessRouteTargetCreate(&dbmodel.AccessRouteTarget{
		RouteRuleID:   ruleFallback.ID,
		ChannelID:     routeChannel.ID,
		UpstreamModel: "route-video-upstream",
		Priority:      1,
		Weight:        1,
		Enabled:       true,
	}, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{
		"model":"test-video-model",
		"prompt":"a running cat"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	VideosHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected video fallback to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback_vid_123") {
		t.Fatalf("expected fallback response body, got %s", rec.Body.String())
	}

	// Check attempts: 1st attempt on routeChannel, 2nd attempt on fallbackChannel
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	logEntry := logs[0]
	if logEntry.ActualModelName != "pool-video-upstream" {
		t.Fatalf("expected actual model pool-video-upstream, got %q", logEntry.ActualModelName)
	}
	if logEntry.TotalAttempts != 2 {
		t.Fatalf("expected 2 attempts recorded in log, got %d", logEntry.TotalAttempts)
	}
	if len(logEntry.Attempts) != 2 {
		t.Fatalf("expected 2 attempts in array, got %d", len(logEntry.Attempts))
	}
	// Attempt 1: route channel
	att1 := logEntry.Attempts[0]
	if att1.ChannelID != routeChannel.ID || att1.ModelName != "route-video-upstream" || att1.Status != dbmodel.AttemptFailed {
		t.Fatalf("attempt 1 mismatch: channel=%d model=%q status=%v", att1.ChannelID, att1.ModelName, att1.Status)
	}
	// Attempt 2: fallback channel
	att2 := logEntry.Attempts[1]
	if att2.ChannelID != fallbackChannel.ID || att2.ModelName != "pool-video-upstream" || att2.Status != dbmodel.AttemptSuccess {
		t.Fatalf("attempt 2 mismatch: channel=%d model=%q status=%v", att2.ChannelID, att2.ModelName, att2.Status)
	}

	// 2.2 Test fallback_mode = none: route fails -> no fallback to pool -> returns 500 / error
	ruleFallback.FallbackMode = dbmodel.AccessRouteFallbackNone
	if err := op.AccessRouteRuleUpdate(&ruleFallback, ctx); err != nil {
		t.Fatalf("update route rule to fallback none: %v", err)
	}

	recNone := httptest.NewRecorder()
	cNone, _ := gin.CreateTestContext(recNone)
	reqNone := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{
		"model":"test-video-model",
		"prompt":"a running cat"
	}`))
	reqNone.Header.Set("Content-Type", "application/json")
	cNone.Request = reqNone
	cNone.Set("api_key_id", 0)
	cNone.Set("user_id", 0)
	cNone.Set("request_ip", "127.0.0.1")

	VideosHandler(cNone)

	if recNone.Code == http.StatusOK {
		t.Fatalf("expected fallback none to fail, got status 200 body %s", recNone.Body.String())
	}

	// Check attempts for the failed request: TotalAttempts should be 1 (only the route candidate)
	logsNone, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	failedLog := logsNone[0]
	if failedLog.TotalAttempts != 1 {
		t.Fatalf("expected exactly 1 attempt for fallback none, got %d", failedLog.TotalAttempts)
	}
	if failedLog.Attempts[0].ChannelID != routeChannel.ID {
		t.Fatalf("expected failed attempt channel %d, got %d", routeChannel.ID, failedLog.Attempts[0].ChannelID)
	}
}

// resetVideoTaskMemory 清空进程内的归属内存表：这是「重启」在单进程测试里的等价物。
func resetVideoTaskMemory(t *testing.T) {
	t.Helper()
	videoTaskStore.Lock()
	videoTaskStore.items = make(map[string]videoTaskEntry)
	videoTaskStore.lastPruneAt = time.Time{}
	videoTaskStore.Unlock()
}

// 创建成功后必须同时落一行库（双写），且这一行记的是真正用的渠道/key/映射后模型、
// 创建人与原始请求体——这些字段是重启后能找回归属、能审计/重试的全部依据。
func TestVideosHandlerPersistsVideoTaskRow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_persist_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:         "video-persist-channel",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		Model:        "persist-upstream-model",
		ModelMapping: map[string]string{"alias-persist": "persist-upstream-model"},
		Priority:     1,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "persist-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"alias-persist","prompt":"a slow sunrise"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	// api_key_id 只能是 0：路由选择要求这把 key 在库/缓存里真实存在（AccessPlanSelect
	// 对不存在的 id 直接报 "API key not found"），而这条用例验证的是落库字段而不是路由。
	// token 维度的归属由 TestVideoPollHandlerRejectsForeignTenantFromDB 的 42/7 那行覆盖。
	c.Set("api_key_id", 0)
	c.Set("user_id", 12)
	c.Set("request_ip", "127.0.0.1")

	VideosHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}

	row, err := op.VideoTaskGetByUpstreamID(ctx, "vid_persist_1")
	if err != nil {
		t.Fatalf("read back row: %v", err)
	}
	if row == nil {
		t.Fatal("create did not persist a video_tasks row (a restart would lose the task)")
	}
	if row.ChannelID != ch.ID || row.ChannelKeyID != ch.Keys[0].ID {
		t.Fatalf("ownership mismatch: channel=%d key=%d, want %d/%d", row.ChannelID, row.ChannelKeyID, ch.ID, ch.Keys[0].ID)
	}
	if row.Model != "persist-upstream-model" {
		t.Fatalf("model = %q, want the mapped upstream model", row.Model)
	}
	if row.OwnerUserID != 12 {
		t.Fatalf("tenant mismatch: token=%d user=%d, want user 12", row.OwnerTokenID, row.OwnerUserID)
	}
	if row.Status != dbmodel.VideoTaskSubmitted {
		t.Fatalf("status = %q, want %q", row.Status, dbmodel.VideoTaskSubmitted)
	}
	if row.SubmitTime == 0 {
		t.Fatal("submit_time not recorded")
	}
	if !strings.Contains(string(row.RequestData), "a slow sunrise") {
		t.Fatalf("request_data does not carry the create request: %s", string(row.RequestData))
	}
	// 双写：内存那套还在。
	if _, ok := videoTaskOwner("vid_persist_1"); !ok {
		t.Fatal("in-memory owner was not recorded (dual write expected)")
	}
}

// 【重启场景·核心】进程重启后内存归属全空，只有库里那一行。此时 VideoPollHandler
// 必须仍能找到归属、仍回到原来那个渠道+key 转发，并把上游原始响应一字不改地透传。
// 这条测试是整次改造的存在理由：没有它，重启就等于让客户端重建任务、重付一次钱。
func TestVideoPollHandlerServesTaskFromDBAfterRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	// 模拟重启：进程内归属内存清空。
	resetVideoTaskMemory(t)

	var (
		mu       sync.Mutex
		pollHits int
		pollAuth string
		pollPath string
	)
	upstreamBody := `{"id":"vid_db_1","status":"completed","video_url":"https://example.com/db.mp4"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pollHits++
		pollAuth = r.Header.Get("Authorization")
		pollPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "keep-me")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-db-owner-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "db-video-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "db-owner-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 这是「上一个进程」留下的那一行。
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_db_1",
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "db-video-model",
		OwnerTokenID:   0,
		OwnerUserID:    0,
		Status:         dbmodel.VideoTaskSubmitted,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}
	if _, ok := videoTaskOwner("vid_db_1"); ok {
		t.Fatal("memory must be empty for this test to prove the DB path")
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "vid_db_1"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/vid_db_1", nil)
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	VideoPollHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("poll after restart = %d %s, want 200 from the DB-recorded owner", rec.Code, rec.Body.String())
	}
	// 返回形状不许变：客户端拿到的就是上游原始响应体。
	if rec.Body.String() != upstreamBody {
		t.Fatalf("response body rewritten: got %q want %q", rec.Body.String(), upstreamBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json (upstream)", ct)
	}
	mu.Lock()
	hits, gotAuth, gotPath := pollHits, pollAuth, pollPath
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("upstream polled %d times, want 1", hits)
	}
	if gotAuth != "Bearer db-owner-secret-key" {
		t.Fatalf("poll used the wrong key: %q", gotAuth)
	}
	if gotPath != "/v1/videos/vid_db_1" {
		t.Fatalf("poll path = %q", gotPath)
	}
}

// 【租户隔离】任务落库不等于谁都查得到：库里的 OwnerTokenID/OwnerUserID 与请求方不一致
// 时仍然 404，且绝不能把请求打到上游去（越权探针连上游都不该看见）。
func TestVideoPollHandlerRejectsForeignTenantFromDB(t *testing.T) {
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_tenant_1","status":"completed","video_url":"https://example.com/t.mp4"}`))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-tenant-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "tenant-video-model",
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "tenant-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := op.VideoTaskCreate(ctx, &dbmodel.VideoTask{
		UpstreamTaskID: "vid_tenant_1",
		ChannelID:      ch.ID,
		ChannelKeyID:   ch.Keys[0].ID,
		Model:          "tenant-video-model",
		OwnerTokenID:   42,
		OwnerUserID:    7,
		Status:         dbmodel.VideoTaskSubmitted,
		SubmitTime:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed task row: %v", err)
	}

	poll := func(tokenID, userID int) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = []gin.Param{{Key: "id", Value: "vid_tenant_1"}}
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/vid_tenant_1", nil)
		c.Set("api_key_id", tokenID)
		c.Set("user_id", userID)
		c.Set("request_ip", "127.0.0.1")
		VideoPollHandler(c)
		return rec
	}

	if rec := poll(999, 999); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign tenant poll = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	mu.Lock()
	hitsAfterForeign := hits
	mu.Unlock()
	if hitsAfterForeign != 0 {
		t.Fatalf("a foreign poll reached the upstream %d time(s)", hitsAfterForeign)
	}

	// 反向对照：这条行本身是可服务的，所以上面的 404 只能是因为租户不匹配。
	rec := poll(42, 7)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner poll = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	mu.Lock()
	hitsAtEnd := hits
	mu.Unlock()
	if hitsAtEnd != 1 {
		t.Fatalf("owner poll hit upstream %d times, want 1", hitsAtEnd)
	}
}

// 创建视频时，只有 429 才进下一把 key（relay.shouldTryNextChannelKey 的唯一放行码）；
// 400 是「这个请求本身不对」，换 key 只会把同一个必然失败的、还带花钱副作用的视频创建
// 请求再发一次。
//
// 【牙在「两把 key」上】只配一把 key 时，「错误地重试下一把 key」无处可去，断言永远绿。
// 所以同一个渠道配两把可用 key，并配一条 429 对照用例：429 那一路必须打到两把 key 上，
// 证明它们真的都在 key 循环里可达——否则「400 只收到一次」也可能只是因为第二把 key
// 压根没进候选列表，而不是因为代码拦住了。
func TestVideosHandlerKeyFailoverOnlyOn429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)

	cases := []struct {
		name     string
		status   int
		wantHits int
	}{
		// 400：一次都不许多打，两把 key 里只能有一把被用到。
		{name: "400_does_not_try_the_next_key", status: http.StatusBadRequest, wantHits: 1},
		// 对照：429 是 shouldTryNextChannelKey 唯一放行的码，两把 key 都要被打到。
		{name: "429_tries_the_next_key", status: http.StatusTooManyRequests, wantHits: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				auths []string
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				auths = append(auths, r.Header.Get("Authorization"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected the video request"}}`))
			}))
			t.Cleanup(upstream.Close)

			ch := dbmodel.Channel{
				Name:         "video-two-keys-" + tc.name,
				Type:         outbound.OutboundTypeOpenAIChat,
				Enabled:      true,
				Model:        "two-keys-upstream-model",
				ModelMapping: map[string]string{"alias-two-keys-" + tc.name: "two-keys-upstream-model"},
				Priority:     1,
				BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
				// 两把都可用：第二把是「不该被用到」的那一把。
				Keys: []dbmodel.ChannelKey{
					{Enabled: true, ChannelKey: "two-keys-secret-a"},
					{Enabled: true, ChannelKey: "two-keys-secret-b"},
				},
			}
			if err := op.ChannelCreate(&ch, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}
			// 前置条件：两把 key 都在候选集里，否则这条用例证明不了任何事。
			available := ch.GetAvailableChannelKeys()
			if len(available) != 2 {
				t.Fatalf("precondition failed: channel exposes %d usable key(s), want 2 (%+v)", len(available), available)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/videos",
				strings.NewReader(`{"model":"alias-two-keys-`+tc.name+`","prompt":"a cat"}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			VideosHandler(c)

			mu.Lock()
			gotAuths := append([]string(nil), auths...)
			mu.Unlock()

			if len(gotAuths) != tc.wantHits {
				t.Fatalf("upstream received %d request(s) %v, want %d: status %d must %s",
					len(gotAuths), gotAuths, tc.wantHits, tc.status,
					map[int]string{
						http.StatusBadRequest:      "not be retried on the next key",
						http.StatusTooManyRequests: "be retried on the next key",
					}[tc.status])
			}
			// 上游的错误不许被吞成成功：客户端要看到上游真实的拒绝。
			if rec.Code != tc.status {
				t.Fatalf("client status = %d (%s), want %d passed through", rec.Code, rec.Body.String(), tc.status)
			}
		})
	}
}
