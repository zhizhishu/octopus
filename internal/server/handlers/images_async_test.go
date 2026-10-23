package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// imageAsyncTestModel 是本用例专用的模型名（价格表是进程级的全局 map，
// 用专用名字才不会与其它用例互相污染）。
const imageAsyncTestModel = "gpt-image-async-handler-test"

// setupImageAsyncEnv 起一套"真 chain"环境：真 DB、真渠道、真 httptest 上游、
// 真路由表。契约第四节第 7 条要求至少一条走 handler 全链路——只测纯函数不算通过。
type imageAsyncEnv struct {
	ctx      context.Context
	engine   *gin.Engine
	upstream *httptest.Server
	channel  model.Channel
	hits     *int
}

func setupImageAsyncEnv(t *testing.T, enabled bool) *imageAsyncEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := setupGeminiModelsDB(t)

	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations" {
			hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1700000000,"data":[{"url":"https://images.upstream.example/a.png"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	channel := model.Channel{
		Name:     "image-async-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    imageAsyncTestModel,
		Priority: 1,
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "image-async-secret"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	value := "false"
	if enabled {
		value = "true"
	}
	if err := op.SettingSetString(model.SettingKeyAsyncImageEnabled, value); err != nil {
		t.Fatalf("set %s=%s: %v", model.SettingKeyAsyncImageEnabled, value, err)
	}
	// 用例之间互相影响：把开关恢复成出厂默认（关）。
	t.Cleanup(func() {
		_ = op.SettingSetString(model.SettingKeyAsyncImageEnabled, "false")
	})

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	if len(engine.Routes()) == 0 {
		t.Fatal("RegisterAll 之后 engine 里一条路由都没有，后面的断言会退化成 404")
	}

	return &imageAsyncEnv{ctx: ctx, engine: engine, upstream: upstream, channel: channel, hits: &hits}
}

func createImageAsyncUserKey(t *testing.T, ctx context.Context, username, secret string, admin bool, balance float64) (model.User, model.APIKey) {
	t.Helper()
	role := model.UserRoleUser
	if admin {
		role = model.UserRoleAdmin
	}
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: username,
		Password: "test-password-123",
		Role:     role,
		Status:   model.UserStatusActive,
		Balance:  balance,
	}, ctx)
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	return user, createVideosFamilyAPIKey(t, ctx, user, username+"-key", secret, nil)
}

