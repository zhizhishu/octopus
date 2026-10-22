package op

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

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
func TestVideoTaskStatusCASConcurrentOnlyOneWinner(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-race", Status: model.VideoTaskQueued}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
