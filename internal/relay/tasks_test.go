package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 统一任务接口走真实处理器 + httptest 假上游（视频 / mj-proxy），不碰任何真实上游。

func newTaskContext(method, target, body string, apiKeyID, userID int, families []dbmodel.APIKeyEndpointFamily) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	c.Request = req
	c.Set("api_key_id", apiKeyID)
	c.Set("user_id", userID)
	c.Set("request_ip", "127.0.0.1")
	if families != nil {
		c.Set(APIKeyEndpointFamiliesContextKey, families)
	}
	return c, rec
}

func allFamilies() []dbmodel.APIKeyEndpointFamily { return dbmodel.AllAPIKeyEndpointFamilies() }

func taskGet(id, query string, apiKeyID, userID int, families []dbmodel.APIKeyEndpointFamily) *httptest.ResponseRecorder {
	target := "/v1/tasks/" + id
	if query != "" {
		target += "?" + query
	}
	c, rec := newTaskContext(http.MethodGet, target, "", apiKeyID, userID, families)
	c.Params = gin.Params{{Key: "key", Value: id}}
	TaskGetHandler(c)
	return rec
}

func taskList(query string, apiKeyID, userID int, families []dbmodel.APIKeyEndpointFamily) *httptest.ResponseRecorder {
	c, rec := newTaskContext(http.MethodGet, "/v1/tasks?"+query, "", apiKeyID, userID, families)
	TaskListHandler(c)
	return rec
}

func decodeTaskView(t *testing.T, rec *httptest.ResponseRecorder) TaskView {
	t.Helper()
	var v TaskView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode task view: %v (%s)", err, rec.Body.String())
	}
	return v
}