// doImageAsyncRequest 走真实 engine（真路由 + 真 APIKeyAuth）。
func doImageAsyncRequest(engine *gin.Engine, method, path, apiKey, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func submitImageAsync(t *testing.T, env *imageAsyncEnv, apiKey, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doImageAsyncRequest(env.engine, http.MethodPost, path, apiKey,
		`{"model":"`+imageAsyncTestModel+`","prompt":"a quiet lake at dawn","n":1,"size":"1024x1024"}`)
	var payload map[string]any
	if rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode submit body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, payload
}

// 契约第四节第 7 条：走 handler 全链路，断言 202 与 poll_url 正确，
// 并且任务行落库（归属 / 渠道 / 子路径 / 请求体都在）。
func TestImageAsyncSubmitFullChainReturns202WithPollURL(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	_, key := createImageAsyncUserKey(t, env.ctx, "async-submit-user", "sk-octopus-async-submit", true, 0)

	rec, payload := submitImageAsync(t, env, key.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s, want 202", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3", got)
	}

	taskID, _ := payload["id"].(string)
	if taskID == "" || !strings.HasPrefix(taskID, "imgtask_") {
		t.Fatalf("id = %q, want an imgtask_ id", payload["id"])
	}
	if payload["task_id"] != taskID {
		t.Fatalf("task_id = %v, want the same value as id (%q)", payload["task_id"], taskID)
	}
	if payload["object"] != "image.generation.task" {
		t.Fatalf("object = %v, want image.generation.task", payload["object"])
	}
	if payload["status"] != "processing" {
		t.Fatalf("status = %v, want processing", payload["status"])
	}
	wantPollURL := "/v1/images/tasks/" + taskID
	if payload["poll_url"] != wantPollURL {
		t.Fatalf("poll_url = %v, want %q", payload["poll_url"], wantPollURL)
	}
	if got := rec.Header().Get("Location"); got != wantPollURL {
		t.Fatalf("Location = %q, want %q", got, wantPollURL)
	}
	createdAt, _ := payload["created_at"].(float64)
	expiresAt, _ := payload["expires_at"].(float64)
	if createdAt <= 0 || expiresAt <= createdAt {
		t.Fatalf("created_at/expires_at = %v/%v, want a future expiry", createdAt, expiresAt)
	}

	// 任务行真的落库了，且归属/渠道/子路径都对（执行器靠这些字段重放）。
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read back task %s: %v (task=%v)", taskID, err, task)
	}
	if task.Status != model.ImageTaskStatusProcessing {
		t.Fatalf("stored status = %q, want processing", task.Status)
	}
	if task.OwnerTokenID != key.ID || task.OwnerUserID != key.UserID {
		t.Fatalf("stored owner = (%d,%d), want (%d,%d)", task.OwnerTokenID, task.OwnerUserID, key.ID, key.UserID)
	}
	if task.ChannelID != env.channel.ID || task.ChannelKeyID != env.channel.Keys[0].ID {
		t.Fatalf("stored channel = (%d,%d), want (%d,%d)", task.ChannelID, task.ChannelKeyID, env.channel.ID, env.channel.Keys[0].ID)
	}
	if task.Endpoint != model.ImageTaskEndpointGenerations {
		t.Fatalf("stored endpoint = %q, want generations", task.Endpoint)
	}
	if task.Model != imageAsyncTestModel {
		t.Fatalf("stored model = %q, want %q", task.Model, imageAsyncTestModel)
	}
	if !strings.Contains(task.RequestBody, "a quiet lake at dawn") {
		t.Fatalf("stored request body lost the prompt: %q", task.RequestBody)
	}

	// 提交阶段**不许**打上游：这是异步与同步的唯一分界。
	if *env.hits != 0 {
		t.Fatalf("submit made %d upstream call(s); an async submit must not wait for the upstream", *env.hits)
	}

	// 轮询：处理中 → 200 + processing。
	poll := doImageAsyncRequest(env.engine, http.MethodGet, wantPollURL, key.APIKey, "")
	if poll.Code != http.StatusOK {
		t.Fatalf("poll: %d %s, want 200", poll.Code, poll.Body.String())
	}
	var polled map[string]any
	if err := json.Unmarshal(poll.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll body: %v", err)
	}
	if polled["status"] != "processing" {
		t.Fatalf("polled status = %v, want processing", polled["status"])
	}
}

// 契约第二节：完成态必须带 image_url 与上游原始返回（result）。
func TestImageAsyncTaskPollReturnsCompletedResult(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	_, key := createImageAsyncUserKey(t, env.ctx, "async-completed-user", "sk-octopus-async-completed", true, 0)

	rec, payload := submitImageAsync(t, env, key.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	taskID, _ := payload["id"].(string)
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read back task: %v", err)
	}

	resultJSON := `{"created":1700000000,"data":[{"url":"https://images.upstream.example/b.png"}]}`
	ok, err := op.ImageTaskStatusCAS(env.ctx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusCompleted, map[string]any{
		"completed_at": time.Now().Unix(),
		"result_json":  resultJSON,
	})
	if err != nil || !ok {
		t.Fatalf("complete task: %v %v", ok, err)
	}

	poll := doImageAsyncRequest(env.engine, http.MethodGet, "/v1/images/tasks/"+taskID, key.APIKey, "")
	if poll.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", poll.Code, poll.Body.String())
	}
	var body struct {
		Status      string          `json:"status"`
		ImageURL    string          `json:"image_url"`
		Result      json.RawMessage `json:"result"`
		CompletedAt int64           `json:"completed_at"`
	}
	if err := json.Unmarshal(poll.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if body.Status != "completed" {
		t.Fatalf("status = %q, want completed", body.Status)
	}
	if body.ImageURL != "https://images.upstream.example/b.png" {
		t.Fatalf("image_url = %q", body.ImageURL)
	}
	if body.CompletedAt <= 0 {
		t.Fatalf("completed_at = %d, want > 0", body.CompletedAt)
	}
	if !strings.Contains(string(body.Result), "images.upstream.example") {
		t.Fatalf("result did not carry the raw upstream payload: %s", body.Result)
	}
}

