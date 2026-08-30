package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
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

	recordVideoTaskOwner([]string{"video_abc", "task_abc"}, 7, 3, "agnes-video-v2.0", 42, 100)

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
		mu              sync.Mutex
		createGotPath   string
		createGotAuth   string
		createGotModel  string
		pollGotPath     string
		pollGotAuth     string
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
