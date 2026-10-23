package task

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// setupImageTaskPollTestDB 建临时库 + 载缓存。执行器每一轮都读库写库，
// 没有真库就无从验证。
func setupImageTaskPollTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}

// imageTaskUpstreamStub 是假图片上游：按需返回成功/失败，并统计并发。
type imageTaskUpstreamStub struct {
	mu sync.Mutex

	status int
	body   string

	hits int

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	hold        time.Duration
	paths       []string
}

func newImageTaskUpstreamStub() *imageTaskUpstreamStub {
	return &imageTaskUpstreamStub{
		status: http.StatusOK,
		body:   `{"created":1700000000,"data":[{"url":"https://images.upstream.example/ok.png"}]}`,
	}
}

func (s *imageTaskUpstreamStub) setResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.body = body
}

func (s *imageTaskUpstreamStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// pathList 返回上游被请求过的路径（带锁拷贝，避免测试与 handler 的读竞态）。
func (s *imageTaskUpstreamStub) pathList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

func (s *imageTaskUpstreamStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.paths = append(s.paths, r.URL.Path)
		status, body, hold := s.status, s.body, s.hold
		s.mu.Unlock()

		current := s.inFlight.Add(1)
		for {
			max := s.maxInFlight.Load()
			if current <= max || s.maxInFlight.CompareAndSwap(max, current) {
				break
			}
		}
		defer s.inFlight.Add(-1)
		if hold > 0 {
			time.Sleep(hold)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func createImageTaskPollTestChannel(t *testing.T, ctx context.Context, baseURL string) model.Channel {
	t.Helper()
	channel := model.Channel{
		Name:     "image-task-poll-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "image-poll-test-model",
		BaseUrls: []model.BaseUrl{{URL: baseURL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "image-poll-secret"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if len(channel.Keys) == 0 || channel.Keys[0].ID == 0 {
		t.Fatalf("channel key id not assigned: %+v", channel.Keys)
	}
	return channel
}

func createImageTaskPollTestUser(t *testing.T, ctx context.Context, balance float64) model.User {
	t.Helper()
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "image-task-poll-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")),
		Password: "test-password-123",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
		Balance:  balance,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

func newImageTaskPollTestTask(t *testing.T, ctx context.Context, channel model.Channel, user model.User, taskID string, cost float64, ttl time.Duration) model.ImageTask {
	t.Helper()
	now := time.Now()
	task := model.ImageTask{
		TaskID:       taskID,
		CreatedAt:    now.Unix(),
		ExpiresAt:    now.Add(ttl).Unix(),
		Status:       model.ImageTaskStatusProcessing,
		OwnerTokenID: 11,
		OwnerUserID:  user.ID,
		ChannelID:    channel.ID,
		ChannelKeyID: channel.Keys[0].ID,
		Model:        "image-poll-test-model",
		Endpoint:     model.ImageTaskEndpointGenerations,
		RequestBody:  `{"model":"image-poll-test-model","prompt":"a quiet lake"}`,
		Cost:         cost,
	}
	if err := op.ImageTaskCreate(ctx, &task); err != nil {
		t.Fatalf("create image task: %v", err)
	}
	return task
}

func reloadImageTask(t *testing.T, ctx context.Context, taskID string) model.ImageTask {
	t.Helper()
	var task model.ImageTask
	if err := db.GetDB().WithContext(ctx).Where("task_id = ?", taskID).First(&task).Error; err != nil {
		t.Fatalf("reload image task %s: %v", taskID, err)
	}
	return task
}

func reloadImageTaskUser(t *testing.T, userID int) model.User {
	t.Helper()
	user, err := op.UserGet(userID)
	if err != nil {
		t.Fatalf("reload user %d: %v", userID, err)
	}
	return user
}

// 【计费第 2 条】完成不再扣：预扣的那一单就是这一单的钱，执行成功不许再扣一次。
func TestImageTaskPollSuccessDoesNotChargeAgain(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)
	stub := newImageTaskUpstreamStub()
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	// 模拟提交时的预扣：1.0 → 0.5。
	if err := op.UserRecordUsage(user.ID, 0.5, ctx); err != nil {
		t.Fatalf("pre-charge: %v", err)
	}
	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_poll_success", 0.5, time.Hour)
	if _, err := op.ImageTaskReservationTake(ctx, task.TaskID, user.ID); err != nil {
		t.Fatalf("take reservation: %v", err)
	}

	runImageTaskPollOnce()

	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusCompleted {
		t.Fatalf("status = %q, want completed (error_message=%q)", stored.Status, stored.ErrorMessage)
	}
	if stored.CompletedAt <= 0 {
		t.Fatalf("completed_at = %d, want > 0", stored.CompletedAt)
	}
	if !strings.Contains(stored.ResultJSON, "images.upstream.example/ok.png") {
		t.Fatalf("result_json lost the upstream payload: %q", stored.ResultJSON)
	}
	if stored.Cost != 0.5 {
		t.Fatalf("cost = %v, want 0.5 (a completed task keeps the pre-charge)", stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 0.5 {
		t.Fatalf("balance = %v, want 0.5 (completion must not charge again)", got)
	}
	if stub.hitCount() != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1", stub.hitCount())
	}
	if paths := stub.pathList(); len(paths) != 1 || paths[0] != "/v1/images/generations" {
		t.Fatalf("upstream path = %v, want /v1/images/generations", paths)
	}
}

// 【计费第 3 条 + 预留释放（失败路径）】上游失败 → 标 failed、退款、释放预留。
func TestImageTaskPollFailureRefundsAndReleases(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)
	stub := newImageTaskUpstreamStub()
	stub.setResponse(http.StatusInternalServerError, `{"error":{"message":"upstream exploded"}}`)
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	if err := op.UserRecordUsage(user.ID, 0.5, ctx); err != nil {
		t.Fatalf("pre-charge: %v", err)
	}
	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_poll_failure", 0.5, time.Hour)
	if _, err := op.ImageTaskReservationTake(ctx, task.TaskID, user.ID); err != nil {
		t.Fatalf("take reservation: %v", err)
	}

	runImageTaskPollOnce()

	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusFailed {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if stored.ErrorMessage == "" {
		t.Fatalf("error_message is empty; the client needs a reason")
	}
	if !strings.Contains(stored.ErrorMessage, "500") {
		t.Fatalf("error_message = %q, want the upstream status in it", stored.ErrorMessage)
	}
	if stored.Cost != 0 {
		t.Fatalf("cost = %v, want 0 (refunded)", stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 1.0 {
		t.Fatalf("balance = %v, want 1.0 (the pre-charge must be refunded)", got)
	}
	// 预留已释放（幂等账本里再取不到这一笔）。
	if op.ImageTaskReservationRelease(task.TaskID) {
		t.Fatalf("the executor did not release the in-flight reservation")
	}

	// 退款幂等：再跑一轮也不会退第二次（任务已不在 processing）。
	runImageTaskPollOnce()
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 1.0 {
		t.Fatalf("balance after a second round = %v, want 1.0 (no double refund)", got)
	}
}

// 【计费第 3 条 + 预留释放（过期路径）】到 expires_at 还没完成 → 标 failed、
// 原因是契约里指定的那句话、退款、释放预留。
func TestImageTaskPollExpiredMarksFailedAndRefunds(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)
	stub := newImageTaskUpstreamStub()
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	if err := op.UserRecordUsage(user.ID, 0.5, ctx); err != nil {
		t.Fatalf("pre-charge: %v", err)
	}
	// ttl 取负：这条任务在出生那一刻就已经过期（模拟执行器停机很久之后重启）。
	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_poll_expired", 0.5, -time.Minute)
	if _, err := op.ImageTaskReservationTake(ctx, task.TaskID, user.ID); err != nil {
		t.Fatalf("take reservation: %v", err)
	}

	runImageTaskPollOnce()

	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusFailed {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if stored.ErrorMessage != "task expired before completion" {
		t.Fatalf("error_message = %q, want %q", stored.ErrorMessage, "task expired before completion")
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 1.0 {
		t.Fatalf("balance = %v, want 1.0 (an expired task must be refunded)", got)
	}
	if op.ImageTaskReservationRelease(task.TaskID) {
		t.Fatalf("the sweep did not release the in-flight reservation")
	}
	// 过期任务不许被执行（白花一次上游调用，而且客户端早就按 404 处理了）。
	if stub.hitCount() != 0 {
		t.Fatalf("expired task hit the upstream %d time(s), want 0", stub.hitCount())
	}
}

// 【契约第三节第 5 条 / 第四节第 6 条】在途预留必须在**所有**终态路径释放。
//
// 断言方式是行为级的而不是读数级的：把余额压到"刚够一次预留"，
// 提交后这笔钱被任务占住（此时再借必失败），任务进终态后必须又能借出来。
// 三条路径（成功/失败/过期）各跑一遍。
func TestImageTaskPollReleasesReservationOnAllTerminalPaths(t *testing.T) {
	const poorBalance = 0.0001 // 远小于一个预留量子，保证"占满"与"能再借"可区分

	cases := []struct {
		name       string
		ttl        time.Duration
		status     int
		body       string
		wantStatus string
		wantUpHits int
	}{
		{
			name:       "success",
			ttl:        time.Hour,
			status:     http.StatusOK,
			body:       `{"data":[{"url":"https://images.upstream.example/ok.png"}]}`,
			wantStatus: model.ImageTaskStatusCompleted,
			wantUpHits: 1,
		},
		{
			name:       "failure",
			ttl:        time.Hour,
			status:     http.StatusBadGateway,
			body:       `{"error":{"message":"bad gateway"}}`,
			wantStatus: model.ImageTaskStatusFailed,
			wantUpHits: 1,
		},
		{
			name:       "expired",
			ttl:        -time.Minute,
			status:     http.StatusOK,
			body:       `{"data":[{"url":"https://images.upstream.example/ok.png"}]}`,
			wantStatus: model.ImageTaskStatusFailed,
			wantUpHits: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupImageTaskPollTestDB(t)
			stub := newImageTaskUpstreamStub()
			stub.setResponse(tc.status, tc.body)
			channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
			user := createImageTaskPollTestUser(t, ctx, poorBalance)

			task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_reservation_"+tc.name, 0, tc.ttl)

			reservation, err := op.ImageTaskReservationTake(ctx, task.TaskID, user.ID)
			if err != nil {
				t.Fatalf("take reservation: %v", err)
			}
			if reservation <= 0 {
				t.Fatalf("reservation = %v, want > 0 (the test needs a real hold)", reservation)
			}
			// 占住之后必须借不出第二笔 —— 否则下面的"又能借"证明不了任何事。
			if _, err := op.UserCanRelay(user.ID, ctx); err == nil {
				t.Fatalf("a second borrow succeeded while the task was still running: nothing is being held")
			}

			runImageTaskPollOnce()

			stored := reloadImageTask(t, ctx, task.TaskID)
			if stored.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error_message=%q)", stored.Status, tc.wantStatus, stored.ErrorMessage)
			}
			if stub.hitCount() != tc.wantUpHits {
				t.Fatalf("upstream hits = %d, want %d", stub.hitCount(), tc.wantUpHits)
			}

			// 终态之后必须能再借：这就是"在途额度被释放了"的可观测证据。
			again, err := op.UserCanRelay(user.ID, ctx)
			if err != nil {
				t.Fatalf("borrow after the task reached %q failed: %v (the in-flight reservation was never released)", stored.Status, err)
			}
			op.UserReleaseQuota(user.ID, again)
		})
	}
}

// 【契约第三节第 2 条后半】并发上限：同时执行的上游请求不许超过配置值。
//
// 理由写在 imageTaskRunConcurrency 上：图床上游按 key 限速，开太多只会集体 429，
// 用户看到的是"全都失败"而不是"慢一点"。
func TestImageTaskPollRespectsConcurrencyLimit(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)

	previous := imageTaskRunConcurrency
	imageTaskRunConcurrency = 2
	t.Cleanup(func() { imageTaskRunConcurrency = previous })

	stub := newImageTaskUpstreamStub()
	stub.hold = 40 * time.Millisecond
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 10)

	for i := 0; i < 4; i++ {
		newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_concurrency_"+string(rune('a'+i)), 0, time.Hour)
	}

	runImageTaskPollOnce()

	if got := stub.inFlight.Load(); got != 0 {
		t.Fatalf("in-flight after the round = %d, want 0 (the round must wait for its batch)", got)
	}
	if max := stub.maxInFlight.Load(); max > int32(imageTaskRunConcurrency) {
		t.Fatalf("max concurrent upstream calls = %d, want <= %d", max, imageTaskRunConcurrency)
	}
	if stub.hitCount() != 4 {
		t.Fatalf("upstream hits = %d, want 4 (the batch must still be fully executed)", stub.hitCount())
	}
	executable, err := op.ImageTaskListExecutable(ctx, time.Now().Unix(), 10)
	if err != nil {
		t.Fatalf("list executable: %v", err)
	}
	if len(executable) != 0 {
		t.Fatalf("%d task(s) still processing after a full round", len(executable))
	}
}

// 【契约第四节第 8 条】请求体（含 prompt）绝不许出现在任何日志里 —— 包括 Debugf。
//
// 断言方式：把 log.Logger 换成一个写进内存 buffer 的 logger，然后跑一遍真链路
// （提交后的执行器 = 唯一会碰到 RequestBody 的地方），最后在整个日志文本里找
// 那个 prompt 标记。同时断言 buffer 非空，否则"日志里没有 prompt"只是没记日志。
func TestImageTaskPollNeverLogsRequestBody(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)

	var buf bytes.Buffer
	var bufMu sync.Mutex
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
			MessageKey:  "msg",
			LevelKey:    "level",
			EncodeLevel: zapcore.CapitalLevelEncoder,
		}),
		zapcore.AddSync(writerFunc(func(p []byte) (int, error) {
			bufMu.Lock()
			defer bufMu.Unlock()
			return buf.Write(p)
		})),
		zapcore.DebugLevel,
	)
	previousLogger := log.Logger
	log.Logger = zap.New(core).Sugar()
	t.Cleanup(func() { log.Logger = previousLogger })

	const promptMarker = "SECRET-PROMPT-MARKER-9f3a1c"
	stub := newImageTaskUpstreamStub()
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_log_redaction", 0, time.Hour)
	// 把标记写进请求体（与提交时存的原文同一个位置）。
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).
		Update("request_body", `{"model":"image-poll-test-model","prompt":"`+promptMarker+`"}`).Error; err != nil {
		t.Fatalf("seed request body: %v", err)
	}

	runImageTaskPollOnce()

	bufMu.Lock()
	captured := buf.String()
	bufMu.Unlock()

	if captured == "" {
		t.Fatalf("no log output captured; this assertion would be vacuously true")
	}
	if strings.Contains(captured, promptMarker) {
		t.Fatalf("the request body (with the user prompt) leaked into the logs:\n%s", captured)
	}
	if strings.Contains(captured, "a quiet lake") {
		t.Fatalf("the stored prompt leaked into the logs:\n%s", captured)
	}

	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusCompleted {
		t.Fatalf("status = %q, want completed (error_message=%q)", stored.Status, stored.ErrorMessage)
	}
	if !strings.Contains(stored.RequestBody, promptMarker) {
		t.Fatalf("precondition broken: the request body no longer holds the marker")
	}
}

