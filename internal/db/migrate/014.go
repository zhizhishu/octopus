package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func init() {
	// AlwaysRun 是必需的, 不是优化。
	//
	// 框架对没标 AlwaysRun 的迁移只跑一次: Up() 一返回 nil 就记 success, 之后
	// 每次启动都跳过。而本迁移的回填天生可能半途而废(行太多、库忙、进程被杀),
	// 注释里"留待下次启动继续"的承诺只有靠每次重跑才成立。标上它之后:
	//   - 真正跑完 = 表里已无缺失行 -> 立刻返回 nil, 每次启动只是一条 LEFT JOIN
	//     探测, 不重灌(全量重灌在百万行上是分钟级)。
	//   - 跑不完 -> 也该返回 nil(见下), 下次启动继续, 永远不阻断启动。
	RegisterAfterAutoMigration(Migration{
		Version:   14,
		Up:        migrateBackfillRelayLogSearchIndex,
		AlwaysRun: true,
	})
}

// RelayLogSearchBackfillState 是"存量到底补齐了没有"那唯一一行标记。
//
// 为什么不能只认 migration_records 里的 success: 014 在探测/回填中途失败时**刻意
// 返回 nil**(不让一次可降级的回填故障把服务挡在启动之外), 而框架收到 nil 就记
// success(migrate.go:117-121)。于是 success 只代表"这一遍跑完了", 不代表"投影表
// 完整"。搜索侧拿它当完整性判据时会直接返回空页(internal/op/log_search_index.go),
// 那批没回填的老日志就永久搜不到, 而页面看起来完全正常。
//
// 单独一张表而不是塞进 settings: settings 会被 SettingList / 备份导出带走,
// 换一台机器导入之后"表是完整的"这个断言就是假的。
type RelayLogSearchBackfillState struct {
	ID       int  `gorm:"primaryKey"`
	Complete bool `gorm:"not null"`
}

// relayLogSearchBackfillStateRowID 是单行表的主键。
const relayLogSearchBackfillStateRowID = 1

// relayLogSearchBackfillMark 写入/更新完整性标记。
//
// 失败只记日志: 标记写不上 = 搜索侧一直走主表 LIKE 退路, 慢但正确,
// 不该让启动失败, 也不该让搜索变成空页。
func relayLogSearchBackfillMark(db *gorm.DB, complete bool) {
	if db == nil {
		return
	}
	if err := db.AutoMigrate(&RelayLogSearchBackfillState{}); err != nil {
		log.Warnf("migrate 014: ensure backfill marker table failed: %v", err)
		return
	}
	row := RelayLogSearchBackfillState{ID: relayLogSearchBackfillStateRowID, Complete: complete}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"complete"}),
	}).Create(&row).Error; err != nil {
		log.Warnf("migrate 014: write backfill marker (complete=%v) failed: %v", complete, err)
	}
}

// RelayLogSearchBackfillComplete 报告存量行是否已经真的补齐。
//
// 表不存在(迁移还没跑到)、没有那一行、或标记为 false, 一律算"没补齐" ——
// 宁可让搜索走慢路径, 也不能让它对着缺行的投影表宣布空页。
func RelayLogSearchBackfillComplete(tx *gorm.DB) bool {
	if tx == nil || !tx.Migrator().HasTable(&RelayLogSearchBackfillState{}) {
		return false
	}
	var st RelayLogSearchBackfillState
	if err := tx.Take(&st, relayLogSearchBackfillStateRowID).Error; err != nil {
		return false
	}
	return st.Complete
}

