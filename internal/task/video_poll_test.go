package task

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// setupVideoPollTestDB 建临时库 + 载缓存。轮询器每一轮都读库写库，没有真库就无从验证。
func setupVideoPollTestDB(t *testing.T) context.Context {
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

func createVideoPollTestChannel(t *testing.T, ctx context.Context, baseURL string) model.Channel {
	t.Helper()
	return createVideoPollTestChannelNamed(t, ctx, "video-poll-channel", baseURL)
}

// createVideoPollTestChannelNamed 给需要多个渠道的用例用：channels.name 上有唯一约束，
// 同一个库里的两个渠道不能同名。
func createVideoPollTestChannelNamed(t *testing.T, ctx context.Context, name, baseURL string) model.Channel {
	t.Helper()
	channel := model.Channel{
		Name:     name,
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "video-upstream-model",
		BaseUrls: []model.BaseUrl{{URL: baseURL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "video-poll-secret"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if len(channel.Keys) == 0 || channel.Keys[0].ID == 0 {
		t.Fatalf("channel key id not assigned: %+v", channel.Keys)
	}
	return channel
}

func createVideoPollTestTask(t *testing.T, ctx context.Context, task model.VideoTask) model.VideoTask {
	t.Helper()
	if err := op.VideoTaskCreate(ctx, &task); err != nil {
		t.Fatalf("create video task: %v", err)
	}
	return task
}

func loadVideoPollTestTask(t *testing.T, ctx context.Context, upstreamID string) model.VideoTask {
	t.Helper()
	task, err := op.VideoTaskGetByUpstreamID(ctx, upstreamID)
	if err != nil {
		t.Fatalf("read back task %s: %v", upstreamID, err)
	}
	if task == nil {
		t.Fatalf("task %s not found", upstreamID)
	}
	return *task
}

// upstreamStub 是一个可换响应体的假上游，用来把「上游说 in_progress」和
// 「上游说 success」放在同一轮基准里对比。
type upstreamStub struct {
	mu       sync.Mutex
	status   map[string]int
	body     map[string]string
	hits     map[string]int
	authSeen map[string]string
	pathSeen map[string]string
	hang     map[string]bool
}

func newUpstreamStub() *upstreamStub {
	return &upstreamStub{
		status:   make(map[string]int),
		body:     make(map[string]string),
		hits:     make(map[string]int),
		authSeen: make(map[string]string),
		pathSeen: make(map[string]string),
		hang:     make(map[string]bool),
	}
}

func (s *upstreamStub) set(id string, statusCode int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = statusCode
	s.body[id] = body
}

// setHang 让这个上游任务 id 的查询「连得上、握手完，但永远不返回」。
// 这是「一条慢查询拖死整轮」的真实形态：即时 500 或连接失败都会立刻返回，
// 覆盖不到「吊着」这条路径。
func (s *upstreamStub) setHang(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang[id] = true
}

func (s *upstreamStub) hitCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[id]
}

func (s *upstreamStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
		s.mu.Lock()
		code, ok := s.status[id]
		body := s.body[id]
		hung := s.hang[id]
		s.hits[id]++
		s.authSeen[id] = r.Header.Get("Authorization")
		s.pathSeen[id] = r.URL.Path
		s.mu.Unlock()
		if hung {
			// 吊到客户端放弃为止（ctx 结束）：不发任何字节，也不是连接错误。
			<-r.Context().Done()
			return
		}
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such task"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
}

// 一轮轮询必须把上游的真实状态搬回库里：先 in_progress（含进度），
// 再 success（写 result_url + finish_time）。这是轮询器存在的理由。
func TestVideoTaskPollAdvancesStatusFromUpstream(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	keyID := channel.Keys[0].ID

	stub.set("cgt-1", http.StatusOK, `{"id":"cgt-1","status":"in_progress","progress":"30%"}`)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-1",
		ChannelID:      channel.ID,
		ChannelKeyID:   keyID,
		Model:          "video-upstream-model",
		Status:         model.VideoTaskSubmitted,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	first := loadVideoPollTestTask(t, ctx, "cgt-1")
	if first.Status != model.VideoTaskInProgress {
		t.Fatalf("status after first round = %q, want %q", first.Status, model.VideoTaskInProgress)
	}
	if first.Progress != "30%" {
		t.Fatalf("progress = %q, want 30%%", first.Progress)
	}
	if got := stub.hitCount("cgt-1"); got != 1 {
		t.Fatalf("upstream polled %d times in one round, want 1", got)
	}
	// 轮询必须带上这把 key 的凭据：查别人的任务等于查不到。
	stub.mu.Lock()
	auth := stub.authSeen["cgt-1"]
	path := stub.pathSeen["cgt-1"]
	stub.mu.Unlock()
	if auth != "Bearer video-poll-secret" {
		t.Fatalf("upstream auth header = %q", auth)
	}
	if path != "/v1/videos/cgt-1" {
		t.Fatalf("upstream poll path = %q", path)
	}

	stub.set("cgt-1", http.StatusOK, `{"id":"cgt-1","status":"success","video_url":"https://example.com/out.mp4"}`)
	runVideoTaskPollOnce()

	final := loadVideoPollTestTask(t, ctx, "cgt-1")
	if final.Status != model.VideoTaskSuccess {
		t.Fatalf("status after success round = %q, want %q", final.Status, model.VideoTaskSuccess)
	}
	if final.ResultURL != "https://example.com/out.mp4" {
		t.Fatalf("result url = %q", final.ResultURL)
	}
	if final.FinishTime == 0 {
		t.Fatalf("finish_time not written for a terminal task")
	}
	if final.FailReason != "" {
		t.Fatalf("fail_reason should be empty on success, got %q", final.FailReason)
	}
}

// 超时扫描的基础语义：问不出结论的超时任务必须被收口成 failure 并写明原因，
// 否则一条永远不收敛的任务会把「未完成」集合钉死。
//
// 这里刻意用 5xx：查询本身失败属于「问不出结论」那一支，仍按超时判失败。
// 「上游其实已经成功 / 还在跑」的两种情形见下面两个用例——它们是这个用例的反面。
func TestVideoTaskPollSweepsTimedOutTask(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-stale", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-stale",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskInProgress,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})

	runVideoTaskPollOnce()

	stale := loadVideoPollTestTask(t, ctx, "cgt-stale")
	if stale.Status != model.VideoTaskFailure {
		t.Fatalf("timed-out task status = %q, want %q", stale.Status, model.VideoTaskFailure)
	}
	if !strings.Contains(stale.FailReason, "timed out") {
		t.Fatalf("fail reason must explain the timeout, got %q", stale.FailReason)
	}
	if stale.FinishTime == 0 {
		t.Fatalf("finish_time not written for a timed-out task")
	}
	// 判死之前必须问过一次上游（而且只问这一次）：收口后它已进终态，
	// 本轮的「未完成」批次不该再捞它。
	if got := stub.hitCount("cgt-stale"); got != 1 {
		t.Fatalf("timed-out task polled %d time(s) in one round, want exactly the one last look", got)
	}
}

// submit_time 为 0 的老行/导入行必须退回 created_at 判超时，不能因为缺 submit_time 就永不超时。
func TestVideoTaskPollTimeoutFallsBackToCreatedAt(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	// 上游 5xx：轮询主路径把它当可恢复错误（不写库），超时收口把它当「问不出结论」
	// 而判超时失败。于是「有没有用 created_at 判超时」这件事只会体现在 fail_reason 上，
	// 不会被「上游 404 / 上游说成功」之类的旁路结果蒙对。
	stub.set("cgt-no-submit-time", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	task := createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-no-submit-time",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     0,
	})
	// 把 created_at 拨回 3 小时前：模拟一条从别处导入、没有 submit_time 的老行。
	if err := db.GetDB().Exec("UPDATE video_tasks SET created_at = ? WHERE id = ?", time.Now().Add(-3*time.Hour).Unix(), task.ID).Error; err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}

	runVideoTaskPollOnce()

	stored := loadVideoPollTestTask(t, ctx, "cgt-no-submit-time")
	if stored.Status != model.VideoTaskFailure || !strings.Contains(stored.FailReason, "timed out") {
		t.Fatalf("expected timeout failure via created_at fallback, got %q (%s)", stored.Status, stored.FailReason)
	}
}