// 契约第四节第 2 条（最容易被写成假绿的一条）：用户 A 创建的任务，
// 用户 B 用同一个 TaskID 查 → 404。**必须测"查不到"这个方向**。
func TestImageAsyncTaskPollIsOwnerScoped(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	userA, keyA := createImageAsyncUserKey(t, env.ctx, "async-owner-a", "sk-octopus-async-owner-a", true, 0)
	userB, keyB := createImageAsyncUserKey(t, env.ctx, "async-owner-b", "sk-octopus-async-owner-b", true, 0)
	// 同一个用户 A 的**第二把 key**：证明归属是 token+user 两段，不是只按用户。
	secondKeyOfSameUser := createVideosFamilyAPIKey(t, env.ctx, userA, "async-owner-a-second", "sk-octopus-async-owner-a-second", nil)

	rec, payload := submitImageAsync(t, env, keyA.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit as A: %d %s", rec.Code, rec.Body.String())
	}
	taskID, _ := payload["id"].(string)
	path := "/v1/images/tasks/" + taskID

	// 正向对照：A 自己查得到（否则下面的 404 可能只是路由/任务没建好）。
	if own := doImageAsyncRequest(env.engine, http.MethodGet, path, keyA.APIKey, ""); own.Code != http.StatusOK {
		t.Fatalf("A polling its own task: %d %s, want 200", own.Code, own.Body.String())
	}

	// 反向：B 拿同一个 TaskID 查 → 404，且响应里没有任务数据。
	other := doImageAsyncRequest(env.engine, http.MethodGet, path, keyB.APIKey, "")
	if other.Code != http.StatusNotFound {
		t.Fatalf("B polling A's task: %d %s, want 404", other.Code, other.Body.String())
	}
	for _, leak := range []string{taskID, imageAsyncTestModel, "image.generation.task"} {
		if strings.Contains(other.Body.String(), leak) {
			t.Fatalf("404 body leaked %q: %s", leak, other.Body.String())
		}
	}

	// 同一个用户、不同的 key：同样 404（归属是 token+user 两段，不是只按用户）。
	sameUserOtherToken := doImageAsyncRequest(env.engine, http.MethodGet, path, secondKeyOfSameUser.APIKey, "")
	if sameUserOtherToken.Code != http.StatusNotFound {
		t.Fatalf("another token polling the task: %d %s, want 404 (owner_token_id must be part of the filter)", sameUserOtherToken.Code, sameUserOtherToken.Body.String())
	}

	// 反向对照的第二层：A 的第二把 key 去查**同一用户**的任务也是 404，
	// 但库里那一行本身是可读的（A 的第一把 key 查得到）—— 所以 404 只可能来自归属过滤。
	if userB.ID == userA.ID {
		t.Fatalf("test precondition broken: two users share an id (%d)", userA.ID)
	}
	if again := doImageAsyncRequest(env.engine, http.MethodGet, path, keyA.APIKey, ""); again.Code != http.StatusOK {
		t.Fatalf("A's first key stopped working after the negative controls: %d %s", again.Code, again.Body.String())
	}

	// 数据库里确实只有 B 的那把 key 对应的 user 才有这个问题：证明 404 不是"任务不存在"。
	if _, err := op.ImageTaskGetForOwner(env.ctx, taskID, keyB.ID, userB.ID); err != nil {
		t.Fatalf("direct op read as B returned a hard error: %v", err)
	}
	if found, err := op.ImageTaskGetForOwner(env.ctx, taskID, keyB.ID, userB.ID); err != nil || found != nil {
		t.Fatalf("op-level read as B = %v, %v; want nil, nil", found, err)
	}
}

