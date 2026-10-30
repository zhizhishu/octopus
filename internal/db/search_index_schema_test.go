package db_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 生产库的现场：relay_logs 里有日志，迁移 014 早就记成已执行，但 relay_log_search
// 这张表**不在**（TASK.md 2026-10-27 记录）。完成标记表是旧结构（没有 cursor 列），
// 还留着上一次写下的 complete=true —— 表后来没了，标记没跟着改。
//
// 要求走真实 InitDB 之后：
//   - 投影表与标记表都在、标记表结构已对齐（主 AutoMigrate 名单负责，不押在 014 的执行历史上）；
//   - 标记不再撒谎：投影表是空的就不能报"已补齐"（否则搜索对着空表直接返回空页）；
//   - 回填能从游标续跑到底，最后每一条老日志都在投影表里。
const oldRelayLogSearchBackfillStatesDDL = "CREATE TABLE `relay_log_search_backfill_states` (`id` integer PRIMARY KEY AUTOINCREMENT,`complete` numeric NOT NULL)"

func TestInitDBRecreatesAndBackfillsMissingSearchIndex(t *testing.T) {
	const n = 7000 // 超过一批(2000)覆盖多批续跑; 也超过已补齐库的尾部核对窗口(5000), 否则那段核对会把空表顺手补满, 测不到"标记撒谎"
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true, CreateBatchSize: 100})
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := raw.AutoMigrate(&model.RelayLog{}, &migrate.MigrationRecord{}); err != nil {
		t.Fatalf("create old tables: %v", err)
	}
	logs := make([]model.RelayLog, 0, n)
	for i := 1; i <= n; i++ {
		logs = append(logs, model.RelayLog{ID: int64(1000 + i), Time: int64(1000 + i), RequestModelName: fmt.Sprintf("legacy-model-%d", i), Severity: 1})
	}
	if err := raw.CreateInBatches(&logs, 200).Error; err != nil {
		t.Fatalf("seed relay_logs: %v", err)
	}
	if err := raw.Exec(oldRelayLogSearchBackfillStatesDDL).Error; err != nil {
		t.Fatalf("create old marker table: %v", err)
	}
	if err := raw.Exec("INSERT INTO relay_log_search_backfill_states (id, complete) VALUES (1, 1)").Error; err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}
	// 老库里 001..022 都跑过。014 记成 success 是现场的关键条件。
	for v := 1; v <= 22; v++ {
		if err := raw.Create(&migrate.MigrationRecord{Version: v, Status: migrate.MigrationRecordStatusSuccess}).Error; err != nil {
			t.Fatalf("mark migration %d applied: %v", v, err)
		}
	}
	if raw.Migrator().HasTable("relay_log_search") {
		t.Fatal("前置条件不成立: 老库不应该有 relay_log_search")
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatalf("raw db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	if err := db.InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	handle := db.GetDB()
	t.Cleanup(func() {
		if sqlDB, err := handle.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	if !handle.Migrator().HasTable("relay_log_search") {
		t.Fatal("老库升级后仍然没有 relay_log_search: 关键词搜索永远退回主表全表 LIKE")
	}
	if !handle.Migrator().HasColumn(&migrate.RelayLogSearchBackfillState{}, "Cursor") {
		t.Fatal("标记表还是旧结构(缺 cursor 列): 回填无法续跑")
	}

	countIndex := func() int64 {
		var c int64
		if err := handle.Table("relay_log_search").Count(&c).Error; err != nil {
			t.Fatalf("count relay_log_search: %v", err)
		}
		return c
	}
	complete, cursor := migrate.RelayLogSearchBackfillProgress(handle)
	if complete && countIndex() != n {
		t.Fatalf("标记说已补齐, 投影表却只有 %d/%d 行: 搜索会把老日志当成不存在", countIndex(), n)
	}
	// 游标承诺: id <= cursor 的主表行都已经在投影表里。
	assertCursorInvariant := func(cursor int64) {
		t.Helper()
		var missing int64
		if err := handle.Raw(`SELECT COUNT(*) FROM relay_logs l LEFT JOIN relay_log_search s ON s.id = l.id
			WHERE s.id IS NULL AND l.id <= ?`, cursor).Scan(&missing).Error; err != nil {
			t.Fatalf("probe cursor invariant: %v", err)
		}
		if missing != 0 {
			t.Fatalf("游标 %d 之前仍有 %d 行没进投影表: 搜索会漏掉它们", cursor, missing)
		}
	}
	assertCursorInvariant(cursor)

	// 模拟后台循环: 一批一批续跑直到补齐。
	for i := 0; !complete && i < 100; i++ {
		complete, err = migrate.BackfillRelayLogSearchIndex(handle, migrate.RelayLogSearchBackfillOptions{MaxBatches: 1})
		if err != nil {
			t.Fatalf("background backfill pass %d: %v", i, err)
		}
		_, cursor = migrate.RelayLogSearchBackfillProgress(handle)
		assertCursorInvariant(cursor)
	}
	if !complete {
		t.Fatal("回填 100 轮后仍未补齐")
	}
	if got := countIndex(); got != n {
		t.Fatalf("补齐后投影表应有 %d 行, 实际 %d", n, got)
	}
	var row model.RelayLogSearchIndex
	if err := handle.Table("relay_log_search").Where("id = ?", 1000+n).Take(&row).Error; err != nil {
		t.Fatalf("read backfilled row: %v", err)
	}
	if row.RequestModelName != fmt.Sprintf("legacy-model-%d", n) {
		t.Fatalf("回填内容不对: %+v", row)
	}
}
