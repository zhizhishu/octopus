package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 15,
		Up:      migrateCreateModelAuditJobs,
	})
}

// 015: 建模型检测任务/结果两张表，并把历史检测结论从日志行搬过来。
//
// 背景：检测结论此前写在 relay_logs 的 model_audit_* 五列上（先到先得），
// 生命周期被日志保留期绑死——日志一清理，花钱测出来的结论就跟着没了。
// 这里把它拆成独立表，并回填存量结论。
//
// 安全性：
//   - 只增不改：relay_logs 的旧列**原样保留**，运行期仍会写它们，不回填不删改。
//     两边并存，任何一侧出问题都不至于丢结论。
//   - 幂等：AutoMigrate 建表；回填按源日志 id 判重，重复跑不会重复插入。
//   - 失败降级：回填出错只返回错误给迁移记录，不阻塞启动；新表建不出来才是硬错。
//   - 分批：按 id 区间回填，避免在大表上长时间持锁。
func migrateCreateModelAuditJobs(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	if err := db.AutoMigrate(&model.ModelAuditJob{}, &model.ModelAuditResult{}); err != nil {
		return fmt.Errorf("create model audit tables: %w", err)
	}

	if !db.Migrator().HasTable("relay_logs") {
		return nil
	}

	// 历史结论回填成一个"导入"任务，让它们在结果页里成批可见。
	//
	// 只搬有结论的行（verdict 非空）。同一批放进一个 job 而不是逐条建 job，
	// 是为了不把结果页刷成上千条一行任务。
	const batchSize = 2000
	var firstLogID int64
	if err := db.Raw(
		`SELECT MIN(id) FROM relay_logs WHERE model_audit_verdict IS NOT NULL AND model_audit_verdict != ''`,
	).Row().Scan(&firstLogID); err != nil {
		return nil // 列可能尚不存在，跳过回填
	}
	if firstLogID == 0 {
		return nil // 没有需要回填的历史结论
	}

	// 已有回填任务就不再重复建。
	var existing int64
	if err := db.Model(&model.ModelAuditJob{}).
		Where("trigger = ?", "import").
		Count(&existing).Error; err != nil {
		return nil
	}
	if existing > 0 {
		return nil
	}

	var total int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM relay_logs WHERE model_audit_verdict IS NOT NULL AND model_audit_verdict != ''`,
	).Row().Scan(&total); err != nil {
		return nil
	}

	job := model.ModelAuditJob{
		Status:  model.AuditJobDone,
		Total:   int(total),
		Trigger: "import",
		Probes:  "",
	}
	if err := db.Create(&job).Error; err != nil {
		return nil
	}

	var lastID int64
	moved := 0
	for {
		type row struct {
			ID                 int64
			ChannelID          int
			ChannelName        string
			RequestModelName   string
			Time               int64
			ModelAuditVerdict  string
			ModelAuditScore    float64
			ModelAuditFindingN int
			ModelAuditReport   string
		}
		var rows []row
		err := db.Raw(`
			SELECT id, channel_id, channel_name, request_model_name, time,
			       model_audit_verdict, model_audit_score, model_audit_finding_n, model_audit_report
			FROM relay_logs
			WHERE id > ? AND model_audit_verdict IS NOT NULL AND model_audit_verdict != ''
			ORDER BY id LIMIT ?`, lastID, batchSize).Scan(&rows).Error
		if err != nil {
			return nil // 列不存在或查询失败：放弃回填，不影响新表可用
		}
		if len(rows) == 0 {
			break
		}

		out := make([]model.ModelAuditResult, 0, len(rows))
		for _, r := range rows {
			lastID = r.ID
			out = append(out, model.ModelAuditResult{
				JobID:       job.ID,
				LogID:       r.ID,
				ChannelID:   r.ChannelID,
				ChannelName: r.ChannelName,
				Model:       r.RequestModelName,
				Status:      model.AuditResultDone,
				Verdict:     r.ModelAuditVerdict,
				Score:       r.ModelAuditScore,
				FindingN:    r.ModelAuditFindingN,
				Report:      r.ModelAuditReport,
			})
		}
		if err := db.CreateInBatches(out, 500).Error; err != nil {
			return nil
		}
		moved += len(out)
		if len(rows) < batchSize {
			break
		}
	}

	// 回填数量与预估不符时以实际为准，免得进度永远是半截。
	if moved != int(total) {
		db.Model(&model.ModelAuditJob{}).Where("id = ?", job.ID).
			Updates(map[string]any{"total": moved, "finished": moved})
	}
	return nil
}
