package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new015DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	// 只建 015 回填会读的那几列：老日志表上的检测结论。
	if err := db.Exec(`CREATE TABLE relay_logs (id integer PRIMARY KEY, channel_id integer, channel_name text,
		request_model_name text, time integer, model_audit_verdict text, model_audit_score real,
		model_audit_finding_n integer, model_audit_report text)`).Error; err != nil {
		t.Fatalf("create relay_logs: %v", err)
	}
	if err := db.Exec(`INSERT INTO relay_logs VALUES (1, 7, 'ch', 'gpt-x', 100, 'none', 0, 0, '{}'),
		(2, 7, 'ch', 'gpt-y', 101, 'high', 80, 2, '{}')`).Error; err != nil {
		t.Fatalf("seed relay_logs: %v", err)
	}
	return db
}

// 结果行写不进去时，015 不能留下"导入"空壳任务（它会在结果页显示成一条打不开的记录）。
func TestMigration015LeavesNoEmptyImportJobWhenResultsFail(t *testing.T) {
	db := new015DB(t)
	if err := db.AutoMigrate(&model.ModelAuditJob{}, &model.ModelAuditResult{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if err := db.Exec(`CREATE TRIGGER block_results BEFORE INSERT ON model_audit_results
		BEGIN SELECT RAISE(ABORT, 'boom'); END`).Error; err != nil {
		t.Fatalf("trigger: %v", err)
	}

	if err := migrateCreateModelAuditJobs(db); err != nil {
		t.Fatalf("015 should degrade, not fail: %v", err)
	}
	var jobs int64
	db.Model(&model.ModelAuditJob{}).Count(&jobs)
	if jobs != 0 {
		t.Fatalf("结果行一条没写进去，却留下了 %d 个空任务", jobs)
	}
}

// 正常路径不受影响：一个导入任务 + 每条历史结论一行。
func TestMigration015ImportsHistoricVerdicts(t *testing.T) {
	db := new015DB(t)
	if err := migrateCreateModelAuditJobs(db); err != nil {
		t.Fatalf("015: %v", err)
	}
	var jobs []model.ModelAuditJob
	db.Find(&jobs)
	if len(jobs) != 1 || jobs[0].Trigger != "import" || jobs[0].Total != 2 {
		t.Fatalf("应有一个 total=2 的导入任务，得到 %+v", jobs)
	}
	var results int64
	db.Model(&model.ModelAuditResult{}).Where("job_id = ?", jobs[0].ID).Count(&results)
	if results != 2 {
		t.Fatalf("应回填 2 行结论，得到 %d", results)
	}
}
