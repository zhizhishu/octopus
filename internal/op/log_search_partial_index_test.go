package op

import (
	"sort"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
)

// 投影表"部分完整"时搜索必须仍然完整。
//
// 这是生产库补表期间的真实形态: relay_log_search 刚被建出来(014 记成已执行而表不在,
// 或表被删过), 回填还在后台按 id 往前走; 而这期间新写进来的日志由写入路径直接投影,
// 所以投影表里**有行、但不全**。
//
// 修复前三条路径都只认"投影表给没给出 id":
//   - 列表: 投影表命中 >0 条就只用这些, 只在主表里的老日志被静默漏掉;
//   - 计数(徽章/总数): 无条件 id IN (投影表子查询), 老日志不计入;
//   - 大 limit(导出/深翻页): 同列表。
//
// 只有"投影表 0 命中"时才退回主表 LIKE —— 而"补表期间新日志恰好也命中"正是最常见的情况。
func seedPartiallyIndexedLogs(t *testing.T) {
	t.Helper()
	ensureSearchIndexTable(t)

	logs := []model.RelayLog{
		// 升级前就在库里的老日志: 只在主表, 回填还没走到它。
		{ID: 9701, Time: 9701, RequestModelName: "needle-model", UserName: "alice"},
		// 升级之后写入的日志: 写入路径已经投影。
		{ID: 9702, Time: 9702, RequestModelName: "unrelated", UserName: "bob"},
		{ID: 9703, Time: 9703, RequestModelName: "needle-model", UserName: "carol"},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs[1:])

	// 回填没跑完: 完成标记为 false(migration_records 里 14 依然是 success)。
	// 同一测试进程里迁移只跑一次(AfterAutoMigrate 跑完就清空名单), 标记表不一定在。
	if err := db.GetDB().AutoMigrate(&migrate.RelayLogSearchBackfillState{}); err != nil {
		t.Fatalf("ensure marker table: %v", err)
	}
	if err := db.GetDB().Save(&migrate.RelayLogSearchBackfillState{ID: 1, Complete: false}).Error; err != nil {
		t.Fatalf("mark backfill incomplete: %v", err)
	}
}