// 契约第四节第 3 条（查询侧）：expires_at 已过的任务查 → 404。
func TestImageAsyncTaskPollExpiredReturns404(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	_, key := createImageAsyncUserKey(t, env.ctx, "async-expired-user", "sk-octopus-async-expired", true, 0)

	rec, payload := submitImageAsync(t, env, key.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	taskID, _ := payload["id"].(string)
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read back task: %v", err)
	}

	past := time.Now().Add(-time.Minute).Unix()
	if err := db.GetDB().WithContext(env.ctx).Model(&model.ImageTask{}).
		Where("id = ?", task.ID).Update("expires_at", past).Error; err != nil {
		t.Fatalf("expire task: %v", err)
	}

	poll := doImageAsyncRequest(env.engine, http.MethodGet, "/v1/images/tasks/"+taskID, key.APIKey, "")
	if poll.Code != http.StatusNotFound {
		t.Fatalf("polling an expired task: %d %s, want 404", poll.Code, poll.Body.String())
	}
}

// 契约第四节第 4 条：开关关闭时两个 async 端点一律 404（不是 403 —— 未启用的
// 功能对外不存在）。
func TestImageAsyncEndpointsAre404WhenDisabled(t *testing.T) {
	env := setupImageAsyncEnv(t, false)
	_, key := createImageAsyncUserKey(t, env.ctx, "async-disabled-user", "sk-octopus-async-disabled", true, 0)

	rec, _ := submitImageAsync(t, env, key.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled submit: %d %s, want 404", rec.Code, rec.Body.String())
	}
	rec, _ = submitImageAsync(t, env, key.APIKey, "/v1/images/edits/async")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled edits submit: %d %s, want 404", rec.Code, rec.Body.String())
	}

	poll := doImageAsyncRequest(env.engine, http.MethodGet, "/v1/images/tasks/imgtask_whatever", key.APIKey, "")
	if poll.Code != http.StatusNotFound {
		t.Fatalf("disabled poll: %d %s, want 404", poll.Code, poll.Body.String())
	}

	// 关闭时也不许偷偷建任务（否则"关着"只是把结果藏起来了）。
	var count int64
	if err := db.GetDB().WithContext(env.ctx).Model(&model.ImageTask{}).Count(&count).Error; err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d image task(s) created while the feature was disabled", count)
	}
}

// 契约第四节第 5 条（提交那一条）：提交时按次预扣一次；查询与完成都不再扣。
func TestImageAsyncSubmitChargesOnceAndCompletionDoesNotChargeAgain(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{PerCall: 0.5})
	t.Cleanup(func() { price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{}) })

	user, key := createImageAsyncUserKey(t, env.ctx, "async-billing-user", "sk-octopus-async-billing", false, 5.0)
	if got := mustHandlerUser(t, user.ID).Balance; got != 5.0 {
		t.Fatalf("balance before submit = %v, want 5", got)
	}

	rec, payload := submitImageAsync(t, env, key.APIKey, "/v1/images/generations/async")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 4.5 {
		t.Fatalf("balance after submit = %v, want 4.5 (charged exactly once at creation)", got)
	}
	taskID, _ := payload["id"].(string)
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read back task: %v", err)
	}
	if task.Cost != 0.5 {
		t.Fatalf("stored cost = %v, want 0.5", task.Cost)
	}

	// 查询处理中：不许扣。
	if poll := doImageAsyncRequest(env.engine, http.MethodGet, "/v1/images/tasks/"+taskID, key.APIKey, ""); poll.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", poll.Code, poll.Body.String())
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 4.5 {
		t.Fatalf("balance after polling = %v, want 4.5 (queries are free)", got)
	}

	// 完成：不许再扣。
	ok, err := op.ImageTaskStatusCAS(env.ctx, task.ID, model.ImageTaskStatusProcessing, model.ImageTaskStatusCompleted, map[string]any{
		"completed_at": time.Now().Unix(),
		"result_json":  `{"data":[{"url":"https://images.upstream.example/c.png"}]}`,
	})
	if err != nil || !ok {
		t.Fatalf("complete task: %v %v", ok, err)
	}
	if poll := doImageAsyncRequest(env.engine, http.MethodGet, "/v1/images/tasks/"+taskID, key.APIKey, ""); poll.Code != http.StatusOK {
		t.Fatalf("poll completed: %d %s", poll.Code, poll.Body.String())
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 4.5 {
		t.Fatalf("balance after completion = %v, want 4.5 (completion must not charge again)", got)
	}
}