// writerFunc 让一个 func([]byte) 满足 io.Writer（zapcore.AddSync 需要它）。
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// 执行器把"2xx 但回来的不是 JSON"当成功是很危险的：result_json 会是垃圾，
// 客户端拿到一个 completed 却什么都解析不出来。这里锁死它被判失败并退款。
//
// 注意反过来不成立：合法的 JSON 即使没有 url（例如上游只回 b64_json）也算完成，
// 所以判据是"有没有可用的结果形状"，不是"有没有 image_url"。
func TestImageTaskPollTreatsUnparseableResultAsFailure(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)
	stub := newImageTaskUpstreamStub()
	stub.setResponse(http.StatusOK, "<!doctype html><html><body>blocked by upstream</body></html>")
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	if err := op.UserRecordUsage(user.ID, 0.5, ctx); err != nil {
		t.Fatalf("pre-charge: %v", err)
	}
	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_unparseable_result", 0.5, time.Hour)

	runImageTaskPollOnce()

	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusFailed {
		t.Fatalf("status = %q, want failed for a 2xx response that is not a usable image result", stored.Status)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 1.0 {
		t.Fatalf("balance = %v, want 1.0 (refunded)", got)
	}
}

// 执行器不许改同步图片端点的任何东西：它只通过 relay.ImageTaskRunUpstream 出站。
// 这条用例守的是"edits 任务必须回到 edits 子路径"（上游不会把 edits 挂在
// generations 下面，走错就是必然 404）。
func TestImageTaskPollUsesRecordedEndpoint(t *testing.T) {
	ctx := setupImageTaskPollTestDB(t)
	stub := newImageTaskUpstreamStub()
	channel := createImageTaskPollTestChannel(t, ctx, stub.server(t).URL)
	user := createImageTaskPollTestUser(t, ctx, 1.0)

	task := newImageTaskPollTestTask(t, ctx, channel, user, "imgtask_edits_endpoint", 0, time.Hour)
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).
		Update("endpoint", model.ImageTaskEndpointEdits).Error; err != nil {
		t.Fatalf("set endpoint: %v", err)
	}

	runImageTaskPollOnce()

	if paths := stub.pathList(); len(paths) != 1 || paths[0] != "/v1/images/edits" {
		t.Fatalf("upstream path = %v, want /v1/images/edits", paths)
	}
}
