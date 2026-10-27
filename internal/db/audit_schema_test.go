package db_test

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 旧结构：与一次性迁移 015 当年建出来的表一致，**没有** attempt / next_retry_at。
// 生产库就是这个形状，也是"检测永远排队中"的根源。
const oldModelAuditResultsDDL = "CREATE TABLE `model_audit_results` (" +
	"`id` integer PRIMARY KEY AUTOINCREMENT,`job_id` integer,`log_id` integer DEFAULT 0," +
	"`channel_id` integer DEFAULT 0,`channel_name` text DEFAULT \"\",`model` text DEFAULT \"\"," +
	"`upstream_model` text DEFAULT \"\",`status` text DEFAULT \"pending\",`verdict` text DEFAULT \"\"," +
	"`score` real DEFAULT 0,`finding_n` integer DEFAULT 0,`report` text,`error_msg` text," +
	"`duration_ms` integer DEFAULT 0,`created_at` datetime,`finished_at` datetime)"

// TestInitDBAlignsExistingAuditTables 守"启动即对齐结构"。
//
// 生产上真栽过这一次（2026-10-27）：model_audit_results 只由 migrate/015 建表，
// 015 是一次性迁移，跑完记成已执行，之后再也不会跑第二遍。于是给 ModelAuditResult
// 加了 attempt / next_retry_at 之后——新库和 CI 从零建表字段齐全全绿，而老库那张表
// 还是旧结构，每次入队 INSERT 都报 "no column named attempt"：任务建出来了、
// 结果行一条写不进去，队列永远空转，用户看到的就是"点了检测没反应"。
//
// 这条测试先按旧结构建表，再走真实的初始化入口，要求列被补齐、并且能真的插进去一行。
func TestInitDBAlignsExistingAuditTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := raw.Exec(oldModelAuditResultsDDL).Error; err != nil {
		t.Fatalf("create old-schema table: %v", err)
	}
	// 关键：老库里 015 早就跑过、记成成功，所以它**不会再跑第二遍**。
	// 不把这个记录补上，测试就等于在新库上跑（015 会重跑并顺手把列补齐），
	// 那样即使主 AutoMigrate 名单漏了这两张表，测试也是绿的——假绿。
	if err := raw.AutoMigrate(&migrate.MigrationRecord{}); err != nil {
		t.Fatalf("create migration record table: %v", err)
	}
	if err := raw.Create(&migrate.MigrationRecord{
		Version: 15,
		Status:  migrate.MigrationRecordStatusSuccess,
	}).Error; err != nil {
		t.Fatalf("mark migration 015 as applied: %v", err)
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatalf("raw db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// 真实的启动入口。它现在必须把这两张表也纳入每次启动的对齐名单。
	if err := db.InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	handle := db.GetDB()
	if handle == nil {
		t.Fatal("InitDB 之后 GetDB 为空")
	}
	// 临时目录要在测试结束时能删掉，得先把连接池关掉（Windows 上文件被占用就删不动）。
	t.Cleanup(func() {
		if sqlDB, err := handle.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	for _, col := range []string{"attempt", "next_retry_at"} {
		if !handle.Migrator().HasColumn(&model.ModelAuditResult{}, col) {
			t.Fatalf("老库升级后仍然缺列 %q —— 入队会在写入结果行时失败，队列永远空转", col)
		}
	}

	// 光有列还不够：按新结构真的写一行进去，证明入队那一步能跑通。
	job := model.ModelAuditJob{ChannelID: 1, ChannelName: "ch", Status: model.AuditJobQueued, Total: 1}
	if err := handle.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	res := model.ModelAuditResult{
		JobID:     job.ID,
		ChannelID: 1,
		Model:     "m",
		Status:    model.AuditResultPending,
		Attempt:   1,
	}
	if err := handle.Create(&res).Error; err != nil {
		t.Fatalf("按新结构写结果行失败（就是生产上的症状）: %v", err)
	}
}
