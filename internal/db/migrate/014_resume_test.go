package migrate

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 回填必须能断点续跑, 而且已经走过的前缀不再重扫。
//
// 以前的写法每批都用 LEFT JOIN 从最小 id 开始找缺行: 已补过的前缀每批都要重新
// 扫一遍, 总工作量随行数平方增长, 并且整段在 InitDB 里同步跑完才放行启动。
func TestRelayLogSearchBackfillResumesFromCursor(t *testing.T) {
	db := newMigrateDB(t)
	const n = relayLogSearchBackfillBatch*2 + 10
	rows := make([]model.RelayLog, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, model.RelayLog{ID: int64(i), Time: int64(i), RequestModelName: "m"})
	}
	if err := db.CreateInBatches(&rows, 200).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.AutoMigrate(&model.RelayLogSearchIndex{}, &RelayLogSearchBackfillState{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	complete, err := BackfillRelayLogSearchIndex(db, RelayLogSearchBackfillOptions{MaxBatches: 1})
	if err != nil || complete {
		t.Fatalf("一批之后不该补齐: complete=%v err=%v", complete, err)
	}
	done, cursor := RelayLogSearchBackfillProgress(db)
	if done || cursor != relayLogSearchBackfillBatch {
		t.Fatalf("一批之后游标应停在 %d, 实际 complete=%v cursor=%d", relayLogSearchBackfillBatch, done, cursor)
	}

	// "进程被杀、重启": 把游标之前的一行从投影表删掉, 证明续跑不会回头重扫前缀
	// (它从游标往后走); 那一行由已补齐库的尾部核对或运维重建负责, 不是续跑的职责。
	if err := db.Exec("DELETE FROM relay_log_search WHERE id = 1").Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	complete, err = BackfillRelayLogSearchIndex(db, RelayLogSearchBackfillOptions{})
	if err != nil || !complete {
		t.Fatalf("续跑应补齐: complete=%v err=%v", complete, err)
	}
	var c int64
	if err := db.Table("relay_log_search").Count(&c).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if c != n-1 {
		t.Fatalf("续跑应只补游标之后的行(期望 %d 行), 实际 %d", n-1, c)
	}
	if done, _ := RelayLogSearchBackfillProgress(db); !done {
		t.Fatal("补齐后标记应为 true")
	}
}

// 已补齐的库: 投影表被清空(或被删后由 AutoMigrate 重建成空表)时, 启动必须发现并从头回填,
// 不能继续相信那个 complete=true —— 那等于让搜索对着空表宣布"没有匹配"。
func TestMigration014RestartsBackfillWhenIndexEmptiedAfterComplete(t *testing.T) {
	// 尾部核对窗口缩到 1 行: 否则 2 行的小库会被"核对最新 5000 行"顺手补齐,
	// 测不到"标记撒谎"这条分支(大库上尾部窗口之外的老日志就永远补不回来)。
	old := relayLogSearchTailReconcileRows
	relayLogSearchTailReconcileRows = 1
	t.Cleanup(func() { relayLogSearchTailReconcileRows = old })

	db := newMigrateDB(t)
	if err := db.Create(&[]model.RelayLog{{ID: 1, Time: 1, RequestModelName: "a"}, {ID: 2, Time: 2, RequestModelName: "b"}}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !RelayLogSearchBackfillComplete(db) {
		t.Fatal("前置条件: 第一遍应补齐")
	}
	if err := db.Migrator().DropTable(&model.RelayLogSearchIndex{}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	var c int64
	if err := db.Table("relay_log_search").Count(&c).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if c != 2 {
		t.Fatalf("投影表被删后应从头回填, 实际 %d 行(标记 complete=%v)", c, RelayLogSearchBackfillComplete(db))
	}
}