// overrideVideoTaskPollTaskTimeout 把单条上游时限压到毫秒级。
// 「上游吊着不返回」的用例否则必须真等 20 秒（默认值见 video_poll.go）。
func overrideVideoTaskPollTaskTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := videoTaskPollTaskTimeout
	videoTaskPollTaskTimeout = d
	t.Cleanup(func() { videoTaskPollTaskTimeout = previous })
}

// 【缺陷 1 - 用例 1】本地早就超时，但上游其实已经生成成功。
//
// 真实场景：用户提交后服务停机/重启/长时间断网，期间上游把片生成好了；两小时后重启，
// 本地还停在 submitted。旧行为：超时扫描不查上游直接写 timeout failure →
// op.VideoTaskListUnfinished 把 failure 排除在外 → 结果地址永远补不回来，
// 用户付过费却看到永久失败，甚至再付一次重做。
// 新行为：先问上游，按它给的成功结果落库（带结果地址），绝不写 failure。
func TestVideoTaskPollTimeoutKeepsUpstreamSuccess(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-late-success", http.StatusOK,
		`{"id":"cgt-late-success","status":"success","video_url":"https://example.com/late.mp4"}`)

	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-late-success",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskSubmitted,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})

	runVideoTaskPollOnce()

	stored := loadVideoPollTestTask(t, ctx, "cgt-late-success")
	if stored.Status == model.VideoTaskFailure {
		t.Fatalf("a task the upstream already finished was killed as timed out (reason %q)", stored.FailReason)
	}
	if stored.Status != model.VideoTaskSuccess {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskSuccess)
	}
	if stored.ResultURL != "https://example.com/late.mp4" {
		t.Fatalf("result url = %q, want the upstream one (a paid result that is never handed back)", stored.ResultURL)
	}
	if stored.FailReason != "" {
		t.Fatalf("fail_reason must stay empty on an upstream success, got %q", stored.FailReason)
	}
	if stored.FinishTime == 0 {
		t.Fatalf("finish_time not written for the recovered task")
	}
	if got := stub.hitCount("cgt-late-success"); got != 1 {
		t.Fatalf("upstream queried %d time(s) in one round, want exactly the one last look", got)
	}
	unfinished, err := op.VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("recovered task still in the unfinished set: %+v", unfinished)
	}
}

