package relay

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// Midjourney 中转走真实 handler + httptest 假 mj-proxy 上游。

type mjCall struct {
	method string
	path   string
	secret string
	auth   string
	body   map[string]any
}

type fakeMJ struct {
	mu    sync.Mutex
	calls []mjCall
	srv   *httptest.Server
}

func newFakeMJ(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, call mjCall)) *fakeMJ {
	t.Helper()
	f := &fakeMJ{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := mjCall{method: r.Method, path: r.URL.Path, secret: r.Header.Get("mj-api-secret"), auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &call.body)
		f.mu.Lock()
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handler(w, r, call)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMJ) Calls() []mjCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mjCall(nil), f.calls...)
}

func createMJChannel(t *testing.T, ctx context.Context, name, baseURL string, keys []string, models string, priority int, typ outbound.OutboundType) dbmodel.Channel {
	t.Helper()
	channelKeys := make([]dbmodel.ChannelKey, 0, len(keys))
	for _, k := range keys {
		channelKeys = append(channelKeys, dbmodel.ChannelKey{Enabled: true, ChannelKey: k})
	}
	ch := dbmodel.Channel{
		Name:     name,
		Type:     typ,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: baseURL}},
		Keys:     channelKeys,
		Model:    models,
		Priority: priority,
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch
}

func newMJContext(method, path, body string, apiKeyID, userID int) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// 客户端用 mj-api-secret 带 octopus key 进来：它绝不能被送到上游。
	req.Header.Set("mj-api-secret", "sk-octopus-client-key")
	c.Request = req
	c.Set("api_key_id", apiKeyID)
	c.Set("user_id", userID)
	c.Set("request_ip", "127.0.0.1")
	return c, rec
}

func resetMJMemory(t *testing.T) {
	t.Helper()
	clear := func() {
		midjourneyTaskStore.Lock()
		midjourneyTaskStore.items = make(map[string]midjourneyTaskEntry)
		midjourneyTaskStore.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func mjSubmit(mode, body string, apiKeyID, userID int) *httptest.ResponseRecorder {
	path := "/mj/submit/" + mode
	if mode == MidjourneyModeSwapFace {
		path = "/mj/insight-face/swap"
	}
	c, rec := newMJContext(http.MethodPost, path, body, apiKeyID, userID)
	c.Request.Header.Set("Accept", "application/json")
	MidjourneySubmitHandler(c, mode)
	return rec
}

func TestMidjourneyImagineSuccessRecordsOwnerAndBills(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"description":"提交成功","result":"1700000000001","properties":{"discordInstanceId":"x"}}`))
	})
	// base 填成 …/v1（同一个渠道还要跑 chat）：MJ 路径必须落到根上的 /mj/...
	ch := createMJChannel(t, ctx, "mj-a", up.srv.URL+"/v1", []string{"mj-upstream-key"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)

	const perCall = 0.2
	price.SetLLMPrice("mj_imagine", dbmodel.LLMPrice{PerCall: perCall})
	t.Cleanup(func() { price.SetLLMPrice("mj_imagine", dbmodel.LLMPrice{}) })
	user, err := op.UserCreate(dbmodel.UserCreateRequest{Username: "mj-user", Password: "test-password-123", Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: 1}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"a cat --v 6","base64Array":[],"notifyHook":"http://attacker.example/hook","accountFilter":{"channelId":"1"},"state":"s1"}`, 0, user.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["result"] != "1700000000001" || got["code"].(float64) != 1 {
		t.Fatalf("submit response not passed through: %s", rec.Body.String())
	}

	calls := up.Calls()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.path != "/mj/submit/imagine" {
		t.Fatalf("upstream path %q, want /mj/submit/imagine", call.path)
	}
	if call.secret != "mj-upstream-key" {
		t.Fatalf("upstream mj-api-secret %q, want channel key", call.secret)
	}
	if call.auth != "" {
		t.Fatalf("upstream must not get an Authorization header, got %q", call.auth)
	}
	if _, has := call.body["notifyHook"]; has {
		t.Fatalf("notifyHook must be stripped: %v", call.body)
	}
	if _, has := call.body["accountFilter"]; has {
		t.Fatalf("accountFilter must be stripped: %v", call.body)
	}
	if call.body["prompt"] != "a cat --v 6" || call.body["state"] != "s1" {
		t.Fatalf("payload fields not preserved: %v", call.body)
	}
	if _, has := call.body["model"]; has {
		t.Fatalf("mj-proxy protocol has no model field: %v", call.body)
	}

	row, err := op.MidjourneyTaskGetByUpstreamID(ctx, "1700000000001")
	if err != nil || row == nil {
		t.Fatalf("owner row missing: %v", err)
	}
	if row.ChannelID != ch.ID || row.ChannelKeyID == 0 || row.OwnerUserID != user.ID || row.Model != "mj_imagine" || row.Action != "IMAGINE" {
		t.Fatalf("unexpected owner row: %+v", row)
	}

	after, _ := op.UserGet(user.ID)
	if charged := user.Balance - after.Balance; charged < perCall-1e-9 || charged > perCall+1e-9 {
		t.Fatalf("charged %v, want per_call %v", charged, perCall)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("relay log missing: %v", err)
	}
	if logs[0].RequestEndpoint != "images_midjourney" || logs[0].RequestModelName != "mj_imagine" || logs[0].Error != "" {
		t.Fatalf("unexpected log: endpoint=%q model=%q err=%q", logs[0].RequestEndpoint, logs[0].RequestModelName, logs[0].Error)
	}
}

func TestMidjourneyRewritesQueuedAndExistingCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	var n atomic.Int32
	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		if n.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"code":22,"description":"排队中","result":"q-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":21,"description":"任务已存在","result":"q-2","properties":{"status":"SUCCESS"}}`))
	})
	createMJChannel(t, ctx, "mj", up.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)

	for i, wantID := range []string{"q-1", "q-2"} {
		rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, 0)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":1`) || !strings.Contains(rec.Body.String(), wantID) {
			t.Fatalf("round %d: want code rewritten to 1, got %d %s", i, rec.Code, rec.Body.String())
		}
		if row, _ := op.MidjourneyTaskGetByUpstreamID(ctx, wantID); row == nil {
			t.Fatalf("round %d: accepted task %s not recorded", i, wantID)
		}
	}
}

