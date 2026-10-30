package migrate

import (
	"fmt"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func init() {
	// AlwaysRun 是必需的，不是优化。
	//
	// 回填按批走，单次启动只做一小段（见 relayLogSeverityStartupBudget），
	// 剩下的交给后台续跑；进程在某一批中途被杀掉也一样。框架对没标 AlwaysRun
	// 的迁移只跑一次：Up() 返回 nil 就记 success，之后每次启动都跳过，
	// 没写完的行就永远停在 severity=0。标上之后：
	//   - 真的跑完 = 表里已无 severity=0 -> 立刻返回，每次启动只是一次索引探测。
	//   - 还没跑完 = 也返回 nil（见下），下次启动接着跑，永远不阻断启动。
	RegisterAfterAutoMigration(Migration{
		Version:   21,
		Up:        migrateBackfillRelayLogSeverity,
		AlwaysRun: true,
	})
}

// RelayLogSeverityBackfillState 是「存量 severity 到底补齐了没有」那一行标记。
//
// migration_records 的 success 只表示「这一遍 Up 返回了 nil」。本迁移在预算用完
// 或读批失败时故意返回 nil，所以 success ≠ 补齐。计数热路径不读这张表（它直接
// 探测 severity=0，探测本身走索引），标记给后台循环和测试用。
type RelayLogSeverityBackfillState struct {
	ID       int  `gorm:"primaryKey"`
	Complete bool `gorm:"not null"`
}

const relayLogSeverityBackfillStateRowID = 1

// relayLogSeverityBackfillBatch 是一批回填的行数。
//
// 一批要读 error/error_code/error_status/total_attempts。这几列在大 TEXT
// （request_content / upstream_request_body / response_content）后面，SQLite
// 读它们会顺着溢出页走一遍，随后 UPDATE 还要把整行写回去。几千行一批把锁和
// WAL 切成小段：中断只丢掉当前这一批（已提交的留下），下一次从 severity=0 接着做。
const relayLogSeverityBackfillBatch = 2000

// relayLogSeverityStartupBudget 是启动阶段最多花在回填上的时间。
//
// 18GB 的库把每一行的正文都读一遍再写回去，不是启动该等的事情。预算用完就返回，
// 服务先起来；同一进程里的后台循环（op.RelayLogSeverityBackfillStart）继续跑，
// 下次启动 AlwaysRun 也会接着跑。小库在预算内就能补完，测试不用等后台。
const relayLogSeverityStartupBudget = 3 * time.Second

// RelayLogSeverityBackfillOptions 控制一次回填调用做多少。
//
// MaxRows / Budget 为 0 表示不设该上限。两者都为 0 时一直跑到没有 severity=0 为止。
type RelayLogSeverityBackfillOptions struct {
	MaxRows int
	Budget  time.Duration
}

// RelayLogSeverityBackfillComplete 报告标记是否为「已补齐」。
// 表不存在、没有那一行、或标记为 false，一律算没补齐。
func RelayLogSeverityBackfillComplete(tx *gorm.DB) bool {
	if tx == nil || !tx.Migrator().HasTable(&RelayLogSeverityBackfillState{}) {
		return false
	}
	var st RelayLogSeverityBackfillState
	if err := tx.Take(&st, relayLogSeverityBackfillStateRowID).Error; err != nil {
		return false
	}
	return st.Complete
}

func relayLogSeverityBackfillMark(db *gorm.DB, complete bool) {
	if db == nil {
		return
	}
	if err := db.AutoMigrate(&RelayLogSeverityBackfillState{}); err != nil {
		log.Warnf("migrate 021: ensure backfill marker table failed: %v", err)
		return
	}
	row := RelayLogSeverityBackfillState{ID: relayLogSeverityBackfillStateRowID, Complete: complete}
	// OnConflict 在 sqlite / mysql / postgres 上都能生成正确的 upsert。
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"complete"}),
	}).Create(&row).Error; err != nil {
		log.Warnf("migrate 021: write backfill marker (complete=%v) failed: %v", complete, err)
	}
}