// 【缺陷 1 - 用例 2】本地超时，但上游说它还在跑。
//
// 上游还在干活，本地按时间判死就是错的：判 failure 会把这一行踢出未完成集合，
// 结果地址再也补不回来。正确行为是推进到上游的真实状态、继续留在未完成集合里。
func TestVideoTaskPollTimeoutKeepsRunningTaskAlive(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-late-running", http.StatusOK,
		`{"id":"cgt-late-running","status":"in_progress","progress":"80%"}`)

	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-late-running",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskSubmitted,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})

	runVideoTaskPollOnce()

	stored := loadVideoPollTestTask(t, ctx, "cgt-late-running")
	if stored.Status == model.VideoTaskFailure {
		t.Fatalf("upstream still working, the row must not be judged failed: %q", stored.FailReason)
	}
	if stored.Status != model.VideoTaskInProgress || stored.Progress != "80%" {
		t.Fatalf("status/progress = %q/%q, want %q/80%% from the upstream", stored.Status, stored.Progress, model.VideoTaskInProgress)
	}
	if strings.Contains(stored.FailReason, "timed out") {
		t.Fatalf("a running task must not carry a timeout reason: %q", stored.FailReason)
	}
	// 这一轮只问了一次：超时收口问过之后，同一轮不该再问一遍。
	if got := stub.hitCount("cgt-late-running"); got != 1 {
		t.Fatalf("upstream queried %d time(s) in one round, want 1", got)
	}
	if unfinished, err := op.VideoTaskListUnfinished(ctx, 10); err != nil {
		t.Fatalf("list unfinished: %v", err)
	} else if len(unfinished) != 1 || unfinished[0].UpstreamTaskID != "cgt-late-running" {
		t.Fatalf("still-running task dropped from the unfinished set: %+v", unfinished)
	}

	// 第二轮也不许「先留一轮、再判死」。
	runVideoTaskPollOnce()
	again := loadVideoPollTestTask(t, ctx, "cgt-late-running")
	if again.Status == model.VideoTaskFailure {
		t.Fatalf("second round judged a still-running task failed: %q", again.FailReason)
	}
	if again.Status != model.VideoTaskInProgress {
		t.Fatalf("second round status = %q, want %q", again.Status, model.VideoTaskInProgress)
	}
}