func TestMidjourneyRejectedSubmitIsNotBilledOrRecorded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":24,"description":"可能包含敏感词","result":"bad-1","properties":{"promptEn":"x","bannedWord":"x"}}`))
	})
	createMJChannel(t, ctx, "mj", up.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
	price.SetLLMPrice("mj_imagine", dbmodel.LLMPrice{PerCall: 0.2})
	t.Cleanup(func() { price.SetLLMPrice("mj_imagine", dbmodel.LLMPrice{}) })
	user, err := op.UserCreate(dbmodel.UserCreateRequest{Username: "mj-rejected", Password: "test-password-123", Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: 1}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, user.ID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":24`) {
		t.Fatalf("business rejection must reach the client verbatim, got %d %s", rec.Code, rec.Body.String())
	}
	if len(up.Calls()) != 1 {
		t.Fatalf("a business rejection must not be retried: %d calls", len(up.Calls()))
	}
	if row, _ := op.MidjourneyTaskGetByUpstreamID(ctx, "bad-1"); row != nil {
		t.Fatalf("rejected task must not be recorded: %+v", row)
	}
	after, _ := op.UserGet(user.ID)
	if after.Balance != user.Balance {
		t.Fatalf("rejected submit billed: %v -> %v", user.Balance, after.Balance)
	}
	logs, _ := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if len(logs) == 0 || !strings.Contains(logs[0].Error, "code 24") {
		t.Fatalf("rejected submit must be logged as a failure, got %+v", logs)
	}
}

