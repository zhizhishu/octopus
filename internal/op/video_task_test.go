package op

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// newVideoTaskTestDB 建一个独立的临时库。t.Cleanup 里必须关连接：
// Windows 上不关的话临时目录删不掉，整条测试会被判 FAIL——那是收尾问题，
// 不是功能问题，但会让红绿失真，所以必须收干净。
func newVideoTaskTestDB(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	openVideoTaskTestDB(t, dsn)
	return dsn
}

func openVideoTaskTestDB(t *testing.T, dsn string) {
	t.Helper()
	if err := db.InitDB("sqlite", dsn, false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
}

// CAS 的契约：只有当前状态还是 from 时才写成功，否则一行都不许动。
// 这是并发安全的地基——已经终态的任务不能被慢一拍的轮询写回可执行态。
func TestVideoTaskStatusCASRespectsFromStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-cas", Status: model.VideoTaskNotStart}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskNotStart, model.VideoTaskSubmitted, map[string]any{
		"submit_time": int64(1700000000),
	})
	if err != nil || !ok {
		t.Fatalf("matching CAS: ok=%v err=%v", ok, err)
	}

	// 用已经过期的 from 再推一次，必须失败且不改行。
	ok, err = VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskNotStart, model.VideoTaskInProgress, map[string]any{
		"progress": "50%",
	})
	if err != nil {
		t.Fatalf("mismatched CAS err: %v", err)
	}
	if ok {
		t.Fatalf("mismatched CAS updated a row, want false")
	}

	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-cas")
	if err != nil || stored == nil {
		t.Fatalf("read back: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskSubmitted {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskSubmitted)
	}
	if stored.Progress != "" {
		t.Fatalf("mismatched CAS wrote progress %q, want untouched", stored.Progress)
	}

	// 终态不可回退：success 之后用 from=in_progress 再推必须失败。
	if ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskSubmitted, model.VideoTaskSuccess, nil); err != nil || !ok {
		t.Fatalf("advance to success: ok=%v err=%v", ok, err)
	}
	if ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskInProgress, model.VideoTaskSuccess, nil); err != nil || ok {
		t.Fatalf("terminal must not regress: ok=%v err=%v", ok, err)
	}
}