// 【缺陷 1 - 用例 4】修过头就完了：上游问不出结论时必须**仍然**按超时判失败，
// 否则一条永远问不动的行会永久钉在未完成集合里，每轮白烧一次上游查询。
// 三种「问不出结论」都要覆盖：查询本身失败（5xx）、上游不认这个任务（404）、根本不可达。
func TestVideoTaskPollTimeoutFailsWhenUpstreamGivesNoAnswer(t *testing.T) {
	ctx := setupVideoPollTestDB(t)

	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-stale-5xx", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	// cgt-stale-404 刻意不注册：stub 对未知 id 回 404「no such task」。

	// 根本不可达：起一个 server 立刻关掉，端口上没人监听。
	dead := httptest.NewServer(stub.handler())
	deadURL := dead.URL
	dead.Close()
	deadChannel := createVideoPollTestChannelNamed(t, ctx, "video-poll-channel-unreachable", deadURL)

	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-stale-5xx",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-stale-404",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-stale-unreachable",
		ChannelID:      deadChannel.ID,
		ChannelKeyID:   deadChannel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
	})

	runVideoTaskPollOnce()

	for _, upstreamID := range []string{"cgt-stale-5xx", "cgt-stale-404", "cgt-stale-unreachable"} {
		stored := loadVideoPollTestTask(t, ctx, upstreamID)
		if stored.Status != model.VideoTaskFailure {
			t.Fatalf("%s: status = %q, want %q (an unanswerable task must still time out)", upstreamID, stored.Status, model.VideoTaskFailure)
		}
		if !strings.Contains(stored.FailReason, "timed out") {
			t.Fatalf("%s: fail reason = %q, want the timeout explanation", upstreamID, stored.FailReason)
		}
	}
}

// 单条失败不许拖垮整批：三条任务，中间那条上游 500，其余两条必须照常更新。
// 中间那条保持原状（可恢复错误留给下一轮 + 超时扫描收口），不许被顺手标失败。
func TestVideoTaskPollIsolatesSingleTaskFailure(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	keyID := channel.Keys[0].ID

	stub.set("cgt-first", http.StatusOK, `{"id":"cgt-first","status":"in_progress"}`)
	stub.set("cgt-broken", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	stub.set("cgt-last", http.StatusOK, `{"id":"cgt-last","status":"success","video_url":"https://example.com/last.mp4"}`)

	seed := []struct {
		id     string
		status model.VideoTaskStatus
	}{
		{"cgt-first", model.VideoTaskQueued},
		{"cgt-broken", model.VideoTaskQueued},
		{"cgt-last", model.VideoTaskQueued},
	}
	for _, item := range seed {
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: item.id,
			ChannelID:      channel.ID,
			ChannelKeyID:   keyID,
			Status:         item.status,
			SubmitTime:     time.Now().Unix(),
		})
	}

	runVideoTaskPollOnce()

	if got := loadVideoPollTestTask(t, ctx, "cgt-first").Status; got != model.VideoTaskInProgress {
		t.Fatalf("task before the failing one = %q, want %q", got, model.VideoTaskInProgress)
	}
	broken := loadVideoPollTestTask(t, ctx, "cgt-broken")
	if broken.Status != model.VideoTaskQueued {
		t.Fatalf("500 is recoverable: status = %q, want unchanged %q", broken.Status, model.VideoTaskQueued)
	}
	last := loadVideoPollTestTask(t, ctx, "cgt-last")
	if last.Status != model.VideoTaskSuccess {
		t.Fatalf("task after the failing one = %q, want %q (batch stopped early)", last.Status, model.VideoTaskSuccess)
	}
	if last.ResultURL != "https://example.com/last.mp4" {
		t.Fatalf("result url after the failing one = %q", last.ResultURL)
	}
}

