package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 24,
		Up:      migrateDeleteEmptyModelAuditJobs,
	})
}

// 024: 清掉"有任务、没结果行"的空壳检测任务。
//
// 来历（2026-10-27 生产上留下 9 条）：老库的 model_audit_results 缺 attempt /
// next_retry_at 两列，当时入队是"先建任务、再逐条插结果行"两步写——任务行落库了，
// 结果行一条都插不进去，留下永远"排队中"、进度 0/N、点开什么都没有的任务。
// d430720 已经把建任务和写结果行收进同一个事务（失败整体回滚），并让每次启动都对齐
// 表结构，所以新的空壳不会再产生；这里只负责把历史遗留的清掉。
//
// 安全性：
//   - 只删**一条结果行都没有**的任务（NOT EXISTS），有任何一行结果的任务一律不动——
//     检测结果是花钱买来的；
//   - 空壳任务本身没有任何可读的内容（结论、报告、错误都在结果行上），删掉不丢信息；
//   - 幂等：再跑一次时已经没有空壳，什么都不删；
//   - 启动期执行，此时检测 worker 还没起来，不存在"任务刚建、结果行还没写"的窗口
//     （何况现在两者在同一个事务里提交）。
func migrateDeleteEmptyModelAuditJobs(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	m := db.Migrator()
	if !m.HasTable(&model.ModelAuditJob{}) || !m.HasTable(&model.ModelAuditResult{}) {
		return nil
	}
	res := db.Where("NOT EXISTS (?)",
		db.Model(&model.ModelAuditResult{}).Select("1").
			Where("model_audit_results.job_id = model_audit_jobs.id"),
	).Delete(&model.ModelAuditJob{})
	if res.Error != nil {
		return fmt.Errorf("delete empty model audit jobs: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infof("migration 024: removed %d empty model audit job(s) that had no result rows", res.RowsAffected)
	}
	return nil
}