// waitTaskLog 等一条指定端点 + 模型的日志（整包跑时列表里还有别的用例留下的行，不能取 logs[0]）。
func waitTaskLog(t *testing.T, ctx context.Context, endpoint, modelName string) dbmodel.RelayLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := op.RelayLogList(ctx, nil, nil, 1, 200, nil)
		if err == nil {
			for _, l := range logs {
				if l.RequestEndpoint == endpoint && l.RequestModelName == modelName {
					return l
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s log for model %s", endpoint, modelName)
	return dbmodel.RelayLog{}
}

func createTaskTestUser(t *testing.T, ctx context.Context, name string, balance float64) dbmodel.User {
	t.Helper()
	u, err := op.UserCreate(dbmodel.UserCreateRequest{Username: name, Password: "test-password-123", Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: balance}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// 视频提交：委托 VideosHandlerKind（计费、日志、落库都是原路径的），回包换成统一任务对象；
// 之后按 id 查、new-api 风格的 /v1/video/generations/:id 查，都只对创建者可见。
func TestTaskSubmitVideoDelegatesBillsAndIsOwnerScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	var upstreamPath, upstreamQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath, upstreamQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_unified_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)
	const modelName = "unified-video-model"
	ch := dbmodel.Channel{Name: "unified-video", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: modelName, Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "video-secret"}}}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	const perCall = 0.3
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{PerCall: perCall})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })
	user := createTaskTestUser(t, ctx, "unified-video-user", 5)
	other := createTaskTestUser(t, ctx, "unified-video-other", 5)

	c, rec := newTaskContext(http.MethodPost, "/v1/tasks/video-generations", `{"model":"`+modelName+`","prompt":"waves"}`, 0, user.ID, allFamilies())
	c.Params = gin.Params{{Key: "key", Value: "video-generations"}}
	TaskSubmitHandler(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status %d body %s", rec.Code, rec.Body.String())
	}
	v := decodeTaskView(t, rec)
	if v.ID != "vid_unified_1" || v.TaskID != "vid_unified_1" || v.Object != "task" || v.Kind != TaskKindVideo || v.Status != TaskStatusSubmitted || v.Model != modelName || v.CreatedAt == 0 {
		t.Fatalf("unexpected submit view: %+v", v)
	}
	if v.Action != VideoMediaKindGenerations {
		t.Fatalf("action = %q, want %q", v.Action, VideoMediaKindGenerations)
	}
	if upstreamPath != "/v1/videos/generations" || upstreamQuery != "" {
		t.Fatalf("upstream got %q?%q, want /v1/videos/generations", upstreamPath, upstreamQuery)
	}
	after, _ := op.UserGet(user.ID)
	if charged := user.Balance - after.Balance; charged < perCall-1e-9 || charged > perCall+1e-9 {
		t.Fatalf("charged %v, want per_call %v", charged, perCall)
	}
	waitTaskLog(t, ctx, "videos", modelName)

	// 查询：创建者能看到，别的用户 404（new-api 形状）。
	if rec := taskGet("vid_unified_1", "", 0, user.ID, allFamilies()); rec.Code != http.StatusOK || decodeTaskView(t, rec).Kind != TaskKindVideo {
		t.Fatalf("owner fetch: %d %s", rec.Code, rec.Body.String())
	}
	rec = taskGet("vid_unified_1", "", 0, other.ID, allFamilies())
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"Task not found"`) {
		t.Fatalf("foreign fetch must be 404 Task not found: %d %s", rec.Code, rec.Body.String())
	}
	// 只开 openai-compatible 的 key 看不到视频任务。
	if rec := taskGet("vid_unified_1", "", 0, user.ID, []dbmodel.APIKeyEndpointFamily{dbmodel.APIKeyEndpointFamilyOpenAICompatible}); rec.Code != http.StatusNotFound {
		t.Fatalf("video task visible without video family: %d", rec.Code)
	}

	// 后台轮询（VideoTaskStatusCAS）写成成功后，统一视图跟着变。
	row, _ := op.VideoTaskGetByUpstreamID(ctx, "vid_unified_1")
	if ok, err := op.VideoTaskStatusCAS(ctx, row.ID, row.Status, dbmodel.VideoTaskSuccess, map[string]any{"result_url": "https://cdn.example/v.mp4", "finish_time": int64(1700000100)}); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	v = decodeTaskView(t, taskGet("vid_unified_1", "kind=video", 0, user.ID, allFamilies()))
	if v.Status != TaskStatusSuccess || v.ResultURL != "https://cdn.example/v.mp4" || v.FinishedAt != 1700000100 || v.Progress != "100%" {
		t.Fatalf("view after success: %+v", v)
	}
}

// 原生端点报错（模型不存在）与 mj 未受理：状态码与正文原样透传，不包装、不记归属。
func TestTaskSubmitPassesNativeErrorsThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetVideoTaskMemory(t)
	resetMJMemory(t)

	native, nrec := newTaskContext(http.MethodPost, "/v1/videos", `{"model":"no-such-model","prompt":"x"}`, 0, 1, allFamilies())
	VideosHandlerKind(native, "")
	c, rec := newTaskContext(http.MethodPost, "/v1/tasks/video", `{"model":"no-such-model","prompt":"x"}`, 0, 1, allFamilies())
	c.Params = gin.Params{{Key: "key", Value: "video"}}
	TaskSubmitHandler(c)
	if rec.Code != nrec.Code || rec.Body.String() != nrec.Body.String() {
		t.Fatalf("error not passed through: unified %d %s, native %d %s", rec.Code, rec.Body.String(), nrec.Code, nrec.Body.String())
	}
	if nrec.Code < 400 {
		t.Fatalf("expected native error, got %d", nrec.Code)
	}

	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":4,"description":"banned prompt","result":""}`))
	})
	createMJChannel(t, ctx, "mj-reject", up.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
	c, rec = newTaskContext(http.MethodPost, "/v1/tasks/mj", `{"prompt":"x"}`, 0, 1, allFamilies())
	c.Params = gin.Params{{Key: "key", Value: "mj"}}
	TaskSubmitHandler(c)
	if !strings.Contains(rec.Body.String(), "banned prompt") || strings.Contains(rec.Body.String(), `"object":"task"`) {
		t.Fatalf("rejected mj submit must pass through: %d %s", rec.Code, rec.Body.String())
	}

	// 不认识的类型 / mode。
	for _, tc := range []struct{ key, target string }{{"music", "/v1/tasks/music"}, {"mj", "/v1/tasks/mj?mode=nope"}} {
		c, rec := newTaskContext(http.MethodPost, tc.target, `{}`, 0, 1, allFamilies())
		c.Params = gin.Params{{Key: "key", Value: tc.key}}
		TaskSubmitHandler(c)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s: %d %s", tc.target, rec.Code, rec.Body.String())
		}
	}
}

