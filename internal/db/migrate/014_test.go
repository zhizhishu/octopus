package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 迁移 014 是"建投影表 + 回填 + 补索引"，线上是**已有一堆日志**的库在跑它，
// 不是空库。这些用例守的就是升级路径：
//   - 存量行必须被回填(否则升级后老日志搜不到)
//   - 重复跑不能重复插入(迁移失败重试、服务重启很常见)
//   - 空库不能报错(全新安装)

func newMigrateDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.RelayLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestMigration014BackfillsExistingRows(t *testing.T) {
	db := newMigrateDB(t)

	// 模拟"升级前就存在的日志"
	rows := []model.RelayLog{
		{ID: 1, Time: 1, RequestModelName: "kimi-k3", Error: "timeout"},
		{ID: 2, Time: 2, RequestModelName: "gpt-6-astra"},
		{ID: 3, Time: 3, RequestModelName: "glm-5.3"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var n int64
	if err := db.Table("relay_log_search").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("存量 3 行应全部回填, 实际 %d 行(老日志会搜不到)", n)
	}

	// 关键字段要真的带过去, 不能只搬 id
	var got model.RelayLogSearchIndex
	if err := db.Table("relay_log_search").Where("id = ?", 1).Take(&got).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.RequestModelName != "kimi-k3" || got.Error != "timeout" {
		t.Fatalf("回填内容不对: %+v", got)
	}
}

func TestMigration014IsIdempotent(t *testing.T) {
	db := newMigrateDB(t)

	rows := []model.RelayLog{
		{ID: 1, Time: 1, RequestModelName: "kimi-k3"},
		{ID: 2, Time: 2, RequestModelName: "other"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 跑三次: 迁移失败重试、服务重启都会重复执行
	for i := 0; i < 3; i++ {
		if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var n int64
	if err := db.Table("relay_log_search").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("重复执行应保持 2 行, 实际 %d 行(重复插入)", n)
	}
}

func TestMigration014OnEmptyDatabase(t *testing.T) {
	db := newMigrateDB(t)
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("空库不应报错(全新安装路径): %v", err)
	}
	// 表仍应被建出来, 否则后续写入没有落点
	if !db.Migrator().HasTable("relay_log_search") {
		t.Fatal("空库也要建出投影表")
	}
}

// 增量: 迁移跑完之后新写入的日志, 由迁移再次执行时补上。
// (线上写入路径也会补, 这里守的是"漏了也有兜底")
func TestMigration014PicksUpRowsAddedLater(t *testing.T) {
	db := newMigrateDB(t)

	if err := db.Create(&model.RelayLog{ID: 1, Time: 1, RequestModelName: "first"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 假设这行写入时投影失败(进程被杀/DB 忙)
	if err := db.Create(&model.RelayLog{ID: 2, Time: 2, RequestModelName: "second"}).Error; err != nil {
		t.Fatalf("seed2: %v", err)
	}

	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	var n int64
	if err := db.Table("relay_log_search").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("漏掉的行应被补上, 实际 %d 行", n)
	}
}

// 索引迁移: 已存在的库也要有 total_attempts 索引。
func TestMigration014AddsTotalAttemptsIndex(t *testing.T) {
	db := newMigrateDB(t)
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='relay_logs'").Scan(&names).Error; err != nil {
		t.Fatalf("list index: %v", err)
	}
	for _, n := range names {
		if n == "idx_relay_logs_total_attempts" {
			return
		}
	}
	t.Fatalf("缺少 idx_relay_logs_total_attempts(重试筛选会退回全表扫): %v", names)
}

// 014 的注释承诺「回填中途失败留待下次启动继续」，但迁移框架只有标了 AlwaysRun
// 的迁移才会在成功记录之后重跑。014 没有标，所以只要 Up() 返回 nil（哪怕是
// 半途而废的 return nil），这一次就被记成 success，下次启动直接跳过——
// 补不上的行永远补不上，而日志表看着一切正常。
//
// 这条用例走真正的 runner（runMigrationsWithRecord），不是直接调函数，
// 因为要验证的恰恰是"记录写成功之后还会不会重跑"。
func TestMigration014RetriesAfterAnIncompletePass(t *testing.T) {
	db := newMigrateDB(t)

	if err := db.Create(&model.RelayLog{ID: 1, Time: 1, RequestModelName: "first"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// AlwaysRun 必须带上，否则测的就不是真实注册的那个迁移。
	migrations := []Migration{{Version: 14, Up: migrateBackfillRelayLogSearchIndex, AlwaysRun: true}}
	if err := runMigrationsWithRecord(db, migrations); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// 这一行是"上次启动之后才写进来的"：写入路径若也失败，就只有回填能救它。
	if err := db.Create(&model.RelayLog{ID: 2, Time: 2, RequestModelName: "second"}).Error; err != nil {
		t.Fatalf("seed2: %v", err)
	}

	// 第二次"启动"：同一个迁移再跑一遍。
	if err := runMigrationsWithRecord(db, []Migration{{Version: 14, Up: migrateBackfillRelayLogSearchIndex, AlwaysRun: true}}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	var n int64
	if err := db.Table("relay_log_search").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("下次启动应继续补齐投影行, 实际只有 %d 行(期望 2)", n)
	}
}

// 014 现在每次启动都会重跑（AlwaysRun），所以"已经补齐"这条路径必须是便宜的
// no-op：一条 LEFT JOIN 探测拿到空结果就返回，不重灌、不重复建索引。
//
// 这条用例挡的是把 AlwaysRun 当免费午餐的后果——如果哪天有人在循环里加了全量
// 重灌或每轮都 AutoMigrate，启动成本就从"一次探测"变成"扫全表"，而单看
// "回填结果对不对"的用例全都还是绿的。
func TestMigration014FullyReconciledPassIsCheapNoOp(t *testing.T) {
	db := newMigrateDB(t)

	for i := 1; i <= 50; i++ {
		if err := db.Create(&model.RelayLog{ID: int64(i), Time: int64(i), RequestModelName: "m"}).Error; err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// 记录下补齐后的状态，再跑一遍，除了"没有缺失行"之外什么都不该变。
	var beforeRows, beforeMaxID int64
	if err := db.Table("relay_log_search").Count(&beforeRows).Error; err != nil {
		t.Fatalf("count before: %v", err)
	}
	if err := db.Table("relay_log_search").Select("COALESCE(MAX(id),0)").Scan(&beforeMaxID).Error; err != nil {
		t.Fatalf("max id before: %v", err)
	}

	// 数一下第二遍真正碰了多少行：让它自己报告，而不是我们猜。
	var probed int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM relay_logs l
		LEFT JOIN relay_log_search s ON s.id = l.id
		WHERE s.id IS NULL`).Scan(&probed).Error; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probed != 0 {
		t.Fatalf("前置条件不成立: 补齐后仍有 %d 行缺失", probed)
	}

	if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	var afterRows, afterMaxID int64
	if err := db.Table("relay_log_search").Count(&afterRows).Error; err != nil {
		t.Fatalf("count after: %v", err)
	}
	if err := db.Table("relay_log_search").Select("COALESCE(MAX(id),0)").Scan(&afterMaxID).Error; err != nil {
		t.Fatalf("max id after: %v", err)
	}
	if afterRows != beforeRows || afterMaxID != beforeMaxID {
		t.Fatalf("空转那一遍不该改动投影表: rows %d->%d, maxID %d->%d",
			beforeRows, afterRows, beforeMaxID, afterMaxID)
	}
}

// 回填失败**不许**把服务挡在门外。
//
// 框架契约(migrate.go:17-19)：AlwaysRun 的 Up MUST return nil — not an error —
// when it intentionally can't finish yet, so a transient skip never aborts startup。
// 而实际链条是：Up 返回 err → runMigrationsWithRecord 记 failed 并 return err
// (migrate.go:117-121) → AfterAutoMigrate return err → db.InitDB return err
// (db.go:101) → cmd/start.go 只 log.Errorf 然后 return —— 进程退出，服务起不来。
//
// 一个可降级的回填故障(库忙、连接抖动、列被改)不该让整个服务下线。
func TestMigration014FillFailureDoesNotBlockStartup(t *testing.T) {
	db := newMigrateDB(t)

	seed := &model.RelayLog{RequestModelName: "gpt-x", UserName: "alice"}
	if err := db.Create(seed).Error; err != nil {
		t.Fatalf("seed relay_log: %v", err)
	}

	var m Migration
	for _, candidate := range afterAutoMigrations {
		if candidate.Version == 14 {
			m = candidate
			break
		}
	}
	if m.Up == nil {
		t.Skip("migration 014 is not registered in this build")
	}

	// 把回填**读批那条 Select 用到的一列**删掉。
	//
	// 这条 Select 显式点名了 session_key 等列，缺任何一列都会让读批那步报错——
	// 这正是"回填可降级失败"的真实形态（列被改、方言差异、库忙等价物）。
	// 刻意不动探测那一步：探测只用到 l.id / s.id，保持它成功，才能真正走到读批分支。
	//
	// 为什么不用"先把索引表造坏"：Up 开头就 AutoMigrate 建表，坏表会被顺手修好，
	// 于是根本走不到失败分支——那样的测试永远绿，是空气测试（栽过一次）。
	if err := db.Migrator().DropColumn(&model.RelayLog{}, "session_key"); err != nil {
		t.Fatalf("drop session_key to force a backfill read failure: %v", err)
	}

	if err := m.Up(db); err != nil {
		t.Fatalf("migration 014 must return nil when the backfill cannot finish "+
			"(a returned error aborts startup), got: %v", err)
	}
	if !m.AlwaysRun {
		t.Fatal("migration 014 must stay AlwaysRun so the backfill resumes on the next startup")
	}
}

// 014 在回填半途而废时蓄意返回 nil（不让服务起不来），框架就记 success。
// 所以"真的补齐了吗"必须由迁移自己写标记，不能由 migration_records 推断。
// 这条用例守两个方向：跑完 = true（否则搜索永远走慢路径），
// 半途失败 = false（否则搜索对着缺行的表宣布空页，老日志永久搜不到）。
func TestMigration014WritesBackfillMarkerOnlyWhenFinished(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		db := newMigrateDB(t)
		if err := db.Create(&[]model.RelayLog{
			{ID: 1, Time: 1, RequestModelName: "kimi-k3"},
			{ID: 2, Time: 2, RequestModelName: "glm-5.3"},
		}).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if !RelayLogSearchBackfillComplete(db) {
			t.Fatal("补齐之后必须置标记, 否则搜索永远退回主表 LIKE(慢)")
		}
		// 空库(全部补齐后再跑一遍的形态)也要保持 true, 不能把它改回 false。
		if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
			t.Fatalf("second pass: %v", err)
		}
		if !RelayLogSearchBackfillComplete(db) {
			t.Fatal("空转那一遍不该把标记改回 false")
		}
	})

	t.Run("incomplete", func(t *testing.T) {
		db := newMigrateDB(t)
		if err := db.Create(&model.RelayLog{RequestModelName: "gpt-x"}).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		// 让回填读批那步失败(缺列), 走"半途而废"分支 —— 该分支返回 nil。
		if err := db.Migrator().DropColumn(&model.RelayLog{}, "session_key"); err != nil {
			t.Fatalf("drop column: %v", err)
		}
		if err := migrateBackfillRelayLogSearchIndex(db); err != nil {
			t.Fatalf("半途而废必须返回 nil(不能挡住启动): %v", err)
		}
		if RelayLogSearchBackfillComplete(db) {
			t.Fatal("回填没跑完却置了完成标记: 搜索会对着缺行的投影表直接返回空页")
		}
	})
}
