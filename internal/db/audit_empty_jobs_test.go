package db_test

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 生产上的 9 条空壳检测任务（有任务行、没结果行）要在升级后第一次启动时被清掉，
// 而有结果的任务原样保留。走真实的 InitDB，证明 024 确实挂在启动流程上。
func TestInitDBRemovesEmptyAuditJobsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := raw.AutoMigrate(&model.ModelAuditJob{}, &model.ModelAuditResult{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	empty := model.ModelAuditJob{ChannelID: 1, ChannelName: "ch", Status: model.AuditJobQueued, Total: 2}
	kept := model.ModelAuditJob{ChannelID: 1, ChannelName: "ch", Status: model.AuditJobDone, Total: 1}
	if err := raw.Create(&empty).Error; err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	if err := raw.Create(&kept).Error; err != nil {
		t.Fatalf("seed kept: %v", err)
	}
	if err := raw.Create(&model.ModelAuditResult{JobID: kept.ID, ChannelID: 1, Model: "m", Status: model.AuditResultDone, Verdict: "none"}).Error; err != nil {
		t.Fatalf("seed result: %v", err)
	}
	if sqlDB, err := raw.DB(); err == nil {
		_ = sqlDB.Close()
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

	var ids []int64
	if err := handle.Model(&model.ModelAuditJob{}).Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != kept.ID {
		t.Fatalf("启动后应只剩有结果的任务 %d，实际 %v", kept.ID, ids)
	}
}