// 【缺陷 2】首条上游「连得上但一直不返回」，后面的健康任务必须照常被推进。
//
// 旧行为：单条查询共用整轮 2 分钟 ctx，第一条吊住就吃光预算，后面的任务全部被
// `if ctx.Err() != nil { return }` 跳过；下一轮 Order("id") 又从同一条开始，于是
// 一个上游故障能持续拖住其它任务。即时 500 的用例覆盖不到这一条（500 是立刻返回的）。
func TestVideoTaskPollSkipsHangingUpstreamAndProcessesRest(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	overrideVideoTaskPollTaskTimeout(t, 200*time.Millisecond)

	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	keyID := channel.Keys[0].ID

	stub.setHang("cgt-hang")
	stub.set("cgt-healthy", http.StatusOK,
		`{"id":"cgt-healthy","status":"success","video_url":"https://example.com/healthy.mp4"}`)

	// 挂起的那条先建：它的 id 更小，Order("id") 会把它排在最前面，
	// 正是「首条慢任务挡住后面所有任务」的布局。
	for _, upstreamID := range []string{"cgt-hang", "cgt-healthy"} {
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: upstreamID,
			ChannelID:      channel.ID,
			ChannelKeyID:   keyID,
			Status:         model.VideoTaskQueued,
			SubmitTime:     time.Now().Unix(),
		})
	}

	start := time.Now()
	runVideoTaskPollOnce()
	elapsed := time.Since(start)

	healthy := loadVideoPollTestTask(t, ctx, "cgt-healthy")
	if healthy.Status != model.VideoTaskSuccess {
		t.Fatalf("task behind the hanging one = %q, want %q: one slow upstream must not stop the batch", healthy.Status, model.VideoTaskSuccess)
	}
	if healthy.ResultURL != "https://example.com/healthy.mp4" {
		t.Fatalf("result url behind the hanging one = %q", healthy.ResultURL)
	}
	if elapsed >= videoTaskPollRoundTimeout/2 {
		t.Fatalf("the round took %s: a single hanging upstream ate the round budget", elapsed)
	}

	// 单条超时不许把任务判死：上游可能只是慢，留在未完成集合里下一轮再问。
	if got := stub.hitCount("cgt-hang"); got != 1 {
		t.Fatalf("hanging task polled %d time(s) in one round, want 1", got)
	}
	hang := loadVideoPollTestTask(t, ctx, "cgt-hang")
	if hang.Status != model.VideoTaskQueued {
		t.Fatalf("hanging task status = %q, want unchanged %q (a slow upstream is not a failure)", hang.Status, model.VideoTaskQueued)
	}
	if hang.FailReason != "" {
		t.Fatalf("hanging task got a fail reason: %q", hang.FailReason)
	}
}

// 上游返回不认识的状态：落 unknown（不是终态，继续轮询），并保留原始响应片段。
func TestVideoTaskPollRecordsUnknownStatus(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-weird", http.StatusOK, `{"id":"cgt-weird","status":"weird_status","detail":"upstream changed its mind"}`)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-weird",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	task := loadVideoPollTestTask(t, ctx, "cgt-weird")
	if task.Status != model.VideoTaskUnknown {
		t.Fatalf("status = %q, want %q", task.Status, model.VideoTaskUnknown)
	}
	if !strings.Contains(task.FailReason, "weird_status") {
		t.Fatalf("raw status snippet must be kept, got %q", task.FailReason)
	}
	if model.IsVideoTaskTerminal(task.Status) {
		t.Fatalf("unknown must not be terminal, otherwise the task freezes forever")
	}
	// 还要留在未完成集合里，下一轮继续问。
	unfinished, err := op.VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0].UpstreamTaskID != "cgt-weird" {
		t.Fatalf("unknown task dropped from the unfinished set: %+v", unfinished)
	}
}

// 任务指向一个已经不存在的渠道：一次判失败并写明原因，不能每轮都重试等着它回来。
func TestVideoTaskPollFailsTaskOnDeletedChannel(t *testing.T) {
	ctx := setupVideoPollTestDB(t)

	deletedChannelID := 4242
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-orphan-channel",
		ChannelID:      deletedChannelID,
		ChannelKeyID:   7,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	task := loadVideoPollTestTask(t, ctx, "cgt-orphan-channel")
	if task.Status != model.VideoTaskFailure {
		t.Fatalf("status = %q, want %q", task.Status, model.VideoTaskFailure)
	}
	if !strings.Contains(task.FailReason, "4242") {
		t.Fatalf("fail reason must name the missing channel, got %q", task.FailReason)
	}
	unfinished, err := op.VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("terminal task still in the unfinished set: %+v", unfinished)
	}

	// 第二轮不许再动它（它已经是终态，list 也不会再捞出来）。
	if err := db.GetDB().Exec("UPDATE video_tasks SET fail_reason = ? WHERE id = ?", "untouched", task.ID).Error; err != nil {
		t.Fatalf("mark: %v", err)
	}
	runVideoTaskPollOnce()
	if got := loadVideoPollTestTask(t, ctx, "cgt-orphan-channel").FailReason; got != "untouched" {
		t.Fatalf("a terminal task was rewritten by a later round: %q", got)
	}
}