// CAS 在并发下只能有一个赢家：两个 goroutine 拿同一个 from 同时推进，
// 必须一个成功一个失败，否则状态机就有两条写入路径互相覆盖。
//
// 【必须有起跑线】两个裸 goroutine 不保证重叠：调度器完全可能让第一个先跑完，
// 那样这条用例只验证了「顺序执行时第二次 CAS 失败」，而 CAS 存在的意义恰恰是「同时」。
// 「读一次再写」这种非原子实现在串行时也会绿，只有真重叠才会暴露两个赢家。
// 所以两个 goroutine 先各自报到（ready）、再由 start 一起放行：双方都就绪后才有
// 任何一方去调 CAS，两次调用的起点被压到同一瞬间。
func TestVideoTaskStatusCASConcurrentOnlyOneWinner(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-race", Status: model.VideoTaskQueued}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	start := make(chan struct{})
	var (
		ready   sync.WaitGroup
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	ready.Add(2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskQueued, model.VideoTaskInProgress, nil)
			if err != nil {
				t.Errorf("concurrent CAS err: %v", err)
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

// 「重启后任务还在」是这次改造的全部价值：库里查得到，客户端就不用重建任务、
// 不用重付一次钱。所以归属必须能从库里读回来，不能只活在内存。
func TestVideoTaskSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "restart.db")

	// 第一次进程：建库、落一条任务，然后连库一起关掉。
	openVideoTaskTestDB(t, dsn)
	task := &model.VideoTask{
		UpstreamTaskID: "cgt-restart",
		ChannelID:      7,
		ChannelKeyID:   9,
		Model:          "some-video-model",
		OwnerTokenID:   3,
		OwnerUserID:    4,
		Status:         model.VideoTaskInProgress,
	}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create before restart: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	// 第二次进程：重新打开同一个库文件，进程内任何内存状态都不再可信。
	openVideoTaskTestDB(t, dsn)

	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-restart")
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if stored == nil {
		t.Fatalf("task lost across restart: nothing found for cgt-restart")
	}
	if stored.ChannelID != 7 || stored.ChannelKeyID != 9 {
		t.Fatalf("ownership lost: channel=%d key=%d, want 7/9", stored.ChannelID, stored.ChannelKeyID)
	}
	if stored.OwnerTokenID != 3 || stored.OwnerUserID != 4 {
		t.Fatalf("tenant lost: token=%d user=%d, want 3/4", stored.OwnerTokenID, stored.OwnerUserID)
	}
}

// 未完成任务的筛选是轮询进程的入口：终态任务不许再被捞出来打上游。
func TestVideoTaskListUnfinishedExcludesTerminal(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	seed := []model.VideoTask{
		{UpstreamTaskID: "a", Status: model.VideoTaskQueued},
		{UpstreamTaskID: "b", Status: model.VideoTaskInProgress},
		{UpstreamTaskID: "c", Status: model.VideoTaskSuccess},
		{UpstreamTaskID: "d", Status: model.VideoTaskFailure},
	}
	for i := range seed {
		if err := VideoTaskCreate(ctx, &seed[i]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	tasks, err := VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d unfinished, want 2", len(tasks))
	}
	for _, task := range tasks {
		if model.IsVideoTaskTerminal(task.Status) {
			t.Fatalf("terminal task %q returned by unfinished list", task.UpstreamTaskID)
		}
	}
}

// CAS 成功时 updates 里的附加列必须和 status 一起落地。
//
// 没有这条断言的话，「CAS 只把 status 写进去、updates 被吞掉」这种错会一路静默：
// 状态看着对，轮询器写回的 progress / result_url / fail_reason 全部消失。其中
// submit_time 最隐蔽——没有任何状态断言覆盖它，只能显式读回来验。
func TestVideoTaskStatusCASWritesUpdateFields(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-cas-updates", Status: model.VideoTaskNotStart}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.SubmitTime != 0 {
		t.Fatalf("precondition failed: submit_time starts at %d, want 0", task.SubmitTime)
	}

	ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskNotStart, model.VideoTaskSubmitted, map[string]any{
		"submit_time": int64(1700000123),
		"progress":    "7%",
	})
	if err != nil {
		t.Fatalf("CAS err: %v", err)
	}
	if !ok {
		t.Fatalf("matching CAS reported no rows affected")
	}

	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-cas-updates")
	if err != nil || stored == nil {
		t.Fatalf("read back: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskSubmitted {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskSubmitted)
	}
	// submit_time 是这条用例的主角：只靠状态断言完全看不出它有没有写进去。
	if stored.SubmitTime != 1700000123 {
		t.Fatalf("submit_time = %d, want 1700000123 (the update payload was dropped)", stored.SubmitTime)
	}
	if stored.Progress != "7%" {
		t.Fatalf("progress = %q, want %q (the update payload was dropped)", stored.Progress, "7%")
	}
}

// 「终态不许回退」的真实保证形态：一个慢一拍的写者拿着**过时的 from** 回来写，
// 必须一行都写不进去。
//
// 这是 CAS 存在的理由（见 VideoTaskStatusCAS 的注释：「一个已经 success 的任务被
// 慢一拍的轮询写回 in_progress，用户就白等一轮」）。场景：轮询 A 读到 in_progress
// 正准备写回进度，超时扫描 B 抢先把它收口成 success；A 的迟到写入必须失败，
// 且行上 status / progress / finish_time 一个字节都不许变。
func TestVideoTaskStatusCASTerminalCannotRegress(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-terminal-late-write", Status: model.VideoTaskInProgress}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 慢写者 A 此刻读到的快照（下一步就是它要拿来回写的 from）。
	staleFrom := task.Status

	// B（超时扫描）先到，把任务收口成 success。
	if ok, err := VideoTaskStatusCAS(ctx, task.ID, model.VideoTaskInProgress, model.VideoTaskSuccess, map[string]any{
		"finish_time": int64(1700000999),
		"result_url":  "https://example.com/done.mp4",
	}); err != nil || !ok {
		t.Fatalf("advance to success: ok=%v err=%v", ok, err)
	}

	// A 的迟到写入：from 是它当时读到的值（此刻已过期），to 是「回退」方向。
	ok, err := VideoTaskStatusCAS(ctx, task.ID, staleFrom, model.VideoTaskInProgress, map[string]any{
		"progress": "99%",
	})
	if err != nil {
		t.Fatalf("late CAS err: %v", err)
	}
	if ok {
		t.Fatalf("late write with a stale from won: a terminal task was pushed back to in_progress")
	}

	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-terminal-late-write")
	if err != nil || stored == nil {
		t.Fatalf("read back: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskSuccess {
		t.Fatalf("status = %q, want %q (terminal regressed)", stored.Status, model.VideoTaskSuccess)
	}
	if stored.Progress != "" {
		t.Fatalf("progress = %q, want untouched", stored.Progress)
	}
	if stored.FinishTime != 1700000999 {
		t.Fatalf("finish_time = %d, want 1700000999 (the rejected write landed)", stored.FinishTime)
	}
	if stored.ResultURL != "https://example.com/done.mp4" {
		t.Fatalf("result_url = %q, want untouched", stored.ResultURL)
	}
}

// 终态回退在**生产里到不了**，所以 CAS 不加方向校验——这条测试钉的就是「到不了」。
//
// 背景：VideoTaskStatusCAS 的 WHERE 只有 `id = ? AND status = from`，没有方向校验，
// 因此 from=success → to=in_progress 单看 CAS 是会成功的。曾有一条 t.Skip 掉的测试
// 把这写成「已知生产缺陷」，等 supervisor 决定要不要补方向校验。决定是**不补**，
// 理由是补了等于给不会发生的事写代码：
//
//   - 三个生产调用点（video_poll.go:385/398/494）传的 from 一律取自
//     VideoTaskListUnfinished 或 VideoTaskListStale 取回的行；
//   - 这两条查询都带 videoTaskNotTerminalWhere，终态行根本进不来；
//   - 所以生产里不存在 from=终态 的调用，越权回退没有路径。
//
// 下面两个断言守的就是这个前提本身。哪天有人把那两条查询的终态过滤去掉，
// 这里会先红——比事后补方向校验更早、也更准（方向校验只能挡住症状）。
func TestVideoTaskTerminalRowsNeverEnterThePoller(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	finished := &model.VideoTask{UpstreamTaskID: "cgt-terminal-done", Status: model.VideoTaskInProgress}
	if err := VideoTaskCreate(ctx, finished); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := VideoTaskStatusCAS(ctx, finished.ID, model.VideoTaskInProgress, model.VideoTaskSuccess, map[string]any{
		"finish_time": int64(1700000999),
		"result_url":  "https://example.com/done.mp4",
	}); err != nil || !ok {
		t.Fatalf("advance to success: ok=%v err=%v", ok, err)
	}

	failed := &model.VideoTask{UpstreamTaskID: "cgt-terminal-failed", Status: model.VideoTaskInProgress}
	if err := VideoTaskCreate(ctx, failed); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := VideoTaskStatusCAS(ctx, failed.ID, model.VideoTaskInProgress, model.VideoTaskFailure, map[string]any{
		"finish_time": int64(1700000998),
		"fail_reason": "upstream rejected",
	}); err != nil || !ok {
		t.Fatalf("advance to failure: ok=%v err=%v", ok, err)
	}

	running := &model.VideoTask{UpstreamTaskID: "cgt-terminal-running", Status: model.VideoTaskInProgress}
	if err := VideoTaskCreate(ctx, running); err != nil {
		t.Fatalf("create: %v", err)
	}

	unfinished, err := VideoTaskListUnfinished(ctx, 100)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	for _, task := range unfinished {
		if model.IsVideoTaskTerminal(task.Status) {
			t.Fatalf("terminal task %q (status %q) leaked into the poller entry list; "+
				"the CAS callers take `from` from here, so a terminal `from` would be reachable",
				task.UpstreamTaskID, task.Status)
		}
	}
	if len(unfinished) != 1 || unfinished[0].UpstreamTaskID != "cgt-terminal-running" {
		t.Fatalf("unfinished = %+v, want only cgt-terminal-running", unfinished)
	}

	// 超时扫描那条入口同样只取「未进终态」的行。
	stale, err := VideoTaskListStale(ctx, time.Now().Unix()+60, 100)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	for _, task := range stale {
		if model.IsVideoTaskTerminal(task.Status) {
			t.Fatalf("terminal task %q leaked into the timeout sweep", task.UpstreamTaskID)
		}
	}
}

// 未完成列表的 limit / 排序边界：这是轮询进程的入口，limit 是保险丝、updated_at 升序
// 是「这一轮从哪条开始」的唯一依据。
//
// 三件事必须同时成立，否则积压时会饿死任务或把同一批反复处理：
//  1. limit 真的截断（不是忽略它把整表捞出来），且只数未完成的行——终态行不占名额；
//  2. 顺序是「最久没被问过的先问」（updated_at 升序），**不是** id 升序：按 id 升序会让
//     上游永报非终态的老行永久占据队头，把 id 更大的新任务挤出批次（队头阻塞，见
//     task.TestVideoTaskPollStuckTasksDoNotBlockNewOnes）；
//  3. limit <= 0 落到默认值（保险丝不能因为调用方传 0 就变成「一条都不处理」）。
func TestVideoTaskListUnfinishedLimitAndOrder(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	// 终态行先落库，id 更小：它们若占名额/被捞出来，下面的断言立刻就能看出来。
	// u-1..u-5 的 updated_at 刻意与 id 升序相反（u-5 最久没被问过、u-1 最近），
	// 这样「按 id 排」和「按 updated_at 排」的结果互不相同，排序改回 id 立刻变红。
	base := time.Now().Add(-time.Hour).Unix()
	seed := []model.VideoTask{
		{UpstreamTaskID: "t-1", Status: model.VideoTaskSuccess},
		{UpstreamTaskID: "t-2", Status: model.VideoTaskFailure},
		{UpstreamTaskID: "u-1", Status: model.VideoTaskNotStart, UpdatedAt: base + 5},
		{UpstreamTaskID: "u-2", Status: model.VideoTaskQueued, UpdatedAt: base + 4},
		{UpstreamTaskID: "u-3", Status: model.VideoTaskSubmitted, UpdatedAt: base + 3},
		{UpstreamTaskID: "u-4", Status: model.VideoTaskInProgress, UpdatedAt: base + 2},
		{UpstreamTaskID: "u-5", Status: model.VideoTaskUnknown, UpdatedAt: base + 1},
	}
	for i := range seed {
		if err := VideoTaskCreate(ctx, &seed[i]); err != nil {
			t.Fatalf("seed %s: %v", seed[i].UpstreamTaskID, err)
		}
	}

	assertOrder := func(label string, tasks []model.VideoTask, want []string) {
		t.Helper()
		got := listedTaskIDs(tasks)
		if len(got) != len(want) {
			t.Fatalf("%s: got %v (%d rows), want %v", label, got, len(got), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v (limit/order decides which tasks a round starves)", label, got, want)
			}
		}
	}

	limited, err := VideoTaskListUnfinished(ctx, 2)
	if err != nil {
		t.Fatalf("list with limit 2: %v", err)
	}
	// 最久没被问过的两条：limit 作用在过滤之后，终态行不占名额。
	assertOrder("limit=2", limited, []string{"u-5", "u-4"})

	all, err := VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list with limit 10: %v", err)
	}
	assertOrder("limit=10", all, []string{"u-5", "u-4", "u-3", "u-2", "u-1"})

	// 保险丝的默认值：0 和负数都该退成「默认批量」，而不是「一条都不处理」。
	for _, limit := range []int{0, -1} {
		tasks, err := VideoTaskListUnfinished(ctx, limit)
		if err != nil {
			t.Fatalf("list with limit %d: %v", limit, err)
		}
		assertOrder("limit="+strconv.Itoa(limit), tasks, []string{"u-5", "u-4", "u-3", "u-2", "u-1"})
	}
}