// MJ：?mode= 选上游提交路径且不被转发到上游；查询走任务所属渠道的 /mj/task/:id/fetch，
// 状态 / 进度 / 成品地址换算成统一视图，并按 MJ 查询的口径记一条日志。
func TestTaskSubmitMidjourneyModeAndFetch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	var submitQuery string
	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		switch {
		case strings.HasPrefix(call.path, "/mj/submit/"):
			submitQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":"mj-unified-1"}`))
		case call.path == "/mj/task/mj-unified-1/fetch":
			_, _ = w.Write([]byte(`{"id":"mj-unified-1","action":"BLEND","status":"SUCCESS","progress":"100%","imageUrl":"https://cdn.example/mj.png","submitTime":1700000000000,"finishTime":1700000060000}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	createMJChannel(t, ctx, "mj-unified", up.srv.URL, []string{"mj-key"}, "mj_blend", 1, outbound.OutboundTypeOpenAIChat)

	c, rec := newTaskContext(http.MethodPost, "/v1/tasks/midjourney?mode=blend", `{"base64Array":["data:image/png;base64,AAAA","data:image/png;base64,BBBB"]}`, 0, 7, allFamilies())
	c.Params = gin.Params{{Key: "key", Value: "midjourney"}}
	TaskSubmitHandler(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	v := decodeTaskView(t, rec)
	if v.ID != "mj-unified-1" || v.Kind != TaskKindMidjourney || v.Status != TaskStatusSubmitted || v.Model != "mj_blend" || v.Action != "blend" {
		t.Fatalf("unexpected submit view: %+v", v)
	}
	if calls := up.Calls(); len(calls) != 1 || calls[0].path != "/mj/submit/blend" {
		t.Fatalf("upstream calls: %+v", calls)
	}
	if submitQuery != "" {
		t.Fatalf("mode= leaked to upstream: %q", submitQuery)
	}

	rec = taskGet("mj-unified-1", "kind=mj", 0, 7, allFamilies())
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body.String())
	}
	v = decodeTaskView(t, rec)
	if v.Status != TaskStatusSuccess || v.Progress != "100%" || v.ResultURL != "https://cdn.example/mj.png" || v.SubmitAt != 1700000000 || v.FinishedAt != 1700000060 {
		t.Fatalf("unexpected fetched view: %+v", v)
	}
	waitTaskLog(t, ctx, midjourneyTaskLogName, "mj_blend")
	if rec := taskGet("mj-unified-1", "", 0, 8, allFamilies()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign MJ fetch: %d", rec.Code)
	}
	if rec := taskGet("mj-unified-1", "", 0, 7, []dbmodel.APIKeyEndpointFamily{dbmodel.APIKeyEndpointFamilyVideo}); rec.Code != http.StatusNotFound {
		t.Fatalf("MJ visible with video-only key: %d", rec.Code)
	}
	if rec := taskGet("mj-unified-1", "kind=bogus", 0, 7, allFamilies()); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus kind: %d", rec.Code)
	}
}

// 异步图片：委托 ImagesAsyncSubmit（开关关闭时原样 404 透传），开启后回统一任务对象。
func TestTaskSubmitImageDelegatesToAsyncImages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	ch := dbmodel.Channel{Name: "unified-image", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: "unified-image-model", Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: "http://127.0.0.1:1"}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "img-secret"}}}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	owner := createTaskTestUser(t, ctx, "unified-image-user", 5)
	other := createTaskTestUser(t, ctx, "unified-image-other", 5)
	submit := func() *httptest.ResponseRecorder {
		c, rec := newTaskContext(http.MethodPost, "/v1/tasks/image", `{"model":"unified-image-model","prompt":"a fox"}`, 0, owner.ID, allFamilies())
		c.Params = gin.Params{{Key: "key", Value: "image"}}
		TaskSubmitHandler(c)
		return rec
	}
	if err := op.SettingSetString(dbmodel.SettingKeyAsyncImageEnabled, "false"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	if rec := submit(); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), `"object":"task"`) {
		t.Fatalf("disabled async images must pass the native 404 through: %d %s", rec.Code, rec.Body.String())
	}
	if err := op.SettingSetString(dbmodel.SettingKeyAsyncImageEnabled, "true"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	t.Cleanup(func() { _ = op.SettingSetString(dbmodel.SettingKeyAsyncImageEnabled, "false") })
	// 异步生图对没配价格的模型直接拒绝（同 /v1/images/generations?async=true），统一入口也一样。
	price.SetLLMPrice("unified-image-model", dbmodel.LLMPrice{})
	if rec := submit(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no price configured") {
		t.Fatalf("unpriced model must be rejected for async image tasks: %d %s", rec.Code, rec.Body.String())
	}
	price.SetLLMPrice("unified-image-model", dbmodel.LLMPrice{Input: 5, Output: 40})
	t.Cleanup(func() { price.SetLLMPrice("unified-image-model", dbmodel.LLMPrice{}) })
	rec := submit()
	if rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	v := decodeTaskView(t, rec)
	if !strings.HasPrefix(v.ID, "imgtask_") || v.Kind != TaskKindImage || v.Status != TaskStatusInProgress || v.Model != "unified-image-model" || v.Action != dbmodel.ImageTaskEndpointGenerations {
		t.Fatalf("unexpected image submit view: %+v", v)
	}
	if rec := taskGet(v.ID, "", 0, owner.ID, allFamilies()); rec.Code != http.StatusOK {
		t.Fatalf("owner fetch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := taskGet(v.ID, "", 0, other.ID, allFamilies()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign image fetch: %d", rec.Code)
	}
}

// 列表：三张表按归属归并、按 created_at 倒序分页；端点族、kind、status 过滤；MJ 状态按页批量问上游。
func TestTaskListMergesOwnedTasksAcrossKinds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		if call.path != "/mj/task/list-by-condition" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ids, _ := call.body["ids"].([]any)
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "status": "IN_PROGRESS", "progress": "40%"})
		}
		b, _ := json.Marshal(out)
		_, _ = w.Write(b)
	})
	mjCh := createMJChannel(t, ctx, "mj-list", up.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
	reloaded, _ := op.ChannelGet(mjCh.ID, ctx)

	const token, user = 5, 1
	now := time.Now().Unix()
	videos := []dbmodel.VideoTask{
		{UpstreamTaskID: "v-mine", OwnerTokenID: token, OwnerUserID: user, Status: dbmodel.VideoTaskSuccess, CreatedAt: 1000, ResultURL: "https://cdn/v"},
		{UpstreamTaskID: "v-legacy", OwnerTokenID: 0, OwnerUserID: user, Status: dbmodel.VideoTaskQueued, CreatedAt: 900},
		{UpstreamTaskID: "v-other-key", OwnerTokenID: 6, OwnerUserID: user, Status: dbmodel.VideoTaskSuccess, CreatedAt: 1100},
		{UpstreamTaskID: "v-unowned", Status: dbmodel.VideoTaskSuccess, CreatedAt: 1200},
	}
	for i := range videos {
		if err := op.VideoTaskCreate(ctx, &videos[i]); err != nil {
			t.Fatalf("video row: %v", err)
		}
	}
	images := []dbmodel.ImageTask{
		{TaskID: "imgtask_mine", OwnerTokenID: token, OwnerUserID: user, Status: dbmodel.ImageTaskStatusFailed, ErrorMessage: "upstream error", CreatedAt: 950, CompletedAt: 960, ExpiresAt: now + 3600},
		{TaskID: "imgtask_expired", OwnerTokenID: token, OwnerUserID: user, Status: dbmodel.ImageTaskStatusCompleted, CreatedAt: 1300, ExpiresAt: now - 1},
		{TaskID: "imgtask_other", OwnerTokenID: 6, OwnerUserID: user, Status: dbmodel.ImageTaskStatusCompleted, CreatedAt: 1400, ExpiresAt: now + 3600},
	}
	for i := range images {
		if err := op.ImageTaskCreate(ctx, &images[i]); err != nil {
			t.Fatalf("image row: %v", err)
		}
	}
	mjRows := []dbmodel.MidjourneyTask{
		{UpstreamTaskID: "mj-mine", ChannelID: reloaded.ID, ChannelKeyID: reloaded.Keys[0].ID, Model: "mj_imagine", Action: "IMAGINE", OwnerTokenID: token, OwnerUserID: user, CreatedAt: 1050},
		{UpstreamTaskID: "mj-other", ChannelID: reloaded.ID, ChannelKeyID: reloaded.Keys[0].ID, Model: "mj_imagine", Action: "IMAGINE", OwnerTokenID: 6, OwnerUserID: user, CreatedAt: 1500},
	}
	for i := range mjRows {
		if err := op.MidjourneyTaskCreate(ctx, &mjRows[i]); err != nil {
			t.Fatalf("mj row: %v", err)
		}
	}

	decode := func(rec *httptest.ResponseRecorder) taskListResponse {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		var out taskListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out
	}
	ids := func(l taskListResponse) string {
		var s []string
		for _, v := range l.Data {
			s = append(s, v.ID)
		}
		return strings.Join(s, ",")
	}

	all := decode(taskList("", token, user, allFamilies()))
	if got := ids(all); got != "mj-mine,v-mine,imgtask_mine,v-legacy" || all.Total != 4 || all.Object != "list" || all.HasMore {
		t.Fatalf("list = %q total=%d has_more=%v", got, all.Total, all.HasMore)
	}
	if all.Data[0].Status != TaskStatusInProgress || all.Data[0].Progress != "40%" {
		t.Fatalf("MJ status not filled from upstream: %+v", all.Data[0])
	}
	if all.Data[2].Status != TaskStatusFailure || all.Data[2].FailReason != "upstream error" || all.Data[2].FinishedAt != 960 {
		t.Fatalf("image view: %+v", all.Data[2])
	}
	if calls := up.Calls(); len(calls) != 1 {
		t.Fatalf("expected one batched MJ upstream call, got %d", len(calls))
	}

	page2 := decode(taskList("page=2&page_size=3", token, user, allFamilies()))
	if got := ids(page2); got != "v-legacy" || page2.Total != 4 || page2.HasMore {
		t.Fatalf("page 2 = %q total=%d has_more=%v", got, page2.Total, page2.HasMore)
	}
	page1 := decode(taskList("page=1&page_size=3", token, user, allFamilies()))
	if !page1.HasMore {
		t.Fatal("page 1 must report has_more")
	}
	if got := ids(decode(taskList("", token, user, []dbmodel.APIKeyEndpointFamily{dbmodel.APIKeyEndpointFamilyVideo}))); got != "v-mine,v-legacy" {
		t.Fatalf("video-only key list = %q", got)
	}
	if got := ids(decode(taskList("kind=image", token, user, allFamilies()))); got != "imgtask_mine" {
		t.Fatalf("kind=image list = %q", got)
	}
	if got := ids(decode(taskList("kind=video", token, user, []dbmodel.APIKeyEndpointFamily{dbmodel.APIKeyEndpointFamilyOpenAICompatible}))); got != "" {
		t.Fatalf("kind=video without video family must be empty, got %q", got)
	}
	if got := ids(decode(taskList("status=SUCCESS", token, user, allFamilies()))); got != "v-mine" {
		t.Fatalf("status=SUCCESS list = %q", got)
	}
	if got := ids(decode(taskList("status=failure", token, user, allFamilies()))); got != "imgtask_mine" {
		t.Fatalf("status=failure list = %q", got)
	}
	// 未带族信息（不该发生）= 一个都不给。
	if got := ids(decode(taskList("", token, user, nil))); got != "" {
		t.Fatalf("missing family context must list nothing, got %q", got)
	}
	for _, q := range []string{"page=0", "page_size=abc", "kind=music", "status=DONE", "page=100&page_size=100"} {
		if rec := taskList(q, token, user, allFamilies()); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, rec.Code)
		}
	}
	// 另一把 key（6）看到的是它自己的 + 同用户的存量无 token 行。
	if got := ids(decode(taskList("", 6, user, allFamilies()))); got != "mj-other,imgtask_other,v-other-key,v-legacy" {
		t.Fatalf("token 6 list = %q", got)
	}
}

// new-api 风格的视频查询：{"code":"success","data":…}；查不到 400 task_not_exist。
func TestVideoGenerationGetUsesNewAPIEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	row := dbmodel.VideoTask{UpstreamTaskID: "vid-envelope", OwnerTokenID: 3, OwnerUserID: 2, Status: dbmodel.VideoTaskInProgress, Progress: "30%", Model: "m", CreatedAt: 1234}
	if err := op.VideoTaskCreate(ctx, &row); err != nil {
		t.Fatalf("row: %v", err)
	}
	get := func(tokenID, userID int) *httptest.ResponseRecorder {
		c, rec := newTaskContext(http.MethodGet, "/v1/video/generations/vid-envelope", "", tokenID, userID, allFamilies())
		c.Params = gin.Params{{Key: "task_id", Value: "vid-envelope"}}
		VideoGenerationGetHandler(c)
		return rec
	}
	rec := get(3, 2)
	var out struct {
		Code string         `json:"code"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Code != "success" || out.Data["task_id"] != "vid-envelope" || out.Data["status"] != TaskStatusInProgress || out.Data["progress"] != "30%" {
		t.Fatalf("envelope: %d %s", rec.Code, rec.Body.String())
	}
	rec = get(4, 2)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"task_not_exist"`) {
		t.Fatalf("foreign: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTaskStatusMappingsMatchNewAPIEnum(t *testing.T) {
	cases := map[dbmodel.VideoTaskStatus]string{
		dbmodel.VideoTaskNotStart: TaskStatusNotStart, dbmodel.VideoTaskSubmitted: TaskStatusSubmitted, dbmodel.VideoTaskQueued: TaskStatusQueued,
		dbmodel.VideoTaskInProgress: TaskStatusInProgress, dbmodel.VideoTaskSuccess: TaskStatusSuccess, dbmodel.VideoTaskFailure: TaskStatusFailure,
		dbmodel.VideoTaskUnknown: TaskStatusUnknown, "": TaskStatusNotStart,
	}
	for in, want := range cases {
		if got := videoTaskUnifiedStatus(in); got != want {
			t.Fatalf("video %q -> %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"processing": TaskStatusInProgress, "completed": TaskStatusSuccess, "failed": TaskStatusFailure} {
		if got := imageTaskUnifiedStatus(in); got != want {
			t.Fatalf("image %q -> %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"SUCCESS": TaskStatusSuccess, "FAILURE": TaskStatusFailure, "CANCEL": TaskStatusFailure, "MODAL": TaskStatusInProgress, "SUBMITTED": TaskStatusSubmitted, "": TaskStatusUnknown} {
		if got := midjourneyUnifiedStatus(in); got != want {
			t.Fatalf("mj %q -> %q, want %q", in, got, want)
		}
	}
	if mjSeconds(1700000000123) != 1700000000 || mjSeconds(1700000000) != 1700000000 {
		t.Fatal("mjSeconds")
	}
}
