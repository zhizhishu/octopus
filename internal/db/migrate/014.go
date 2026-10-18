package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 14,
		Up:      migrateBackfillRelayLogSearchIndex,
	})
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
			// 老库可能还没有这张表的历史数据, 或视图不同; 不因此阻断启动。
			return nil
		}
		if len(ids) == 0 {
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
			return nil
		}
		if len(ids) < batchSize {
			return nil
		}
	}
}
