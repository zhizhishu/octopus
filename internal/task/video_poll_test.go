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
	channel := model.Channel{
		Name:     "video-poll-channel",
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
}

func newUpstreamStub() *upstreamStub {
	return &upstreamStub{
		status:   make(map[string]int),
		body:     make(map[string]string),
		hits:     make(map[string]int),
		authSeen: make(map[string]string),
		pathSeen: make(map[string]string),
	}
}

func (s *upstreamStub) set(id string, statusCode int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = statusCode
	s.body[id] = body
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
		s.hits[id]++
		s.authSeen[id] = r.Header.Get("Authorization")
		s.pathSeen[id] = r.URL.Path
		s.mu.Unlock()
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

// 超时扫描：提交时间早于阈值还在跑的任务必须被收口成 failure 并写明原因，
// 否则一条永远不收敛的任务会把「未完成」集合钉死。
func TestVideoTaskPollSweepsTimedOutTask(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	stub.set("cgt-stale", http.StatusOK, `{"id":"cgt-stale","status":"in_progress"}`)

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
	// 已经收口的任务不该再被捞去打一次上游。
	if got := stub.hitCount("cgt-stale"); got != 0 {
		t.Fatalf("timed-out task still polled %d time(s) upstream in the same round", got)
	}
}

// submit_time 为 0 的老行/导入行必须退回 created_at 判超时，不能因为缺 submit_time 就永不超时。
func TestVideoTaskPollTimeoutFallsBackToCreatedAt(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	// 上游说它还在跑：这样「没超时」的唯一表现就是状态停在 in_progress，
	// 断言不会被「上游 404」之类的旁路结果蒙对。
	stub.set("cgt-no-submit-time", http.StatusOK, `{"id":"cgt-no-submit-time","status":"in_progress"}`)

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
