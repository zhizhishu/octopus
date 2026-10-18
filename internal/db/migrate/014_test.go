package migrate

import (
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
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
