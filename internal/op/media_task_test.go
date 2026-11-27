package op

import (
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 合并列表（op 层）：三表 UNION、类型/状态/模型/时间过滤、分页与归属隔离。
func TestMediaTaskListByOwnerMergesAndFilters(t *testing.T) {
	ctx := setupQuotaTest(t)
	now := time.Now().Unix()

	seed := []struct {
		kind  string
		id    string
		owner int
		model string
	}{
		{"image", "img-a", 7, "gpt-image-1"},
		{"mj", "mj-a", 7, "mj_imagine"},
		{"video", "vid-a", 7, "doubao-video"},
		{"image", "img-b", 8, "gpt-image-1"}, // 别人的，不能出现
	}
	for i, row := range seed {
		created := now - int64(100-i) // img-a 最老，vid-a 最新（相对 seed 内顺序）
		switch row.kind {
		case "image":
			task := model.ImageTask{
				TaskID: row.id, CreatedAt: created, ExpiresAt: now + 3600,
				Status: model.ImageTaskStatusCompleted, OwnerUserID: row.owner, OwnerTokenID: 1,
				Model: row.model, CompletedAt: now - 5,
			}
			if err := db.GetDB().Create(&task).Error; err != nil {
				t.Fatalf("seed image: %v", err)
			}
		case "mj":
			task := model.MidjourneyTask{
				CreatedAt: created, UpstreamTaskID: row.id, Model: row.model,
				OwnerUserID: row.owner, OwnerTokenID: 1,
			}
			if err := db.GetDB().Create(&task).Error; err != nil {
				t.Fatalf("seed mj: %v", err)
			}
		case "video":
			task := model.VideoTask{
				CreatedAt: created, UpstreamTaskID: row.id, Model: row.model,
				Status: model.VideoTaskSuccess, OwnerUserID: row.owner, OwnerTokenID: 1,
			}
			if err := db.GetDB().Create(&task).Error; err != nil {
				t.Fatalf("seed video: %v", err)
			}
		}
	}

	// 全部类型：owner 7 的三行按 created_at 倒序，别人的 img-b 不出现。
	rows, total, err := MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("total=%d len=%d, want 3/3", total, len(rows))
	}
	gotIDs := []string{rows[0].TaskID, rows[1].TaskID, rows[2].TaskID}
	// 注意 seed 的 created：img-a=now-100, mj-a=now-99, vid-a=now-98 —— 倒序 vid/mj/img。
	if gotIDs[0] != "vid-a" || gotIDs[1] != "mj-a" || gotIDs[2] != "img-a" {
		t.Fatalf("order = %v, want [vid-a mj-a img-a]", gotIDs)
	}
	for _, row := range rows {
		if row.TaskID == "img-b" {
			t.Fatalf("other owner's row leaked into the list")
		}
	}
	// MJ 行状态照实为 unknown（本地不存状态），不发明新词。
	if rows[1].Status != "unknown" {
		t.Fatalf("mj status = %q, want unknown", rows[1].Status)
	}
	if rows[1].Kind != "image" {
		t.Fatalf("mj row kind = %q, want image", rows[1].Kind)
	}
	// 图片行 finish 用 completed_at。
	if rows[2].FinishTime != now-5 {
		t.Fatalf("image finish_time = %d, want %d", rows[2].FinishTime, now-5)
	}

	// 类型过滤：只要视频。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Kind: MediaTaskKindVideo, Page: 1, PageSize: 20})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].TaskID != "vid-a" {
		t.Fatalf("video filter: total=%d rows=%v err=%v", total, rows, err)
	}
	// 状态过滤：unknown 只剩 MJ 行。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Status: "unknown", Page: 1, PageSize: 20})
	if err != nil || total != 1 || rows[0].TaskID != "mj-a" {
		t.Fatalf("unknown filter: total=%d rows=%v err=%v", total, rows, err)
	}
	// 状态过滤：completed 只剩 image_tasks 行（MJ 段被排除）。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Status: model.ImageTaskStatusCompleted, Page: 1, PageSize: 20})
	if err != nil || total != 1 || rows[0].TaskID != "img-a" {
		t.Fatalf("completed filter: total=%d rows=%v err=%v", total, rows, err)
	}
	// 模型子串过滤。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Model: "mj_", Page: 1, PageSize: 20})
	if err != nil || total != 1 || rows[0].TaskID != "mj-a" {
		t.Fatalf("model filter: total=%d rows=%v err=%v", total, rows, err)
	}
	// 时间过滤：只取 vid-a 之后（含）的。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, StartTime: now - 98, Page: 1, PageSize: 20})
	if err != nil || total != 1 || rows[0].TaskID != "vid-a" {
		t.Fatalf("time filter: total=%d rows=%v err=%v", total, rows, err)
	}
	// 分页：每页 2 条时第二页只剩 1 条。
	rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Page: 2, PageSize: 2})
	if err != nil || total != 3 || len(rows) != 1 || rows[0].TaskID != "img-a" {
		t.Fatalf("pagination: total=%d rows=%v err=%v", total, rows, err)

		// fix-ui1127a-r1 第 6 条：管理员视角。AllUsers+OwnerUserID=0 看全部用户的任务；
		// AllUsers+OwnerUserID=8 收窄到指定用户；行里带归属用户 id（一清二楚）。
		rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 0, AllUsers: true, Page: 1, PageSize: 20})
		if err != nil || total != 4 || len(rows) != 4 {
			t.Fatalf("all-users: total=%d len=%d err=%v, want 4/4", total, len(rows), err)
		}
		seenOwner := map[int64]bool{}
		seenB := false
		for _, row := range rows {
			seenOwner[row.OwnerUserID] = true
			if row.TaskID == "img-b" {
				seenB = true
			}
			if row.OwnerUserID == 0 {
				t.Fatalf("row %s missing owner user id", row.TaskID)
			}
		}
		if !seenB {
			t.Fatalf("all-users view missing other owner's img-b")
		}
		if !seenOwner[7] || !seenOwner[8] {
			t.Fatalf("owner ids missing: %v", seenOwner)
		}
		// 管理员收窄到 user 8：只有 img-b。
		rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 8, AllUsers: true, Page: 1, PageSize: 20})
		if err != nil || total != 1 || len(rows) != 1 || rows[0].TaskID != "img-b" {
			t.Fatalf("admin user filter: total=%d rows=%v err=%v", total, rows, err)
		}
		// 非管理员语义不变：AllUsers=false 时 owner 0 什么都看不到（fail-closed）。
		rows, total, err = MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 0, AllUsers: false, Page: 1, PageSize: 20})
		if err != nil || total != 0 {
			t.Fatalf("non-admin owner-0 must stay empty: total=%d err=%v", total, err)
		}
	}
}