func logIDsDesc(logs []model.RelayLog) []int64 {
	ids := make([]int64, 0, len(logs))
	for _, l := range logs {
		ids = append(ids, l.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

func TestRelayLogSearchCompleteWhileIndexPartiallyBackfilled(t *testing.T) {
	want := []int64{9703, 9701}

	t.Run("list", func(t *testing.T) {
		ctx := setupRelayLogTest(t)
		seedPartiallyIndexedLogs(t)
		got, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "needle-model"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if ids := logIDsDesc(got); len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
			t.Fatalf("回填进行中搜索漏掉了只在主表里的老日志: want %v got %v", want, ids)
		}
	})

	t.Run("large_limit_export", func(t *testing.T) {
		ctx := setupRelayLogTest(t)
		seedPartiallyIndexedLogs(t)
		got, err := RelayLogList(ctx, nil, nil, 1, relayLogTwoPhaseMaxIDs+100, &model.RelayLogScope{Search: "needle-model"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if ids := logIDsDesc(got); len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
			t.Fatalf("回填进行中导出漏掉了只在主表里的老日志: want %v got %v", want, ids)
		}
	})

	t.Run("counts", func(t *testing.T) {
		ctx := setupRelayLogTest(t)
		seedPartiallyIndexedLogs(t)
		n, err := RelayLogCount(ctx, nil, nil, &model.RelayLogScope{Search: "needle-model"})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Fatalf("回填进行中计数少算了只在主表里的老日志: want 2 got %d", n)
		}
		sc, err := RelayLogSeverityCounts(ctx, nil, nil, &model.RelayLogScope{Search: "needle-model"})
		if err != nil {
			t.Fatalf("severity counts: %v", err)
		}
		if sc.Total != 2 {
			t.Fatalf("回填进行中徽章总数少算: want 2 got %+v", sc)
		}
	})
}

// 带范围(用户/时间/严重程度)的搜索不能因为"投影表里更新的命中都属于别人"而返回空页。
//
// 投影表没有 user_id/time/severity, 所以列表先从投影表按关键词取最新 dbLimit 个 id,
// 再回主表按范围过滤。修复前只取一轮: 最新那 20 条命中都是别的用户的, 过滤完是 0,
// 页面就显示"没有匹配" —— 而这个用户自己更早的那条命中明明在库里。
func TestRelayLogScopedSearchNotStarvedByOtherUsersHits(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	// 1000 条别人的更新命中: 超过一轮候选(100)也超过 relayLogSearchMaxIndexRounds 轮的总量,
	// 同时覆盖"多翻几轮"和"翻够了交给主表 LIKE"两条路径。
	logs := []model.RelayLog{{ID: 9800, Time: 9800, UserID: 1, RequestModelName: "shared-model"}}
	for i := 1; i <= 1000; i++ {
		logs = append(logs, model.RelayLog{ID: int64(9800 + i), Time: int64(9800 + i), UserID: 2, RequestModelName: "shared-model"})
	}
	if err := db.GetDB().CreateInBatches(&logs, 100).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), logs)
	var indexed int64
	if err := db.GetDB().Table(relayLogSearchIndexTable).Count(&indexed).Error; err != nil || indexed != int64(len(logs)) {
		t.Fatalf("前置条件: 投影表应有 %d 行, 实际 %d (%v)", len(logs), indexed, err)
	}
	if err := db.GetDB().AutoMigrate(&migrate.RelayLogSearchBackfillState{}); err != nil {
		t.Fatalf("ensure marker table: %v", err)
	}
	// 投影表完整: 走的是"只信投影表"那条快路径。
	if err := db.GetDB().Save(&migrate.RelayLogSearchBackfillState{ID: 1, Complete: true}).Error; err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	if err := db.GetDB().Where("version = ?", 14).Delete(&migrate.MigrationRecord{}).Error; err != nil {
		t.Fatalf("reset record: %v", err)
	}
	if err := db.GetDB().Create(&migrate.MigrationRecord{Version: 14, Status: migrate.MigrationRecordStatusSuccess}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	got, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{UserID: 1, Search: "shared-model"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9800 {
		t.Fatalf("用户 1 自己的命中被别人的新命中挤掉了: want [9800] got %v", logIDsDesc(got))
	}
	// 时间范围同理: 只看最早那一秒。
	start, end := 9800, 9800
	got, err = RelayLogList(ctx, &start, &end, 1, 20, &model.RelayLogScope{Search: "shared-model"})
	if err != nil {
		t.Fatalf("list with time range: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9800 {
		t.Fatalf("时间范围内的命中被范围外的新命中挤掉了: want [9800] got %v", logIDsDesc(got))
	}
	// 另一个用户翻到第 2 页: 结果必须与主表 LIKE 逐行一致(第 21..40 新的那批)。
	got, err = RelayLogList(ctx, nil, nil, 2, 20, &model.RelayLogScope{UserID: 2, Search: "shared-model"})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	ids := logIDsDesc(got)
	if len(ids) != 20 || ids[0] != 9800+1000-20 || ids[19] != 9800+1000-39 {
		t.Fatalf("用户 2 第 2 页不对: %v", ids)
	}
}

// 宽泛搜索词的导出不能因为命中太多而失败。
//
// 修复前大 limit 分支先把投影表的**全部**命中 id Pluck 出来再拼 `id IN (?, ?, ...)`。
// 命中超过 SQLite 的绑定参数上限(32766)时整条导出报 "too many SQL variables"。
// 生产库几百万行, 搜 "gpt" 这样的词命中几十万行是常态。
func TestRelayLogLargeLimitSearchWithManyHits(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	const n = 33000
	// 用一条递归 CTE 灌数据: 逐行 Create 3.3 万条 60 列的日志要十几秒。
	seed := `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?) `
	if err := db.GetDB().Exec(seed+`INSERT INTO relay_logs (id, time, request_model_name, severity) SELECT 100000 + i, 100000 + i, 'gpt-broad', 1 FROM seq`, n).Error; err != nil {
		t.Fatalf("seed relay_logs: %v", err)
	}
	if err := db.GetDB().Exec(seed+`INSERT INTO relay_log_search (id, request_model_name) SELECT 100000 + i, 'gpt-broad' FROM seq`, n).Error; err != nil {
		t.Fatalf("seed relay_log_search: %v", err)
	}
	if err := db.GetDB().AutoMigrate(&migrate.RelayLogSearchBackfillState{}); err != nil {
		t.Fatalf("ensure marker table: %v", err)
	}
	if err := db.GetDB().Save(&migrate.RelayLogSearchBackfillState{ID: 1, Complete: true}).Error; err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	if err := db.GetDB().Where("version = ?", 14).Delete(&migrate.MigrationRecord{}).Error; err != nil {
		t.Fatalf("reset record: %v", err)
	}
	if err := db.GetDB().Create(&migrate.MigrationRecord{Version: 14, Status: migrate.MigrationRecordStatusSuccess}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	got, err := RelayLogList(ctx, nil, nil, 1, relayLogTwoPhaseMaxIDs+100, &model.RelayLogScope{Search: "gpt-broad"})
	if err != nil {
		t.Fatalf("命中 %d 行的导出失败(修复前: too many SQL variables): %v", n, err)
	}
	if len(got) != relayLogTwoPhaseMaxIDs+100 || got[0].ID != 100000+n {
		t.Fatalf("导出第一页不对: len=%d first=%v", len(got), logIDsDesc(got)[:1])
	}
}