// 014: 建 relay_log_search 投影表并回填存量日志的搜索列。
//
// 背景(实测 5 万行): 日志页搜索走 12 个字段的 LIKE '%x%'。前缀通配符注定用不上
// 索引, 只能全表逐行扫描; 而 relay_logs 每行带着 request_content/response_content
// (真实日志常在几百字节到几十 KB), 扫描要为每一行读溢出页。
//   - 在 relay_logs 上搜: 277ms
//   - 在只含搜索列的窄表上搜: 9ms
//   - 只搜 1 列 vs 12 列: 266ms vs 420ms(可见慢的不是字段数, 是回表读大字段)
//
// 所以把参与搜索的列投影成一张窄表, 搜索先在这里拿到 id, 再回主表取那 20 行。
//
// 安全性:
//   - 只增不改: 只建新表 + 灌数据, 不动 relay_logs 任何一列。
//   - 幂等: 建表用 IF NOT EXISTS 语义(AutoMigrate), 回填用 INSERT ... ON CONFLICT
//     DO NOTHING / 先 DELETE 后 INSERT, 重复跑不会重复插入也不会报错。
//   - 分段回填: 一次 INSERT ... SELECT 会在大表上长时间占锁; 按 id 区间分批,
//     每批之间让出, 启动不会因为几百万行日志而卡住。
//   - 可失败降级: 回填中途失败留待下次启动继续(记录只在成功后写入), 而运行时
//     写入路径会继续补齐新行 —— 缺的行只是"暂时搜不到", 不会搜错。
func migrateBackfillRelayLogSearchIndex(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("relay_logs") {
		return nil
	}

	if err := db.AutoMigrate(&model.RelayLogSearchIndex{}); err != nil {
		return fmt.Errorf("create relay_log_search: %w", err)
	}

	// 先把标记置成"没补齐": 这一遍还没走完, 中途失败/被杀都不能留着一个过去的 true。
	// 重启时这里会重跑, 探测到没有缺行再置回 true。
	relayLogSearchBackfillMark(db, false)

	// 顺带给主表的 total_attempts 建索引。
	//
	// 严重程度分桶(warn = total_attempts > 1)和 retried 筛选都是在它上面做的判断,
	// 但这一列此前没有索引, 两个谓词只能全表逐行求值: 实测 10 万行 847ms。
	// 加上索引后同样的谓词 10.5ms。新库由 AutoMigrate 按 struct tag 直接建出,
	// 这里是为了让已经存在的库也补上。
	//
	// 建索引在百万行上是秒级操作且只跑一次; 失败只记日志不阻断启动 ——
	// 索引缺失只是慢, 不该让服务起不来。
	// 刻意忽略错误: 迁移层没有 logger, 而索引建不上只是变慢、不影响正确性,
	// 不该让整个迁移失败、把服务挡在启动之外。下次启动会再试一次。
	_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_relay_logs_total_attempts ON relay_logs(total_attempts)").Error

	// 先补齐"主表有新行、索引表没有"的部分。左连接取缺失 id, 避免每次启动都
	// 全量重灌(全量重灌在百万行上是分钟级, 而缺行通常是 0)。
	const batchSize = 2000
	for {
		var ids []int64
		err := db.Raw(`
			SELECT l.id FROM relay_logs l
			LEFT JOIN relay_log_search s ON s.id = l.id
			WHERE s.id IS NULL
			ORDER BY l.id
			LIMIT ?`, batchSize).Scan(&ids).Error
		if err != nil {
			// 🔴 返回 nil，不是 err。
			//
			// 框架契约(migrate.go:17-19)原文：Up MUST be idempotent and MUST return
			// nil — not an error — when it intentionally can't finish yet, so a
			// transient skip never aborts startup. 而 runMigrationsWithRecord 一收到
			// 非 nil 就记 failed 并 return err(migrate.go:117-121)，AfterAutoMigrate
			// 再 return err，db.InitDB 再 return err，cmd/start.go 只 log.Errorf 然后
			// return —— 进程直接退出，服务起不来。
			//
			// 一次可降级的回填故障(库忙、连接抖动、进程被杀)不该让整个服务下线。
			// 续跑能力本来就由 AlwaysRun 提供：下次启动会再试一遍，不需要用错误去唤醒。
			// 这里记 warn 留证据，然后放行。
			log.Warnf("migrate 014: probe missing rows failed, will retry next startup: %v", err)
			return nil
		}
		if len(ids) == 0 {
			// 探测到"主表有、投影表没有"的行数为 0 —— 这才是真的补齐了,
			// 也只有这里和下面"最后一批"那处可以置 true。
			relayLogSearchBackfillMark(db, true)
			return nil
		}

		// 按 id 逐条 UPDATE-or-INSERT 等价物: 用 INSERT ... SELECT 批量灌,
		// 冲突则忽略, 保持幂等。
		rows := make([]model.RelayLogSearchIndex, 0, len(ids))
		var src []model.RelayLog
		if err := db.Select(
			"id, user_name, request_api_key_name, request_model_name, actual_model_name, "+
				"upstream_response_model, channel_name, request_endpoint, request_path, "+
				"session_key, error, error_code, request_user_agent, channel_id",
		).Where("id IN ?", ids).Find(&src).Error; err != nil {
			log.Warnf("migrate 014: read relay_logs batch failed, will retry next startup: %v", err)
			return nil
		}
		for _, l := range src {
			rows = append(rows, model.RelayLogSearchIndex{
				ID:                    l.ID,
				UserName:              l.UserName,
				RequestAPIKeyName:     l.RequestAPIKeyName,
				RequestModelName:      l.RequestModelName,
				ActualModelName:       l.ActualModelName,
				UpstreamResponseModel: l.UpstreamResponseModel,
				ChannelName:           l.ChannelName,
				RequestEndpoint:       l.RequestEndpoint,
				RequestPath:           l.RequestPath,
				SessionKey:            l.SessionKey,
				Error:                 l.Error,
				ErrorCode:             l.ErrorCode,
				RequestUserAgent:      l.RequestUserAgent,
				ChannelId:             l.ChannelId,
			})
		}
		if len(rows) == 0 {
			return nil
		}
		// 只插不存在的: OnConflict DoNothing 在 sqlite/mysql/pg 上都可用,
		// 避免并发写入路径同时插入时撞主键。
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error; err != nil {
			log.Warnf("migrate 014: insert relay_log_search batch failed, will retry next startup: %v", err)
			return nil
		}
		if len(ids) < batchSize {
			// 不足一批 = 缺失 id 已经取完并灌完, 同样算补齐。
			relayLogSearchBackfillMark(db, true)
			return nil
		}
	}
}