// 渠道还在、但这把 key 已经不在了：同样一次判失败，不能无限重试。
func TestVideoTaskPollFailsTaskOnDeletedChannelKey(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-orphan-key",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID + 1000,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	task := loadVideoPollTestTask(t, ctx, "cgt-orphan-key")
	if task.Status != model.VideoTaskFailure {
		t.Fatalf("status = %q, want %q", task.Status, model.VideoTaskFailure)
	}
	if !strings.Contains(task.FailReason, "no longer exists") {
		t.Fatalf("fail reason must name the missing key, got %q", task.FailReason)
	}
	if got := stub.hitCount("cgt-orphan-key"); got != 0 {
		t.Fatalf("a task with no usable key must not hit the upstream, got %d", got)
	}
}

// 上游 404：上游自己都不认这个任务了，再问也是 404，直接收口。
func TestVideoTaskPollFailsTaskOnUpstream404(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-gone-upstream",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	task := loadVideoPollTestTask(t, ctx, "cgt-gone-upstream")
	if task.Status != model.VideoTaskFailure {
		t.Fatalf("status = %q, want %q", task.Status, model.VideoTaskFailure)
	}
	if !strings.Contains(task.FailReason, "404") {
		t.Fatalf("fail reason must record the upstream 404, got %q", task.FailReason)
	}
}

// 上游 status 到本层七值的映射：认识的原样落库，不认识的一律 unknown（不猜）。
func TestMapVideoTaskUpstreamStatus(t *testing.T) {
	cases := []struct {
		raw  string
		want model.VideoTaskStatus
	}{
		{"not_start", model.VideoTaskNotStart},
		{"submitted", model.VideoTaskSubmitted},
		{"queued", model.VideoTaskQueued},
		{"in_progress", model.VideoTaskInProgress},
		{" Processing ", model.VideoTaskInProgress},
		{"success", model.VideoTaskSuccess},
		{"completed", model.VideoTaskSuccess},
		{"failure", model.VideoTaskFailure},
		{"FAILED", model.VideoTaskFailure},
		{"weird_status", model.VideoTaskUnknown},
		{"", model.VideoTaskUnknown},
	}
	for _, tc := range cases {
		if got := mapVideoTaskUpstreamStatus(tc.raw); got != tc.want {
			t.Fatalf("map(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// 【铁律的机械证据】轮询器绝不许由 init() 自启。
//
// 三层证据：
//  1. 运行时：本测试二进制里 videoTaskPollStarts 仍是 0，说明没有任何包级代码
//     悄悄调用过 VideoTaskPollStart（下面那些 runVideoTaskPollOnce 的用例是显式
//     调用的，它们证明「跑得动」，不证明「被自动启动」）。
//  2. 静态：本包所有非测试文件里没有 func init()。
//  3. 接线：VideoTaskPollStart 在 init.go 的 Init 里被调用——生产启动路径，
//     测试二进制永远不会走到那里。
//
// 为什么不用「数 goroutine」：包内其它用例和 runtime 自己的协程数不稳定，那种断言会
// 随机红，红过一次就没人再信它了。
func TestVideoTaskPollStartIsNeverAutoStarted(t *testing.T) {
	if starts := videoTaskPollStarts.Load(); starts != 0 {
		t.Fatalf("poller already started %d time(s) inside the test binary; it must only start from an explicit call", starts)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if isFunc && fn.Name.Name == "init" && fn.Recv == nil {
				t.Errorf("%s declares func init(); package-level startup is exactly how a background DB writer leaks into every test binary", name)
			}
		}
	}

	initFile, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "init.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse init.go: %v", err)
	}
	var initFn *ast.FuncDecl
	for _, decl := range initFile.Decls {
		if fn, isFunc := decl.(*ast.FuncDecl); isFunc && fn.Name.Name == "Init" && fn.Recv == nil {
			initFn = fn
			break
		}
	}
	if initFn == nil || initFn.Body == nil {
		t.Fatal("func Init not found in init.go")
	}
	startCalls := 0
	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == "VideoTaskPollStart" {
			startCalls++
		}
		return true
	})
	if startCalls == 0 {
		t.Error("internal/task/init.go must call VideoTaskPollStart() from Init; production wiring is missing")
	}
}
