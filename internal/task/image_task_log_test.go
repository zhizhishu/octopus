package task

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 图片异步任务的历史日志：每条任务在终态写恰好一行，口径与同步图片日志一致——
// 请求内容、真实出站请求体（开关打开时原样、不打码）、结果、费用。

const imgLogSecret = "sk-live-imgasync-secret-0123456789"

type imgLogUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
	status int
	body   string
}

func (u *imgLogUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, b)
		status, body := u.status, u.body
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (u *imgLogUpstream) lastBody(t *testing.T) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("upstream received nothing")
	}
	return u.bodies[len(u.bodies)-1]
}

func setupImgLogTest(t *testing.T, bodyLogOn bool) context.Context {
	t.Helper()
	ctx := setupImageTaskPollTestDB(t)
	_ = op.RelayLogClear(ctx, nil)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })
	v := 0
	if bodyLogOn {
		v = 1
	}
	if err := op.SettingSetInt(model.SettingKeyUpstreamRequestBodyLog, v); err != nil {
		t.Fatalf("set body log toggle: %v", err)
	}
	return ctx
}

// newImgLogTask 建一条带日志元数据的任务（模拟提交时 ImagesAsyncSubmit 写下的那几列）。
func newImgLogTask(t *testing.T, ctx context.Context, upstreamURL, taskID string, cost float64, ttl time.Duration) (model.ImageTask, model.User) {
	t.Helper()
	channel := createImageTaskPollTestChannel(t, ctx, upstreamURL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)
	if cost > 0 {
		if err := op.UserRecordUsage(user.ID, cost, ctx); err != nil {
			t.Fatalf("pre-charge: %v", err)
		}
	}
	task := newImageTaskPollTestTask(t, ctx, channel, user, taskID, cost, ttl)
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).Updates(map[string]any{
		"request_body":       `{"model":"client-alias","prompt":"lake ` + imgLogSecret + `","size":"1024x1024"}`,
		"request_model":      "client-alias",
		"request_ip":         "203.0.113.7",
		"request_user_agent": "img-client/1.0",
	}).Error; err != nil {
		t.Fatalf("seed log metadata: %v", err)
	}
	return reloadImageTask(t, ctx, taskID), user
}

func imgLogRows(t *testing.T, ctx context.Context) []model.RelayLog {
	t.Helper()
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 20, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	out := make([]model.RelayLog, 0, len(logs))
	for _, l := range logs {
		full, err := op.RelayLogGetByID(ctx, l.ID, nil)
		if err != nil {
			t.Fatalf("get log %d: %v", l.ID, err)
		}
		out = append(out, *full)
	}
	return out
}

func TestImageTaskLogSuccessRecordsRequestUpstreamBodyResultAndCost(t *testing.T) {
	ctx := setupImgLogTest(t, true)
	up := &imgLogUpstream{status: http.StatusOK, body: `{"created":1700000000,"data":[{"url":"https://images.upstream.example/ok.png","b64_json":"QUJD"}]}`}
	task, user := newImgLogTask(t, ctx, up.server(t).URL, "imgtask_log_ok", 0.5, time.Hour)

	runImageTaskPollOnce()
	runImageTaskPollOnce() // 第二轮不能再写一行

	rows := imgLogRows(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("want exactly one log row per task, got %d", len(rows))
	}
	l := rows[0]
	if l.RequestEndpoint != "images_generations_async" || l.RequestPath != "/v1/images/generations/async" {
		t.Fatalf("endpoint/path = %q %q", l.RequestEndpoint, l.RequestPath)
	}
	if l.RequestModelName != "client-alias" || l.ActualModelName != task.Model {
		t.Fatalf("models = %q / %q", l.RequestModelName, l.ActualModelName)
	}
	if l.UserID != user.ID || l.APIKeyID != task.OwnerTokenID || l.RequestIP != "203.0.113.7" || l.RequestUserAgent != "img-client/1.0" {
		t.Fatalf("owner/ip/ua not carried over: %+v", l)
	}
	if l.Time != task.CreatedAt {
		t.Fatalf("log time = %d, want the submit time %d", l.Time, task.CreatedAt)
	}
	if l.ChannelId != task.ChannelID || l.ChannelName != "image-task-poll-channel" || l.TotalAttempts != 1 {
		t.Fatalf("channel/attempts = %d %q %d", l.ChannelId, l.ChannelName, l.TotalAttempts)
	}
	// 出站请求体 = 上游实际收到的字节，原样不打码。
	sent := up.lastBody(t)
	if l.UpstreamRequestBody != string(sent) {
		t.Fatalf("upstream body snapshot must equal the bytes sent\nlogged: %s\nsent:   %s", l.UpstreamRequestBody, sent)
	}
	if !strings.Contains(l.UpstreamRequestBody, imgLogSecret) || !strings.Contains(l.UpstreamRequestBody, `"model":"image-poll-test-model"`) {
		t.Fatalf("snapshot must be verbatim and carry the mapped model: %s", l.UpstreamRequestBody)
	}
	// 请求内容与同步图片同一口径（含 prompt，密钥形态的串按 redactLogBody 打码）。
	if !strings.Contains(l.RequestContent, "lake") || strings.Contains(l.RequestContent, imgLogSecret) {
		t.Fatalf("request content should match the sync images rule: %s", l.RequestContent)
	}
	// 结果：落库的 result_json（b64 已剔除）。
	if !strings.Contains(l.ResponseContent, "images.upstream.example/ok.png") || strings.Contains(l.ResponseContent, "QUJD") {
		t.Fatalf("response content = %s", l.ResponseContent)
	}
	if l.Cost != 0.5 || l.Error != "" {
		t.Fatalf("cost/error = %v %q, want 0.5 and no error", l.Cost, l.Error)
	}
	// 记日志不许再扣钱。
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 0.5 {
		t.Fatalf("balance = %v, want 0.5", got)
	}
}

