package op

import (
	"context"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// rawInsertVideoTaskWithStatus 绕开 ORM 直接写一行，status 用 SQL 字面量 NULL。
//
// 为什么必须绕过 ORM：GORM 的 Create 见到 default tag 的零值字段会整列省略、让数据库
// 默认值 'not_start' 生效，所以走 Create 永远造不出 NULL 行——用 Create 写的「NULL 行」
// 测试实际上什么都没验证。这条缺陷的来源恰恰是「不经 ORM 的写入」（手工 SQL、外部导入、
// 迁移脚本），测试必须从同一个入口复现它。
func rawInsertVideoTaskWithStatus(t *testing.T, upstreamTaskID, statusSQL string, createdAt int64) int64 {
	t.Helper()
	if err := db.GetDB().Exec(
		"INSERT INTO video_tasks (upstream_task_id, channel_id, channel_key_id, model, status, created_at, updated_at) VALUES (?, 7, 11, 'seedance-1-0-lite-t2v', "+statusSQL+", ?, ?)",
		upstreamTaskID, createdAt, createdAt,
	).Error; err != nil {
		t.Fatalf("raw insert %s: %v", upstreamTaskID, err)
	}
	var id int64
	if err := db.GetDB().Raw("SELECT id FROM video_tasks WHERE upstream_task_id = ?", upstreamTaskID).Scan(&id).Error; err != nil {
		t.Fatalf("read back id of %s: %v", upstreamTaskID, err)
	}
	return id
}

// rawStatusIsNull 直接问数据库「status 这一格是不是 NULL」。
// 断言必须落在 SQL 层：GORM 把 NULL 扫成零值 ""，用 Go 侧的值反推不出 NULL 与空串的区别。
func rawStatusIsNull(t *testing.T, id int64) bool {
	t.Helper()
	var isNull int
	if err := db.GetDB().Raw("SELECT status IS NULL FROM video_tasks WHERE id = ?", id).Scan(&isNull).Error; err != nil {
		t.Fatalf("status IS NULL probe for %d: %v", id, err)
	}
	return isNull == 1
}

// 持久化边界的真缺陷：status 为 NULL 的行必须能被轮询器捞到，否则这条任务永远卡住、
// 客户端永远等不到结果。SQL 里 `NULL NOT IN (...)` 求值为 UNKNOWN，WHERE 不选它。
func TestVideoTaskListUnfinishedIncludesNullStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	nullID := rawInsertVideoTaskWithStatus(t, "cgt-null", "NULL", 1700000000)
	if !rawStatusIsNull(t, nullID) {
		t.Fatalf("precondition failed: status of cgt-null is not NULL")
	}

	// 同一个库里放一条终态、一条正常未完成，确保修复没有把「只捞 NULL」当成目标。
	if err := VideoTaskCreate(ctx, &model.VideoTask{UpstreamTaskID: "cgt-success", Status: model.VideoTaskSuccess}); err != nil {
		t.Fatalf("seed success: %v", err)
	}
	if err := VideoTaskCreate(ctx, &model.VideoTask{UpstreamTaskID: "cgt-queued", Status: model.VideoTaskQueued}); err != nil {
		t.Fatalf("seed queued: %v", err)
	}

	tasks, err := VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}

	var sawNull, sawQueued, sawSuccess bool
	for _, task := range tasks {
		switch task.UpstreamTaskID {
		case "cgt-null":
			sawNull = true
			if task.Status != "" {
				t.Fatalf("NULL status scanned as %q, want the zero value", task.Status)
			}
		case "cgt-queued":
			sawQueued = true
		case "cgt-success":
			sawSuccess = true
		}
	}
	if !sawNull {
		t.Fatalf("VideoTaskListUnfinished dropped the NULL-status task (got %d tasks: %+v)", len(tasks), tasks)
	}
	if !sawQueued {
		t.Fatalf("VideoTaskListUnfinished dropped a normal unfinished task")
	}
	if sawSuccess {
		t.Fatalf("VideoTaskListUnfinished returned a terminal task")
	}
}

