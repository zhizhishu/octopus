package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new024DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.ModelAuditJob{}, &model.ModelAuditResult{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// 生产上的空壳形态：任务行在、total=N、结果行一条都没有。只删这种；
// 有结果的任务（不论成功、失败、跳过、还在排队）一行都不能动。
func TestMigration024DeletesOnlyEmptyAuditJobs(t *testing.T) {
	db := new024DB(t)

	mk := func(status string, total int) model.ModelAuditJob {
		j := model.ModelAuditJob{ChannelID: 1, ChannelName: "ch", Status: status, Total: total}
		if err := db.Create(&j).Error; err != nil {
			t.Fatalf("create job: %v", err)
		}
		return j
	}
	empty1 := mk(model.AuditJobQueued, 3) // 生产上那 9 条：永远排队中
	empty2 := mk(model.AuditJobCanceled, 1)
	withPending := mk(model.AuditJobQueued, 1)
	withDone := mk(model.AuditJobDone, 2)
	withFailed := mk(model.AuditJobDone, 1)

	rows := []model.ModelAuditResult{
		{JobID: withPending.ID, ChannelID: 1, Model: "a", Status: model.AuditResultPending},
		{JobID: withDone.ID, ChannelID: 1, Model: "b", Status: model.AuditResultDone, Verdict: "none"},
		{JobID: withDone.ID, ChannelID: 1, Model: "c", Status: model.AuditResultDone, Verdict: "high"},
		{JobID: withFailed.ID, ChannelID: 1, Model: "d", Status: model.AuditResultFailed},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("create results: %v", err)
	}

	if err := runMigrationsWithRecord(db, []Migration{{Version: 24, Up: migrateDeleteEmptyModelAuditJobs}}); err != nil {
		t.Fatalf("run 024: %v", err)
	}

	var left []int64
	if err := db.Model(&model.ModelAuditJob{}).Order("id").Pluck("id", &left).Error; err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	want := []int64{withPending.ID, withDone.ID, withFailed.ID}
	if len(left) != len(want) {
		t.Fatalf("剩余任务 %v，期望 %v（空壳 %d/%d 应被删）", left, want, empty1.ID, empty2.ID)
	}
	for i := range want {
		if left[i] != want[i] {
			t.Fatalf("剩余任务 %v，期望 %v", left, want)
		}
	}
	var resultCount int64
	db.Model(&model.ModelAuditResult{}).Count(&resultCount)
	if resultCount != int64(len(rows)) {
		t.Fatalf("结果行不该被动：剩 %d，期望 %d", resultCount, len(rows))
	}

	// 幂等：再跑一次本体不报错、不再删任何东西。
	if err := migrateDeleteEmptyModelAuditJobs(db); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	var again int64
	db.Model(&model.ModelAuditJob{}).Count(&again)
	if again != int64(len(want)) {
		t.Fatalf("重跑后任务数变了: %d", again)
	}
}

// 表还不存在（极老的库、或检测表从未建过）时直接跳过，不能挡住启动。
func TestMigration024SkipsWhenTablesMissing(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bare.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := migrateDeleteEmptyModelAuditJobs(db); err != nil {
		t.Fatalf("missing tables should be a no-op, got %v", err)
	}
}