func TestImageTaskLogToggleOffStoresNoBodyAndSendsSameBytes(t *testing.T) {
	sentWith := func(on bool) ([]byte, string) {
		ctx := setupImgLogTest(t, on)
		up := &imgLogUpstream{status: http.StatusOK, body: `{"data":[{"url":"https://images.upstream.example/x.png"}]}`}
		newImgLogTask(t, ctx, up.server(t).URL, "imgtask_log_toggle", 0, time.Hour)
		runImageTaskPollOnce()
		rows := imgLogRows(t, ctx)
		if len(rows) != 1 {
			t.Fatalf("want one row, got %d", len(rows))
		}
		return up.lastBody(t), rows[0].UpstreamRequestBody
	}
	offSent, offLogged := sentWith(false)
	onSent, onLogged := sentWith(true)
	if offLogged != "" {
		t.Fatalf("toggle off must not store the upstream body, got %s", offLogged)
	}
	if onLogged == "" {
		t.Fatal("toggle on must store the upstream body")
	}
	if string(offSent) != string(onSent) {
		t.Fatalf("snapshot must not change outbound bytes\noff: %s\non:  %s", offSent, onSent)
	}
}

func TestImageTaskLogFailureRecordsErrorAndZeroCost(t *testing.T) {
	ctx := setupImgLogTest(t, true)
	up := &imgLogUpstream{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream exploded"}}`}
	newImgLogTask(t, ctx, up.server(t).URL, "imgtask_log_fail", 0.5, time.Hour)

	runImageTaskPollOnce()

	rows := imgLogRows(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	l := rows[0]
	if l.ErrorStatus != http.StatusInternalServerError || !strings.Contains(l.Error, "500") {
		t.Fatalf("error = %d %q", l.ErrorStatus, l.Error)
	}
	if l.Cost != 0 {
		t.Fatalf("failed task is refunded, log cost must be 0, got %v", l.Cost)
	}
	if l.UpstreamRequestBody != string(up.lastBody(t)) {
		t.Fatalf("a failed call still went out; snapshot must equal what was sent")
	}
	if l.TotalAttempts != 1 || l.Attempts[0].Status != model.AttemptFailed {
		t.Fatalf("attempts = %+v", l.Attempts)
	}
}

func TestImageTaskLogExpiredTaskWritesOneRowWithoutUpstreamCall(t *testing.T) {
	ctx := setupImgLogTest(t, true)
	up := &imgLogUpstream{status: http.StatusOK, body: `{"data":[]}`}
	task, _ := newImgLogTask(t, ctx, up.server(t).URL, "imgtask_log_expired", 0.5, time.Hour)
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).
		Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatalf("expire task: %v", err)
	}

	runImageTaskPollOnce()

	rows := imgLogRows(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	l := rows[0]
	if !strings.Contains(l.Error, "expired") || l.Cost != 0 || l.UpstreamRequestBody != "" || l.TotalAttempts != 0 {
		t.Fatalf("expired row = error %q cost %v body %q attempts %d", l.Error, l.Cost, l.UpstreamRequestBody, l.TotalAttempts)
	}
	if l.RequestModelName != "client-alias" || !strings.Contains(l.RequestContent, "lake") {
		t.Fatalf("expired row should still show what was requested: %q %q", l.RequestModelName, l.RequestContent)
	}
	up.mu.Lock()
	hits := len(up.bodies)
	up.mu.Unlock()
	if hits != 0 {
		t.Fatalf("expired task must not call upstream, got %d hits", hits)
	}
}