// 捞得到还不够：这一行必须真的能被推进。CAS 的 WHERE 是 `status = ?`，
// 而 `status = NULL` 同样永不成立——只修查询不修 CAS 等于换个地方卡住。
func TestVideoTaskStatusCASAdvancesNullStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	id := rawInsertVideoTaskWithStatus(t, "cgt-null-cas", "NULL", 1700000000)

	tasks, err := VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("precondition failed: NULL row not listed, got %+v", tasks)
	}
	from := tasks[0].Status // 读出来是零值 ""，正是轮询器回传给 CAS 的那个值

	ok, err := VideoTaskStatusCAS(ctx, id, from, model.VideoTaskInProgress, map[string]any{"progress": "30%"})
	if err != nil {
		t.Fatalf("CAS on NULL row: %v", err)
	}
	if !ok {
		t.Fatalf("CAS on NULL row reported no rows affected; the NULL row can never be advanced")
	}
	if rawStatusIsNull(t, id) {
		t.Fatalf("CAS reported success but the column is still NULL")
	}
	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-null-cas")
	if err != nil || stored == nil {
		t.Fatalf("read back after CAS: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskInProgress {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskInProgress)
	}
	if stored.Progress != "30%" {
		t.Fatalf("progress = %q, want %q (the update payload was dropped)", stored.Progress, "30%")
	}

	// 修 NULL 不等于把 CAS 变成无条件 UPDATE：用过期的 from 再推必须失败。
	if ok, err := VideoTaskStatusCAS(ctx, id, model.VideoTaskQueued, model.VideoTaskSuccess, nil); err != nil || ok {
		t.Fatalf("stale from must not win: ok=%v err=%v", ok, err)
	}
	// 反过来，其它读者拿非空 from 也不许推一行其实是 NULL 的任务。
	nullID := rawInsertVideoTaskWithStatus(t, "cgt-null-guard", "NULL", 1700000000)
	if ok, err := VideoTaskStatusCAS(ctx, nullID, model.VideoTaskQueued, model.VideoTaskSuccess, nil); err != nil || ok {
		t.Fatalf("non-empty from must not match a NULL row: ok=%v err=%v", ok, err)
	}
	if !rawStatusIsNull(t, nullID) {
		t.Fatalf("a rejected CAS modified the NULL row")
	}
}

// 空串和 NULL 在 Go 侧都读成零值 ""，CAS 的 from 为空时必须两者都覆盖。
//
// 这条钉的是「修 NULL 不许踩掉空串」：如果 from 为空的分支只写 `status IS NULL`，
// 一行真正的空串从此再也推不动（原来 `status = ?` 还能匹配它）——修一个卡死点、
// 换出另一个，是这类补丁最容易犯的错。
func TestVideoTaskStatusCASAdvancesEmptyStringStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	id := rawInsertVideoTaskWithStatus(t, "cgt-empty", "''", 1700000000)
	if rawStatusIsNull(t, id) {
		t.Fatalf("precondition failed: expected an empty string, not NULL")
	}

	ok, err := VideoTaskStatusCAS(ctx, id, "", model.VideoTaskSubmitted, nil)
	if err != nil {
		t.Fatalf("CAS on empty-string status: %v", err)
	}
	if !ok {
		t.Fatalf("CAS on empty-string status reported no rows affected")
	}
	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-empty")
	if err != nil || stored == nil {
		t.Fatalf("read back: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskSubmitted {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskSubmitted)
	}
}

// 超时收口走的是另一条查询，用的是同一个 status 条件。只修未完成列表的话，
// NULL 行会被捞去轮询、却永远等不到超时兜底——卡死换个姿势继续卡。
func TestVideoTaskListStaleIncludesNullStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	old := int64(1700000000)
	nullID := rawInsertVideoTaskWithStatus(t, "cgt-null-stale", "NULL", old)
	if !rawStatusIsNull(t, nullID) {
		t.Fatalf("precondition failed: status is not NULL")
	}
	// 终态的老任务必须继续被排除，否则修复会把已收口的行重新判一次超时。
	if err := db.GetDB().Exec(
		"INSERT INTO video_tasks (upstream_task_id, channel_id, status, created_at, updated_at) VALUES ('cgt-success-stale', 7, 'success', ?, ?)",
		old, old,
	).Error; err != nil {
		t.Fatalf("seed terminal stale row: %v", err)
	}

	tasks, err := VideoTaskListStale(ctx, old+3600, 10)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	var sawNull, sawSuccess bool
	for _, task := range tasks {
		switch task.UpstreamTaskID {
		case "cgt-null-stale":
			sawNull = true
		case "cgt-success-stale":
			sawSuccess = true
		}
	}
	if !sawNull {
		t.Fatalf("VideoTaskListStale dropped the NULL-status task (got %+v)", tasks)
	}
	if sawSuccess {
		t.Fatalf("VideoTaskListStale returned a terminal task")
	}
}

