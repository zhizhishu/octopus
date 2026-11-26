package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 29,
		Up:      migrateChannelQueueSettings,
	})
}

// migrateChannelQueueSettings 把「渠道排队可调」的两个旋钮（queue_size / queue_timeout_ms）
// 在存量渠道上收敛成确定的 0：列由 AutoMigrate 加，这里补缺列并清掉残留 NULL，
// 让「存量渠道不排队」成为库里的确定事实（而不是「读出来碰巧是 0」）。
// 幂等：只动 NULL 行，跑第二次什么都不改，也绝不覆盖运营商自己配的值。
func migrateChannelQueueSettings(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	m := db.Migrator()
	if !m.HasTable(&model.Channel{}) {
		return nil
	}
	// 字段名（GORM Migrator 用）与列名（UPDATE 用）都是本文件里的字面量，不含外部输入。
	if err := convergeChannelColumnToZero(db, m, "QueueSize", "queue_size"); err != nil {
		return err
	}
	return convergeChannelColumnToZero(db, m, "QueueTimeoutMs", "queue_timeout_ms")
}

// convergeChannelColumnToZero 保证 channels.<column> 这一列存在、且没有残留 NULL。
// 缺列时补列；只把 NULL 写成 0，已有值（包括运营商自己配的非 0 值）一律不动。
func convergeChannelColumnToZero(db *gorm.DB, m gorm.Migrator, field, column string) error {
	if !m.HasColumn(&model.Channel{}, field) {
		if err := m.AddColumn(&model.Channel{}, field); err != nil {
			return fmt.Errorf("migrate channel %s: add column: %w", column, err)
		}
	}
	// 列名是本文件里的字面量，不含外部输入；用 Sprintf 拼而不是运算符拼接，
	// 也躲开不同 gofmt 版本对二元运算符空格规则的分歧。
	res := db.Model(&model.Channel{}).
		Where(fmt.Sprintf("%s IS NULL", column)).
		Update(column, 0)
	if res.Error != nil {
		return fmt.Errorf("migrate channel %s: %w", column, res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infof("migration 029: %d channel(s) converged to %s=0", res.RowsAffected, column)
	}
	return nil
}