func TestMidjourneyHTTPErrorFailsOverButTransportErrorDoesNot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	good := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"result":"ok-1"}`))
	})
	broken := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	})
	createMJChannel(t, ctx, "mj-broken", broken.srv.URL, []string{"kb"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
	createMJChannel(t, ctx, "mj-good", good.srv.URL, []string{"kg"}, "mj_imagine", 2, outbound.OutboundTypeOpenAIChat)

	rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok-1") {
		t.Fatalf("HTTP 500 must fail over to the next channel, got %d %s", rec.Code, rec.Body.String())
	}
	if len(broken.Calls()) != 1 || len(good.Calls()) != 1 {
		t.Fatalf("calls broken=%d good=%d, want 1/1", len(broken.Calls()), len(good.Calls()))
	}

	// 传输层失败：上游可能已经建了任务，绝不能换渠道再提交一次。
	ctx2 := setupRelayErrorDB(t)
	resetMJMemory(t)
	good2 := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"result":"ok-2"}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var hung atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			hung.Add(1)
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_ = conn.Close() // 读完请求就断：典型的「不知道上游有没有受理」
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	createMJChannel(t, ctx2, "mj-drop", "http://"+ln.Addr().String(), []string{"kd"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
	createMJChannel(t, ctx2, "mj-good2", good2.srv.URL, []string{"kg2"}, "mj_imagine", 2, outbound.OutboundTypeOpenAIChat)

	rec = mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, 0)
	if rec.Code < 400 {
		t.Fatalf("ambiguous transport failure must surface as an error, got %d %s", rec.Code, rec.Body.String())
	}
	if hung.Load() == 0 {
		t.Fatalf("test did not hit the dropping upstream")
	}
	if len(good2.Calls()) != 0 {
		t.Fatalf("transport failure was retried on another channel (%d calls): duplicate task risk", len(good2.Calls()))
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != float64(4) || body["description"] == "" {
		t.Fatalf("error body must be mj-proxy shaped, got %s", rec.Body.String())
	}
}

func TestMidjourneyFollowUpActionsArePinnedToOwnerChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	owner := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		switch call.path {
		case "/mj/submit/imagine":
			_, _ = w.Write([]byte(`{"code":1,"result":"origin-1"}`))
		default:
			_, _ = w.Write([]byte(`{"code":1,"result":"child-` + strings.TrimPrefix(call.path, "/mj/submit/") + `"}`))
		}
	})
	other := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"result":"wrong-channel"}`))
	})
	// 归属渠道只挂了 mj_imagine；另一个渠道优先级更高、挂着全部后续动作模型。
	// 后续动作若按模型正常选路，就会落到 other 上 —— 那里根本没有 origin-1 这条任务。
	createMJChannel(t, ctx, "mj-owner", owner.srv.URL, []string{"owner-key"}, "mj_imagine", 2, outbound.OutboundTypeOpenAIChat)
	createMJChannel(t, ctx, "mj-other", other.srv.URL, []string{"other-key"}, "mj_upscale,mj_variation,mj_reroll,mj_modal,mj_inpaint,mj_video", 1, outbound.OutboundTypeOpenAIResponse)

	if rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, 3); rec.Code != http.StatusOK {
		t.Fatalf("imagine: %d %s", rec.Code, rec.Body.String())
	}
	cases := []struct{ mode, body string }{
		{MidjourneyModeChange, `{"taskId":"origin-1","action":"UPSCALE","index":1}`},
		{MidjourneyModeSimpleChange, `{"content":"origin-1 V2"}`},
		{MidjourneyModeAction, `{"taskId":"origin-1","customId":"MJ::JOB::upsample::2::3dbbd469-36af-4a0f-8f02-df6c579e7011"}`},
		{MidjourneyModeModal, `{"taskId":"origin-1","prompt":"y"}`},
		{MidjourneyModeVideo, `{"taskId":"origin-1","prompt":"move"}`},
	}
	for _, tc := range cases {
		rec := mjSubmit(tc.mode, tc.body, 0, 3)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "wrong-channel") {
			t.Fatalf("%s not pinned to owner: %d %s", tc.mode, rec.Code, rec.Body.String())
		}
	}
	if n := len(other.Calls()); n != 0 {
		t.Fatalf("follow-up actions leaked to a non-owner channel: %d calls", n)
	}
	for _, call := range owner.Calls() {
		if call.secret != "owner-key" {
			t.Fatalf("owner channel called with wrong key %q", call.secret)
		}
	}
	// 子任务也记了归属（可以继续在它上面放大）。
	if row, _ := op.MidjourneyTaskGetByUpstreamID(ctx, "child-change"); row == nil {
		t.Fatalf("child task of a pinned action not recorded")
	}

	// 别的租户拿着同一个 taskId：fail-closed，上游一次都不碰。
	before := len(owner.Calls())
	rec := mjSubmit(MidjourneyModeChange, `{"taskId":"origin-1","action":"UPSCALE","index":1}`, 0, 4)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "task_not_found") {
		t.Fatalf("foreign tenant follow-up: got %d %s", rec.Code, rec.Body.String())
	}
	// 不认识的 taskId 同样拒绝。
	rec = mjSubmit(MidjourneyModeChange, `{"taskId":"nope","action":"UPSCALE","index":1}`, 0, 3)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "task_not_found") {
		t.Fatalf("unknown origin task: got %d %s", rec.Code, rec.Body.String())
	}
	if len(owner.Calls()) != before || len(other.Calls()) != 0 {
		t.Fatalf("rejected follow-ups reached an upstream")
	}
}

