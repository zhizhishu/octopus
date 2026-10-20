package op

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 这些测试守的是"搜索加速没有改变语义"。
//
// 加速做法是把 12 个字段的 LIKE 从主表挪到只含搜索列的投影表(relay_log_search),
// 用 id IN (子查询) 与其余条件取交集。最容易出的错不是慢, 而是:
//   - 投影表没维护 → 新日志列表能看到、搜索搜不到
//   - 投影表没删 → 日志已删、搜索还命中, 点开是空的
//   - 降级路径不一致 → 建表前后搜索结果不同

// ensureSearchIndexTable 保证投影表存在。
//
// 为什么每个用例都要调: "降级"用例会故意 DropTable 来验证退路, 而所有用例共用
// 同一个包级 db 句柄, 用例顺序一变后面就会撞 "no such table"。显式建表让每个
// 用例自带前置条件, 不依赖执行顺序。
func ensureSearchIndexTable(t *testing.T) {
	t.Helper()
	if err := db.GetDB().AutoMigrate(&model.RelayLogSearchIndex{}); err != nil {
		t.Fatalf("create search index: %v", err)
	}
}

func TestRelayLogSearchIndexIsMaintainedOnWrite(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	if !db.GetDB().Migrator().HasTable(relayLogSearchIndexTable) {
		t.Fatal("迁移 014 未建出投影表: 搜索会静默退回主表 LIKE, 优化等于没生效")
	}

	logs := []model.RelayLog{
		{ID: 9001, Time: 9001, RequestModelName: "kimi-k3", ChannelName: "primary"},
		{ID: 9002, Time: 9002, RequestModelName: "gpt-6-astra", ChannelName: "backup"},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	var n int64
	if err := db.GetDB().Table(relayLogSearchIndexTable).Count(&n).Error; err != nil {
		t.Fatalf("count index: %v", err)
	}
	if n != 2 {
		t.Fatalf("投影表应有 2 行, 实际 %d", n)
	}

	got, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "kimi-k3"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9001 {
		t.Fatalf("经投影表搜索应只命中 9001, 实际 %+v", got)
	}
}

// 数字搜索要同时命中日志 id 与渠道号 —— 投影表必须带 channel_id 列,
// 否则这一步得回主表, 又把大字段拖回来。
func TestRelayLogSearchIndexMatchesChannelID(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	logs := []model.RelayLog{{ID: 9101, Time: 9101, ChannelId: 5, RequestModelName: "x"}}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	got, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "5"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, g := range got {
		if g.ID == 9101 {
			return
		}
	}
	t.Fatalf("数字搜索应命中 channel_id=5 的行, 实际 %+v", got)
}

// 删除必须同步删投影: 否则搜得到、点开没有。
func TestRelayLogSearchIndexRemovedOnClear(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	logs := []model.RelayLog{
		{ID: 9201, Time: 9201, RequestModelName: "glm-5.3"},
		{ID: 9202, Time: 9202, RequestModelName: "glm-5.3"},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var n int64
	if err := db.GetDB().Table(relayLogSearchIndexTable).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("清空后投影表应无残留, 实际 %d 行(会搜到打不开的日志)", n)
	}
}

// 按存储上限清理同样要带走投影行。
func TestRelayLogSearchIndexRemovedOnStorageCleanup(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	logs := []model.RelayLog{
		{ID: 9301, Time: 9301, RequestModelName: "grok-4.7", RequestContent: strings.Repeat("a", 700), ResponseContent: strings.Repeat("b", 700)},
		{ID: 9302, Time: 9302, RequestModelName: "grok-4.7", RequestContent: strings.Repeat("c", 700), ResponseContent: strings.Repeat("d", 700)},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	// 只留最新一条
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000002"); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var remaining []model.RelayLog
	if err := db.GetDB().Order("id ASC").Find(&remaining).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	// 前提校验: 这一步必须真的删掉了东西, 否则"两边一致"是平凡成立的, 测不到同步。
	if len(remaining) != 1 || remaining[0].ID != 9302 {
		t.Fatalf("前置条件不成立: 应只剩最新一条 9302, 实际 %+v", remaining)
	}

	var idxIDs []int64
	if err := db.GetDB().Table(relayLogSearchIndexTable).Pluck("id", &idxIDs).Error; err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(idxIDs) != 1 || idxIDs[0] != 9302 {
		t.Fatalf("投影表应只剩 9302(实际 %v): 否则删掉的日志仍能被搜到", idxIDs)
	}
}

// 投影表不在时必须优雅降级到主表 LIKE, 而不是报错或返回空。
func TestRelayLogSearchFallsBackWhenIndexMissing(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{{ID: 9401, Time: 9401, RequestModelName: "step-5-preview"}}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.GetDB().Migrator().DropTable(relayLogSearchIndexTable); err != nil {
		t.Fatalf("drop: %v", err)
	}

	got, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "step-5"})
	if err != nil {
		t.Fatalf("投影表缺失时应降级而不是报错: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9401 {
		t.Fatalf("降级到主表搜索应仍能命中, 实际 %+v", got)
	}
}

// 降级路径与投影路径必须给出同一批结果 —— 否则"优化上线前后搜到的条数会变"。
func TestRelayLogSearchFallbackAndIndexAgree(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{ID: 9501, Time: 9501, RequestModelName: "mimo-v2.6-pro", UserName: "alice"},
		{ID: 9502, Time: 9502, RequestModelName: "other", UserName: "bob", Error: "mimo-v2.6-pro timed out"},
		{ID: 9503, Time: 9503, RequestModelName: "unrelated", UserName: "carol"},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)

	viaIndex, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "mimo-v2.6-pro"})
	if err != nil {
		t.Fatalf("index path: %v", err)
	}

	if err := db.GetDB().Migrator().DropTable(relayLogSearchIndexTable); err != nil {
		t.Fatalf("drop: %v", err)
	}
	viaFallback, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "mimo-v2.6-pro"})
	if err != nil {
		t.Fatalf("fallback path: %v", err)
	}

	if len(viaIndex) != len(viaFallback) {
		t.Fatalf("两条路径结果数不同: 投影 %d, 降级 %d", len(viaIndex), len(viaFallback))
	}
	for i := range viaIndex {
		if viaIndex[i].ID != viaFallback[i].ID {
			t.Fatalf("第 %d 行不同: 投影 %d, 降级 %d", i, viaIndex[i].ID, viaFallback[i].ID)
		}
	}
	if len(viaIndex) != 2 {
		t.Fatalf("应命中 9501 与 9502, 实际 %d 条", len(viaIndex))
	}
}
