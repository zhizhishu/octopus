package op

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// seedVideoTaskListRows 落一组归属各不相同的任务，供「列表只返回自己的」用。
//
// 其中 legacy-no-owner 那一行是 owner_token_id/owner_user_id 都为 0 的历史行
// （表默认 0，外部导入/迁移进来的老行也可能是这样）。它对立面很关键：列表接口
// **不能**把「没记归属」当成「谁都能看」。
func seedVideoTaskListRows(t *testing.T, ctx context.Context) {
	t.Helper()
	rows := []model.VideoTask{
		{UpstreamTaskID: "own-token-11", OwnerTokenID: 11, OwnerUserID: 21, Status: model.VideoTaskSuccess, SubmitTime: 1_700_000_100},
		{UpstreamTaskID: "own-token-12", OwnerTokenID: 12, OwnerUserID: 22, Status: model.VideoTaskInProgress, SubmitTime: 1_700_000_200},
		{UpstreamTaskID: "legacy-no-owner", Status: model.VideoTaskFailure, SubmitTime: 1_700_000_300},
	}
	for i := range rows {
		if err := VideoTaskCreate(ctx, &rows[i]); err != nil {
			t.Fatalf("seed %s: %v", rows[i].UpstreamTaskID, err)
		}
	}
}

func listedTaskIDs(tasks []model.VideoTask) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.UpstreamTaskID)
	}
	return ids
}

// 【租户隔离】按归属列任务时只能看到自己那条：A 的查询里不许出现 B 的行，
// 也不许出现「没有归属」的历史行。
func TestVideoTaskListByOwnerIsolatesTenants(t *testing.T) {
	openVideoTaskTestDB(t, filepath.Join(t.TempDir(), "octopus.db"))
	ctx := context.Background()
	seedVideoTaskListRows(t, ctx)

	// 令牌 11 的调用者：只能看到 own-token-11。
	tasks, total, err := VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerTokenID: 11, OwnerUserID: 21, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list as token 11: %v", err)
	}
	if total != 1 || len(tasks) != 1 {
		t.Fatalf("token 11 saw total=%d rows=%v, want exactly its own row", total, listedTaskIDs(tasks))
	}
	if tasks[0].UpstreamTaskID != "own-token-11" {
		t.Fatalf("token 11 saw %q, want own-token-11", tasks[0].UpstreamTaskID)
	}

	// 令牌 12 的调用者：只能看到 own-token-12。
	tasks, total, err = VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerTokenID: 12, OwnerUserID: 22, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list as token 12: %v", err)
	}
	if total != 1 || len(tasks) != 1 || tasks[0].UpstreamTaskID != "own-token-12" {
		t.Fatalf("token 12 saw total=%d rows=%v, want exactly own-token-12", total, listedTaskIDs(tasks))
	}

	// 只按用户过滤（登录态 JWT 的日志页走这条路）：用户 21 只拿到 owner_user_id=21 的行。
	tasks, total, err = VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerUserID: 21, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list as user 21: %v", err)
	}
	if total != 1 || len(tasks) != 1 || tasks[0].UpstreamTaskID != "own-token-11" {
		t.Fatalf("user 21 saw total=%d rows=%v, want exactly own-token-11", total, listedTaskIDs(tasks))
	}

	// 反向对照：库里确实有 3 行，上面几条查询看到的少只能是归属过滤的结果，
	// 不是「表里本来就没数据」。
	var all int64
	if err := db.GetDB().Model(&model.VideoTask{}).Count(&all).Error; err != nil {
		t.Fatalf("count all rows: %v", err)
	}
	if all != 3 {
		t.Fatalf("table holds %d rows, want 3 (the isolation assertions above need a non-empty table)", all)
	}
}

// 归属是全 0 时必须报错，不许退化成「全表」。
// 这条断言就是「忘了传归属」与「故意查全部」之间的那道闸。
func TestVideoTaskListByOwnerRefusesEmptyOwnerScope(t *testing.T) {
	openVideoTaskTestDB(t, filepath.Join(t.TempDir(), "octopus.db"))
	ctx := context.Background()
	seedVideoTaskListRows(t, ctx)

	for _, filter := range []VideoTaskListFilter{
		{},
		{Page: 1, PageSize: 20},
		{OwnerTokenID: 0, OwnerUserID: 0},
	} {
		tasks, total, err := VideoTaskListByOwner(ctx, filter)
		if !errors.Is(err, ErrVideoTaskOwnerScopeRequired) {
			t.Fatalf("filter %+v: err=%v, want ErrVideoTaskOwnerScopeRequired", filter, err)
		}
		if tasks != nil || total != 0 {
			t.Fatalf("filter %+v: got %v rows / total=%d, want nothing", filter, listedTaskIDs(tasks), total)
		}
	}
}

// 状态过滤与分页：total 必须描述「满足过滤条件的全部行数」，而不是当前页条数，
// 否则前端算出来的页数是错的。
func TestVideoTaskListByOwnerStatusAndPagination(t *testing.T) {
	openVideoTaskTestDB(t, filepath.Join(t.TempDir(), "octopus.db"))
	ctx := context.Background()

	rows := []model.VideoTask{
		{UpstreamTaskID: "p-1", OwnerUserID: 31, Status: model.VideoTaskSuccess},
		{UpstreamTaskID: "p-2", OwnerUserID: 31, Status: model.VideoTaskSuccess},
		{UpstreamTaskID: "p-3", OwnerUserID: 31, Status: model.VideoTaskSuccess},
		{UpstreamTaskID: "p-4", OwnerUserID: 31, Status: model.VideoTaskFailure},
		{UpstreamTaskID: "p-5", OwnerUserID: 31, Status: model.VideoTaskInProgress},
		// 别人的行：任何一页都不许出现。
		{UpstreamTaskID: "q-1", OwnerUserID: 32, Status: model.VideoTaskSuccess},
	}
	for i := range rows {
		if err := VideoTaskCreate(ctx, &rows[i]); err != nil {
			t.Fatalf("seed %s: %v", rows[i].UpstreamTaskID, err)
		}
	}

	page1, total, err := VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerUserID: 31, Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5 (all rows of user 31, not one page)", total)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 rows = %v, want 2", listedTaskIDs(page1))
	}
	// id DESC：最新的两条在前。
	if page1[0].UpstreamTaskID != "p-5" || page1[1].UpstreamTaskID != "p-4" {
		t.Fatalf("page 1 = %v, want newest first [p-5 p-4]", listedTaskIDs(page1))
	}

	page3, total, err := VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerUserID: 31, Page: 3, PageSize: 2})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if total != 5 || len(page3) != 1 || page3[0].UpstreamTaskID != "p-1" {
		t.Fatalf("page 3 = %v total=%d, want [p-1] total 5", listedTaskIDs(page3), total)
	}

	success, total, err := VideoTaskListByOwner(ctx, VideoTaskListFilter{OwnerUserID: 31, Status: model.VideoTaskSuccess, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	if total != 3 || len(success) != 3 {
		t.Fatalf("success rows = %v total=%d, want 3 success rows", listedTaskIDs(success), total)
	}
	for _, task := range success {
		if task.Status != model.VideoTaskSuccess {
			t.Fatalf("status filter leaked %q", task.Status)
		}
		if task.OwnerUserID != 31 {
			t.Fatalf("status filter leaked another tenant's row %q", task.UpstreamTaskID)
		}
	}
}