func TestMidjourneyInpaintIsNotBilled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		if call.path == "/mj/submit/imagine" {
			_, _ = w.Write([]byte(`{"code":1,"result":"o-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":21,"result":"modal-1","properties":{"status":"MODAL"}}`))
	})
	createMJChannel(t, ctx, "mj", up.srv.URL, []string{"k"}, "mj_imagine,mj_inpaint", 1, outbound.OutboundTypeOpenAIChat)
	price.SetLLMPrice("mj_inpaint", dbmodel.LLMPrice{PerCall: 0.5})
	t.Cleanup(func() { price.SetLLMPrice("mj_inpaint", dbmodel.LLMPrice{}) })
	user, err := op.UserCreate(dbmodel.UserCreateRequest{Username: "mj-inpaint", Password: "test-password-123", Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: 1}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, user.ID); rec.Code != http.StatusOK {
		t.Fatalf("imagine: %d", rec.Code)
	}
	rec := mjSubmit(MidjourneyModeAction, `{"taskId":"o-1","customId":"MJ::Inpaint::1::abc::SOLO"}`, 0, user.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("inpaint: %d %s", rec.Code, rec.Body.String())
	}
	// INPAINT 的 21 不改写（new-api 同样保留，客户端靠 21 进入弹窗）。
	if !strings.Contains(rec.Body.String(), `"code":21`) {
		t.Fatalf("INPAINT code 21 must not be rewritten: %s", rec.Body.String())
	}
	after, _ := op.UserGet(user.ID)
	if after.Balance != user.Balance {
		t.Fatalf("INPAINT must be free: %v -> %v", user.Balance, after.Balance)
	}
}