// 写入侧不该主动制造需要兜的局面：Status 为空的 Create 必须落成 not_start。
//
// 【这条测的不是修复的判别器】GORM 见到 default:'not_start' 的零值字段会省略该列、
// 让数据库默认值生效，并把生效后的值回填到结构体上，所以拆掉 VideoTaskCreate 里的
// 兜底这行测试照样绿。留着它钉的是不变量本身：Create 之后 status 不许是 NULL/空串，
// 且这个保证不依赖「哪天有人把 tag 上的 default 改掉」。
func TestVideoTaskCreateFillsEmptyStatus(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	task := &model.VideoTask{UpstreamTaskID: "cgt-create-empty"}
	if err := VideoTaskCreate(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Status != model.VideoTaskNotStart {
		t.Fatalf("in-memory status = %q, want %q", task.Status, model.VideoTaskNotStart)
	}
	if rawStatusIsNull(t, task.ID) {
		t.Fatalf("Create wrote NULL into status")
	}
	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-create-empty")
	if err != nil || stored == nil {
		t.Fatalf("read back: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskNotStart {
		t.Fatalf("stored status = %q, want %q", stored.Status, model.VideoTaskNotStart)
	}
}

// NULL 行在并发下也只能有一个赢家。
//
// 为什么单独一条：from 为空走的是**另一条 WHERE**（`status IS NULL` 或 `status` 等于空串），
// 它和 from 非空那条（`status = ?`）不是一个表达式。而这条分支恰恰是最容易被写成
// 无条件 UPDATE 的地方——「NULL 行推不动」的最省事修法就是把 status 条件整个拿掉，
// 那样单写者用例全绿，只有两个写者同时推进才会暴露：两边都返回 ok，后写的那份
// progress 覆盖先写的，任务进度凭空跳变。
//
// 起跑线与 TestVideoTaskStatusCASConcurrentOnlyOneWinner 同理：必须真的重叠。
func TestVideoTaskStatusCASConcurrentNullRowSingleWinner(t *testing.T) {
	newVideoTaskTestDB(t)
	ctx := context.Background()

	id := rawInsertVideoTaskWithStatus(t, "cgt-null-race", "NULL", 1700000000)
	if !rawStatusIsNull(t, id) {
		t.Fatalf("precondition failed: status of cgt-null-race is not NULL")
	}

	payloads := []string{"10%", "90%"}
	start := make(chan struct{})
	var (
		ready   sync.WaitGroup
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	ready.Add(len(payloads))
	for _, payload := range payloads {
		wg.Add(1)
		go func(progress string) {
			defer wg.Done()
			ready.Done()
			<-start
			// from 取零值 ""：这正是轮询器从 NULL 行读出来的值。
			ok, err := VideoTaskStatusCAS(ctx, id, "", model.VideoTaskInProgress, map[string]any{"progress": progress})
			if err != nil {
				t.Errorf("concurrent CAS on NULL row (%s) err: %v", progress, err)
				return
			}
			if ok {
				mu.Lock()
				winners = append(winners, progress)
				mu.Unlock()
			}
		}(payload)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v (%d), want exactly 1: the empty-from branch must still be a CAS, not an unconditional UPDATE", winners, len(winners))
	}

	stored, err := VideoTaskGetByUpstreamID(ctx, "cgt-null-race")
	if err != nil || stored == nil {
		t.Fatalf("read back after concurrent CAS: %v %v", stored, err)
	}
	if stored.Status != model.VideoTaskInProgress {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskInProgress)
	}
	// 落库的必须是**赢家那一份** payload，不是两份的混合、也不是空。
	if stored.Progress != winners[0] {
		t.Fatalf("progress = %q, want the winner's payload %q (a loser's write landed after the CAS was decided)", stored.Progress, winners[0])
	}
	if rawStatusIsNull(t, id) {
		t.Fatalf("the column is still NULL after a winning CAS")
	}
}
