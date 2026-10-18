package op

// relay_log_search 投影表的维护。
//
// 为什么要单独一个文件: 这张表是"搜索用的副本", 一致性规则集中在一处比散落在
// 写入/清理/清空各处更容易审。所有对 relay_logs 的增删都必须经过这里的函数,
// 否则会出现"日志能看到但搜不到"或"搜到已删除的 id"。
//
// 不做触发器: MySQL/PG/SQLite 的触发器语法不通用, 而且历史数据回填本来就要单独
// 走迁移(014), 两套机制并存反而更容易漏。

import (
	"context"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/db/migrate"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// relayLogSearchRowsFrom 把日志行投影成搜索表行。
func relayLogSearchRowsFrom(logs []model.RelayLog) []model.RelayLogSearchIndex {
	rows := make([]model.RelayLogSearchIndex, 0, len(logs))
	for _, l := range logs {
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
	return rows
}

// relayLogSearchIndexUpsert 写入搜索投影行。同 id 已存在时覆盖(日志内容不会变,
// 但重复写入无害, 且能修正迁移期间可能漏掉的行)。
//
// 失败不返回错误: 这是"加速用的副本", 写不进去最多是这条日志暂时搜不到,
// 不应该让日志落库本身失败。下次启动的迁移会把缺的行补上。
func relayLogSearchIndexUpsert(tx *gorm.DB, logs []model.RelayLog) {
	if len(logs) == 0 || !relayLogSearchIndexReady(tx) {
		return
	}
	rows := relayLogSearchRowsFrom(logs)
	_ = tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns(relayLogSearchIndexColumns()),
	}).CreateInBatches(rows, 200).Error
}

// relayLogSearchIndexDeleteByIDs 按 id 删投影行。
func relayLogSearchIndexDeleteByIDs(tx *gorm.DB, ids []int64) {
	if len(ids) == 0 || !relayLogSearchIndexReady(tx) {
		return
	}
	_ = tx.Where("id IN ?", ids).Delete(&model.RelayLogSearchIndex{}).Error
}

// relayLogSearchIndexDeleteWhere 按主表同款条件删投影行。
// 用于"按时间/按存储上限/按 scope 清空"这类批量删除 —— 这些路径删掉的行可能很多,
// 先查出 id 再按 id 删会撑爆内存, 所以直接把同一个 WHERE 套到投影表上。
//
// 投影表与主表共有 id/channel_id, 其余列名一一对应, 因此只有用到这些列的条件
// 才能安全下推。调用方负责只传可下推的条件(见 relayLogSearchIndexMirroredColumns)。
func relayLogSearchIndexDeleteWhere(tx *gorm.DB, apply func(*gorm.DB) *gorm.DB) {
	if !relayLogSearchIndexReady(tx) {
		return
	}
	_ = apply(tx.Table(relayLogSearchIndexTable)).Delete(&model.RelayLogSearchIndex{}).Error
}

// relayLogSearchIndexMirroredColumns 是投影表与主表同名的列。
// 判断某个 WHERE 条件能否下推到投影表时对照它。
func relayLogSearchIndexMirroredColumns() []string { return relayLogSearchIndexColumns() }

func relayLogSearchIndexColumns() []string {
	return []string{
		"user_name", "request_api_key_name", "request_model_name", "actual_model_name",
		"upstream_response_model", "channel_name", "request_endpoint", "request_path",
		"session_key", "error", "error_code", "request_user_agent", "channel_id",
	}
}

// relayLogSearchIndexReady 判断投影表是否可用(迁移是否已建好)。
// 用 HasTable 而不是缓存标记: 迁移可能在运行期才完成, 且测试里每个用例都换库。
func relayLogSearchIndexReady(tx *gorm.DB) bool {
	if tx == nil {
		return false
	}
	return tx.Migrator().HasTable(relayLogSearchIndexTable)
}

// relayLogSearchIndexRebuild 供测试与运维使用: 全量重建投影表。
func relayLogSearchIndexRebuild(ctx context.Context) error {
	if err := db.GetDB().WithContext(ctx).AutoMigrate(&model.RelayLogSearchIndex{}); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Where("1 = 1").Delete(&model.RelayLogSearchIndex{}).Error; err != nil {
		return err
	}
	const batch = 1000
	var lastID int64
	for {
		var logs []model.RelayLog
		if err := db.GetDB().WithContext(ctx).
			Select(append([]string{"id"}, relayLogSearchIndexColumns()...)).
			Where("id > ?", lastID).Order("id ASC").Limit(batch).Find(&logs).Error; err != nil {
			return err
		}
		if len(logs) == 0 {
			return nil
		}
		relayLogSearchIndexUpsert(db.GetDB().WithContext(ctx), logs)
		lastID = logs[len(logs)-1].ID
		if len(logs) < batch {
			return nil
		}
	}
}

// relayLogSearchIndexMigrationVersion 是"建表 + 回填存量"那次迁移的版本号。
const relayLogSearchIndexMigrationVersion = 14

// relayLogSearchIndexComplete 报告投影表是否已经完整回填。
//
// 用来区分两种"搜不到":
//   - 表是完整的 → 真的没有匹配行, 可以直接返回空页(快)。
//   - 表还没回填完(迁移在跑/失败过) → 老日志只存在于主表, 必须退回主表 LIKE。
//
// 判据用迁移记录而不是"行数相等": 行数在主表持续写入时会瞬时不等, 且大表上
// 每次搜索都数两遍行数本身就是笔开销。
func relayLogSearchIndexComplete(ctx context.Context) bool {
	var rec migrate.MigrationRecord
	err := db.GetDB().WithContext(ctx).
		Where("version = ? AND status = ?", relayLogSearchIndexMigrationVersion, migrate.MigrationRecordStatusSuccess).
		Take(&rec).Error
	return err == nil
}