func TestMidjourneyFetchImageSeedAndListAreOwnerScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)

	mk := func(name, key string) (*fakeMJ, dbmodel.Channel) {
		f := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
			switch {
			case strings.HasSuffix(call.path, "/fetch"):
				id := strings.TrimSuffix(strings.TrimPrefix(call.path, "/mj/task/"), "/fetch")
				_, _ = w.Write([]byte(`{"id":"` + id + `","status":"SUCCESS","imageUrl":"https://cdn.example/` + id + `.png","progress":"100%"}`))
			case strings.HasSuffix(call.path, "/image-seed"):
				_, _ = w.Write([]byte(`{"code":1,"result":"12345"}`))
			case call.path == "/mj/task/list-by-condition":
				ids, _ := call.body["ids"].([]any)
				out := make([]map[string]any, 0)
				for _, v := range ids {
					out = append(out, map[string]any{"id": v, "status": "SUCCESS", "from": name})
				}
				b, _ := json.Marshal(out)
				_, _ = w.Write(b)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		// base 填成 …/mj：不能叠成 /mj/mj/...
		ch := createMJChannel(t, ctx, name, f.srv.URL+"/mj", []string{key}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)
		got, err := op.ChannelGet(ch.ID, ctx)
		if err != nil || len(got.Keys) == 0 {
			t.Fatalf("reload channel: %v", err)
		}
		return f, *got
	}
	a, chA := mk("mj-a", "ka")
	b, chB := mk("mj-b", "kb")
	own := func(id string, ch dbmodel.Channel, tokenID, userID int) {
		recordMidjourneyTaskOwner(ctx, id, midjourneyTaskEntry{channelID: ch.ID, channelKeyID: ch.Keys[0].ID, model: "mj_imagine", ownerTokenID: tokenID, ownerUserID: userID}, "IMAGINE")
	}
	own("task-a", chA, 5, 9)
	own("task-b", chB, 5, 9)
	own("task-x", chA, 6, 99) // 别人的

	fetch := func(id, suffix string, tokenID, userID int) *httptest.ResponseRecorder {
		c, rec := newMJContext(http.MethodGet, "/mj/task/"+id+"/"+suffix, "", tokenID, userID)
		c.Params = gin.Params{{Key: "id", Value: id}}
		MidjourneyTaskFetchHandler(c, suffix)
		return rec
	}

	rec := fetch("task-a", "fetch", 5, 9)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"imageUrl":"https://cdn.example/task-a.png"`) {
		t.Fatalf("fetch task-a: %d %s", rec.Code, rec.Body.String())
	}
	if calls := a.Calls(); len(calls) != 1 || calls[0].path != "/mj/task/task-a/fetch" || calls[0].secret != "ka" || calls[0].method != http.MethodGet {
		t.Fatalf("fetch did not reach owner channel correctly: %+v", calls)
	}
	rec = fetch("task-b", "image-seed", 5, 9)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "12345") {
		t.Fatalf("image-seed task-b: %d %s", rec.Code, rec.Body.String())
	}
	if calls := b.Calls(); len(calls) != 1 || calls[0].path != "/mj/task/task-b/image-seed" || calls[0].secret != "kb" {
		t.Fatalf("image-seed did not reach owner channel: %+v", calls)
	}

	// 别人的任务 / 不认识的任务：fail-closed，不碰上游。
	for _, id := range []string{"task-x", "unknown"} {
		rec = fetch(id, "fetch", 5, 9)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "task_not_found") {
			t.Fatalf("fetch %s: got %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if len(a.Calls()) != 1 {
		t.Fatalf("foreign fetch reached the upstream")
	}

	// list-by-condition：只返回自己的，按渠道分组各问一次。
	c, listRec := newMJContext(http.MethodPost, "/mj/task/list-by-condition", `{"ids":["task-a","task-b","task-x","unknown","task-a"]}`, 5, 9)
	MidjourneyTaskListByConditionHandler(c)
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil || listRec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listRec.Code, listRec.Body.String())
	}
	gotIDs := map[string]string{}
	for _, item := range list {
		gotIDs[item["id"].(string)] = item["from"].(string)
	}
	if len(gotIDs) != 2 || gotIDs["task-a"] != "mj-a" || gotIDs["task-b"] != "mj-b" {
		t.Fatalf("list must contain exactly the caller's tasks from their owner channels: %v", gotIDs)
	}
	for _, call := range a.Calls() {
		if call.path == "/mj/task/list-by-condition" {
			ids, _ := call.body["ids"].([]any)
			if len(ids) != 1 || ids[0] != "task-a" {
				t.Fatalf("foreign / unknown ids leaked upstream: %v", ids)
			}
		}
	}

	// 重启（内存归属清空）之后，库里那一行仍能把 fetch 送回归属渠道。
	midjourneyTaskStore.Lock()
	midjourneyTaskStore.items = make(map[string]midjourneyTaskEntry)
	midjourneyTaskStore.Unlock()
	if rec = fetch("task-a", "fetch", 5, 9); rec.Code != http.StatusOK {
		t.Fatalf("fetch after restart: %d %s", rec.Code, rec.Body.String())
	}
	if rec = fetch("task-x", "fetch", 5, 9); rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant check must also apply to the DB row, got %d", rec.Code)
	}
}

func TestMidjourneyRequestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"result":"t"}`))
	})
	createMJChannel(t, ctx, "mj", up.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeOpenAIChat)

	cases := []struct{ mode, body, want string }{
		{MidjourneyModeImagine, `{"prompt":""}`, "prompt_is_required"},
		{MidjourneyModeImagine, `not json`, "bind_request_body_failed"},
		{MidjourneyModeChange, `{"action":"UPSCALE","index":1}`, "task_id_is_required"},
		{MidjourneyModeChange, `{"taskId":"t","index":1}`, "action_is_required"},
		{MidjourneyModeChange, `{"taskId":"t","action":"UPSCALE"}`, "index_is_required"},
		{MidjourneyModeSimpleChange, `{"content":"t X9"}`, "content_parse_failed"},
		{MidjourneyModeAction, `{"taskId":"t"}`, "custom_id_is_required"},
		{MidjourneyModeAction, `{"taskId":"t","customId":"MJ::JOB::nonsense::1::x"}`, "unknown_action"},
		{MidjourneyModeModal, `{"prompt":"x"}`, "task_id_is_required"},
	}
	for _, tc := range cases {
		rec := mjSubmit(tc.mode, tc.body, 0, 0)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s %s: got %d %s, want 400 %s", tc.mode, tc.body, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if len(up.Calls()) != 0 {
		t.Fatalf("invalid requests reached the upstream")
	}

	// key 不允许的动作模型 → 拒绝（supported_models 白名单同样作用于 MJ 动作模型名）。
	c, rec := newMJContext(http.MethodPost, "/mj/submit/imagine", `{"prompt":"x"}`, 0, 0)
	c.Set("supported_models", "gpt-4o")
	MidjourneySubmitHandler(c, MidjourneyModeImagine)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model not supported") {
		t.Fatalf("supported_models not enforced: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMidjourneySkipsCustomChannelAndUnsupportedTypes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	custom := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		_, _ = w.Write([]byte(`{"code":1,"result":"custom"}`))
	})
	createMJChannel(t, ctx, "mj-custom", custom.srv.URL, []string{"k"}, "mj_imagine", 1, outbound.OutboundTypeCustomOpenAIChat)
	rec := mjSubmit(MidjourneyModeImagine, `{"prompt":"x"}`, 0, 0)
	if rec.Code < 400 || len(custom.Calls()) != 0 {
		t.Fatalf("custom channel must be skipped: %d %s calls=%d", rec.Code, rec.Body.String(), len(custom.Calls()))
	}
}