func migrateBackfillRelayLogSeverity(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// 列加不上去是结构性故障：没有 severity，计数无法切换，应该让启动失败被看见。
	// 回填本身的失败不是——见下面 Backfill 的错误处理。
	if err := ensureRelayLogSeverityColumn(db); err != nil {
		return err
	}
	// 索引缺失只是慢。建索引失败记日志，不挡启动；计数会一直走能出正确答案的路径。
	ensureRelayLogSeverityIndexes(db)

	if _, err := BackfillRelayLogSeverity(db, RelayLogSeverityBackfillOptions{
		Budget: relayLogSeverityStartupBudget,
	}); err != nil {
		// 框架契约：Up 在「这回做不完」时必须返回 nil，非 nil 会记 failed 并让
		// InitDB 失败、进程退出。读批失败（库忙、列被改、进程将死）留下
		// severity=0，下次启动 / 后台循环继续。计数在此期间走旧 TRIM 条件，数字仍对。
		log.Warnf("migrate 021: severity backfill stopped, will retry: %v", err)
		return nil
	}
	return nil
}

func ensureRelayLogSeverityColumn(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable(&model.RelayLog{}) {
		return nil
	}
	if db.Migrator().HasColumn(&model.RelayLog{}, "Severity") {
		return nil
	}
	if err := db.Migrator().AddColumn(&model.RelayLog{}, "Severity"); err != nil {
		return fmt.Errorf("add relay_logs.severity: %w", err)
	}
	return nil
}

// ensureRelayLogSeverityIndexes 建两条索引：
//   - idx_relay_logs_severity：探测「还有没有 severity=0」、回填的 WHERE severity=0、
//     以及按桶过滤的列表。
//   - idx_relay_logs_time_severity：(time, severity) 覆盖索引，计数只读索引。
//
// MySQL 没有 CREATE INDEX IF NOT EXISTS（MariaDB 有，MySQL 没有）。错误若被吞掉，
// 索引就永远建不上。mysql 先 HasIndex 再裸 CREATE；sqlite / postgres 用 IF NOT EXISTS。
// 名字必须和 struct tag 里 GORM 生成的一致，否则 HasIndex 对不上、每次启动都重建。
func ensureRelayLogSeverityIndexes(db *gorm.DB) {
	if db == nil || db.Dialector == nil || !db.Migrator().HasTable(&model.RelayLog{}) {
		return
	}
	specs := []struct {
		name string
		cols string
	}{
		{"idx_relay_logs_severity", "severity"},
		{"idx_relay_logs_time_severity", "time, severity"},
	}
	for _, spec := range specs {
		switch db.Dialector.Name() {
		case "mysql":
			if db.Migrator().HasIndex(&model.RelayLog{}, spec.name) {
				continue
			}
			if err := db.Exec("CREATE INDEX " + spec.name + " ON relay_logs(" + spec.cols + ")").Error; err != nil {
				log.Warnf("migrate 021: ensure %s failed: %v", spec.name, err)
			}
		default:
			if err := db.Exec("CREATE INDEX IF NOT EXISTS " + spec.name + " ON relay_logs(" + spec.cols + ")").Error; err != nil {
				log.Warnf("migrate 021: ensure %s failed: %v", spec.name, err)
			}
		}
	}
}

