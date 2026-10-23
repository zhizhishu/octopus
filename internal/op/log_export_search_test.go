package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 导出 / 深翻页（dbLimit 超过两段式上限）必须**照样带着搜索条件**。
//
// 回归背景：大 limit 那条分支曾经直接 `query.Order("id DESC").Limit(N).Find()`，
// 而 relayLogApplyScopeBase 只落时间和用户范围、**不落搜索词**。结果是
// "在搜索结果页点导出" 导出的是全库日志 —— 用户筛出 2 条，拿到的是全部，
// 里面还有别人的记录和根本没筛的时间段。这既是数据泄露，也让导出失去意义。
//
// 这条测试故意把 pageSize 推到两段式上限之上，走的就是那条分支。
func TestRelayLogLargeLimitKeepsSearchFilter(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	logs := []model.RelayLog{
		{ID: 9501, Time: 9501, RequestModelName: "kimi-k3", ChannelName: "primary", UserID: 1},
		{ID: 9502, Time: 9502, RequestModelName: "gpt-6-astra", ChannelName: "backup", UserID: 1},
		{ID: 9503, Time: 9503, RequestModelName: "kimi-k3", ChannelName: "primary", UserID: 1},
		{ID: 9504, Time: 9504, RequestModelName: "glm-5", ChannelName: "backup", UserID: 1},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	// 超过 relayLogTwoPhaseMaxIDs → 走大 limit 单查询分支（导出路径）。
	pageSize := relayLogTwoPhaseMaxIDs + 100

	got, err := RelayLogList(ctx, nil, nil, 1, pageSize, &model.RelayLogScope{Search: "kimi-k3"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(got) != 2 {
		ids := make([]int, 0, len(got))
		for _, g := range got {
			ids = append(ids, int(g.ID))
		}
		t.Fatalf("大 limit 带搜索词应只返回 2 条匹配(kimi-k3)，实际 %d 条 ids=%v —— "+
			"说明导出丢了搜索条件，把全库日志都导出来了", len(got), ids)
	}
	for _, g := range got {
		if g.RequestModelName != "kimi-k3" {
			t.Fatalf("返回了不匹配搜索词的记录: id=%d model=%s", g.ID, g.RequestModelName)
		}
		if g.ID != 9501 && g.ID != 9503 {
			t.Fatalf("命中了不该命中的 id=%d", g.ID)
		}
	}
}

// 投影表不可用时，大 limit 分支也必须退回主表 LIKE，而不是直接返回全部。
func TestRelayLogLargeLimitSearchFallsBackToMainTable(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{ID: 9601, Time: 9601, RequestModelName: "kimi-k3", ChannelName: "primary", UserID: 1},
		{ID: 9602, Time: 9602, RequestModelName: "gpt-6-astra", ChannelName: "backup", UserID: 1},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	// 故意不建投影表：模拟迁移未完成/被删。
	if db.GetDB().Migrator().HasTable(relayLogSearchIndexTable) {
		if err := db.GetDB().Migrator().DropTable(relayLogSearchIndexTable); err != nil {
			t.Fatalf("drop index table: %v", err)
		}
	}

	got, err := RelayLogList(ctx, nil, nil, 1, relayLogTwoPhaseMaxIDs+100, &model.RelayLogScope{Search: "kimi-k3"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9601 {
		t.Fatalf("投影表缺失时大 limit 搜索应退回主表 LIKE 并只命中 9601，实际 %+v", got)
	}
	ensureSearchIndexTable(t)
}