func mustHandlerUser(t *testing.T, userID int) model.User {
	t.Helper()
	user, err := op.UserGet(userID)
	if err != nil {
		t.Fatalf("get user %d: %v", userID, err)
	}
	return user
}

// 【路由真起】契约「注册顺序」那一节要求"必须跑真实起路由的测试确认，不能只靠推理"。
//
// 这条用例把全部路由注册进一个真 engine：注册阶段就 panic 的路由树会直接把用例炸掉，
// 而路由表逐条对照能抓住"某条路径永远匹配不上"（那对客户端和没注册是一回事）。
func TestImageAsyncRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	registered := make(map[string]string, len(engine.Routes()))
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = route.Handler
	}

	cases := []struct {
		method string
		path   string
		want   string
	}{
		// 异步三条（契约新增）。
		{http.MethodPost, "/v1/images/generations/async", "asyncSubmit"},
		{http.MethodPost, "/v1/images/edits/async", "asyncSubmit"},
		{http.MethodGet, "/v1/images/tasks/:task_id", "asyncGet"},
		// 同步三条：行为一个字都不许变，路由也必须还在原处。
		{http.MethodPost, "/v1/images/generations", "generations"},
		{http.MethodPost, "/v1/images/edits", "edits"},
		{http.MethodPost, "/v1/images/variations", "variations"},
	}
	for _, tc := range cases {
		key := tc.method + " " + tc.path
		handler, ok := registered[key]
		if !ok {
			t.Fatalf("%s is not registered (contract route missing)", key)
		}
		trimmed := strings.TrimPrefix(handler, "github.com/zhizhishu/octopus/internal/server/handlers.")
		if trimmed != tc.want && !strings.HasPrefix(trimmed, tc.want+".") {
			t.Fatalf("%s is handled by %q, want a %s handler", key, handler, tc.want)
		}
	}
}

// 同一条路由表还必须真的能匹配：`/generations` 与 `/generations/async` 在同一层，
// 字面量更长的那条一旦被短的那条吃掉，客户端拿到的是"请求被当成了同步生成"。
func TestImageAsyncRoutesMatchAndAreNotSwallowedByTheSyncRoute(t *testing.T) {
	env := setupImageAsyncEnv(t, false) // 关着：handler 回 404，正好用来区分"没匹配上"
	_, key := createImageAsyncUserKey(t, env.ctx, "async-route-match", "sk-octopus-async-route-match", true, 0)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/images/generations/async"},
		{http.MethodPost, "/v1/images/edits/async"},
		{http.MethodGet, "/v1/images/tasks/imgtask_probe"},
	} {
		rec := doImageAsyncRequest(env.engine, tc.method, tc.path, key.APIKey, `{"model":"`+imageAsyncTestModel+`"}`)
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s %s did not match any route", tc.method, tc.path)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s -> %d, want the handler's own 404 (feature disabled)", tc.method, tc.path, rec.Code)
		}
	}

	// 反向：同步端点没有被 async 路由吃掉（关着开关时它照旧走 ImagesHandler，
	// 回的是"模型不可用/上游不可用"这一类错误，绝不是我们那条 "not found"）。
	syncRec := doImageAsyncRequest(env.engine, http.MethodPost, "/v1/images/generations", key.APIKey,
		`{"model":"`+imageAsyncTestModel+`","prompt":"x"}`)
	if strings.Contains(syncRec.Body.String(), "404 page not found") {
		t.Fatalf("the sync route stopped matching")
	}
	if syncRec.Code == http.StatusAccepted {
		t.Fatalf("the sync route answered 202; it must never behave like the async one")
	}
}