// BackfillRelayLogSeverity 把 severity=0 的行按 id 升序分批写成
// ClassifyRelayLogSeverity 的结果。
//
// 返回 complete=true 表示表里已经没有 severity=0。complete=false 且 err=nil
// 表示这回碰到了 MaxRows / Budget，调用方应稍后再调。err != nil 是读或写失败，
// 已提交的批留在库里，未提交的下回重做。
//
// 分类在 Go 里做，而不是一条 UPDATE ... CASE TRIM()。SQL TRIM 只剥空格，
// 和 Go TrimSpace / 前端 trim 不一致；回填如果用 SQL，会把这个分歧写进永久列。
func BackfillRelayLogSeverity(db *gorm.DB, opt RelayLogSeverityBackfillOptions) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("relay_logs") {
		relayLogSeverityBackfillMark(db, true)
		return true, nil
	}
	if !db.Migrator().HasColumn(&model.RelayLog{}, "Severity") {
		return false, fmt.Errorf("relay_logs.severity is missing")
	}

	pending, err := relayLogSeverityUnsetPending(db)
	if err != nil {
		return false, err
	}
	if !pending {
		relayLogSeverityBackfillMark(db, true)
		return true, nil
	}
	// 只有真的还有未回填行才把标记打成 false。已经补齐的库不要先写 false
	// 再写回 true——那一窗口里崩了，标记会撒谎。
	relayLogSeverityBackfillMark(db, false)

	batch := relayLogSeverityBackfillBatch
	updated := 0
	started := time.Now()
	for {
		n := batch
		if opt.MaxRows > 0 {
			left := opt.MaxRows - updated
			if left <= 0 {
				return false, nil
			}
			if left < n {
				n = left
			}
		}
		got, err := backfillRelayLogSeverityBatch(db, n)
		if err != nil {
			return false, err
		}
		if got == 0 {
			relayLogSeverityBackfillMark(db, true)
			return true, nil
		}
		updated += got
		relayLogSeverityCheckpoint(db)
		// 不足一批 = 未回填的行已经取完。
		if got < n {
			relayLogSeverityBackfillMark(db, true)
			return true, nil
		}
		if opt.MaxRows > 0 && updated >= opt.MaxRows {
			return false, nil
		}
		if opt.Budget > 0 && time.Since(started) >= opt.Budget {
			log.Infof("migrate 021: severity backfill paused after %d rows (budget %s); it resumes in the background and on next startup", updated, opt.Budget)
			return false, nil
		}
	}
}

func relayLogSeverityUnsetPending(db *gorm.DB) (bool, error) {
	var probe []struct {
		OK int
	}
	err := db.Raw("SELECT 1 AS ok FROM relay_logs WHERE severity = ? LIMIT 1", model.RelayLogSeverityUnset).Scan(&probe).Error
	if err != nil {
		return false, err
	}
	return len(probe) > 0, nil
}

type relayLogSeverityBackfillRow struct {
	ID            int64
	Error         *string
	ErrorCode     *string
	ErrorStatus   *int
	TotalAttempts *int
}

func severityBackfillString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func severityBackfillInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func backfillRelayLogSeverityBatch(db *gorm.DB, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	var rows []relayLogSeverityBackfillRow
	// 只取分类要用的列。正文仍然会在磁盘上被跳过（它们排在这些列前面），
	// 但不会进到 Go 的内存里。
	err := db.Table("relay_logs").
		Select("id, error, error_code, error_status, total_attempts").
		Where("severity = ?", model.RelayLogSeverityUnset).
		Order("id ASC").
		Limit(n).
		Find(&rows).Error
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	buckets := map[int][]int64{}
	for _, row := range rows {
		sev := model.ClassifyRelayLogSeverity(model.RelayLog{
			Error:         severityBackfillString(row.Error),
			ErrorCode:     severityBackfillString(row.ErrorCode),
			ErrorStatus:   severityBackfillInt(row.ErrorStatus),
			TotalAttempts: severityBackfillInt(row.TotalAttempts),
		})
		buckets[sev] = append(buckets[sev], row.ID)
	}
	for sev, ids := range buckets {
		// 带上 severity=0，避免把并发插入、已经写好桶的行覆盖掉。
		if err := db.Table("relay_logs").
			Where("id IN ? AND severity = ?", ids, model.RelayLogSeverityUnset).
			Update("severity", sev).Error; err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

func relayLogSeverityCheckpoint(db *gorm.DB) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "sqlite" {
		return
	}
	// 每批提交后做一次被动 checkpoint，避免回填把 WAL 撑到整表那么大。
	// PASSIVE 不等待其他读者，做不完就留下，下一轮再试。
	_ = db.Exec("PRAGMA wal_checkpoint(PASSIVE)").Error
}