func TestMidjourneyParsersMatchNewAPI(t *testing.T) {
	for _, tc := range []struct {
		customID, want string
	}{
		{"MJ::JOB::upsample::2::3dbbd469", "UPSCALE"},
		{"MJ::JOB::variation::3::3dbbd469", "VARIATION"},
		{"MJ::JOB::low_variation::1::x::SOLO", "LOW_VARIATION"},
		{"MJ::JOB::high_variation::1::x::SOLO", "HIGH_VARIATION"},
		{"MJ::JOB::pan_left::1::x::SOLO", "PAN"},
		{"MJ::JOB::reroll::0::x::SOLO", "REROLL"},
		{"MJ::Outpaint::50::1::x::SOLO", "ZOOM"},
		{"MJ::CustomZoom::x", "CUSTOM_ZOOM"},
		{"MJ::Inpaint::1::x::SOLO", "INPAINT"},
	} {
		got, desc := midjourneyActionFromCustomID(tc.customID)
		if desc != "" || got != tc.want {
			t.Fatalf("customId %q -> %q (%s), want %q", tc.customID, got, desc, tc.want)
		}
	}
	for _, tc := range []struct{ content, id, action string }{
		{"123 U1", "123", "UPSCALE"},
		{"123 v4", "123", "VARIATION"},
		{"123 R", "123", "REROLL"},
	} {
		id, action, ok := parseMidjourneySimpleChange(tc.content)
		if !ok || id != tc.id || action != tc.action {
			t.Fatalf("simple-change %q -> %q %q %v", tc.content, id, action, ok)
		}
	}
	for _, bad := range []string{"123", "123 U5", "123 X1", "123 U"} {
		if _, _, ok := parseMidjourneySimpleChange(bad); ok {
			t.Fatalf("simple-change %q should not parse", bad)
		}
	}
	if midjourneyModelName("SWAP_FACE") != "swap_face" || midjourneyModelName("IMAGINE") != "mj_imagine" || midjourneyModelName("CUSTOM_ZOOM") != "mj_custom_zoom" {
		t.Fatalf("model naming diverged from new-api CovertMjpActionToModelName")
	}
}

func TestMidjourneySwapFaceAndUploadPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	resetMJMemory(t)
	up := newFakeMJ(t, func(w http.ResponseWriter, r *http.Request, call mjCall) {
		if call.path == "/mj/submit/upload-discord-images" {
			_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":["https://cdn.example/u.png"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":1,"result":"swap-1"}`))
	})
	createMJChannel(t, ctx, "mj", up.srv.URL, []string{"k"}, "swap_face,mj_upload", 1, outbound.OutboundTypeOpenAIChat)
	if rec := mjSubmit(MidjourneyModeSwapFace, `{"sourceBase64":"data:image/png;base64,AA","targetBase64":"data:image/png;base64,BB"}`, 0, 0); rec.Code != http.StatusOK {
		t.Fatalf("swap: %d %s", rec.Code, rec.Body.String())
	}
	rec := mjSubmit(MidjourneyModeUpload, `{"base64Array":["data:image/png;base64,AA"]}`, 0, 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://cdn.example/u.png") {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	calls := up.Calls()
	if len(calls) != 2 || calls[0].path != "/mj/insight-face/swap" || calls[1].path != "/mj/submit/upload-discord-images" {
		t.Fatalf("unexpected upstream paths: %+v", calls)
	}
	if row, _ := op.MidjourneyTaskGetByUpstreamID(ctx, "swap-1"); row == nil || row.Model != "swap_face" {
		t.Fatalf("swap task owner not recorded: %+v", row)
	}
}
