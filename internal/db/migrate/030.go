package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 30,
		Up:      migrateChannelProbeOverrides,
	})
}

// migrateChannelProbeOverrides 把渠道级探测覆写三个新列（probe_daily_cap /
// probe_max_failures / probe_model）在存量行上收敛成确定的零值。
//
// 列本身由 AutoMigrate 加（model.Channel.ProbeDailyCap / ProbeMaxFailures /
// ProbeModel），这条迁移只负责把老行写成确定值：
//
//   - 加列时存量行吃不吃 DEFAULT 取决于方言和版本，读进 int/string 只是*碰巧*
//     等于零值——那不该是行为保证；
//   - 语义上也确实要求零值：渠道级覆写的 0 表达「沿用全局」，NULL 读进程序却会被
//     当成无覆写，两边必须落成同一个确定值。
//
// 幂等：只动还是空值的行，跑第二次什么都不改；也绝不覆盖运营商后来显式设置的
// 非零覆写。
func migrateChannelProbeOverrides(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	m := db.Migrator()
	if !m.HasTable(&model.Channel{}) ||
		!m.HasColumn(&model.Channel{}, "probe_daily_cap") ||
		!m.HasColumn(&model.Channel{}, "probe_max_failures") ||
		!m.HasColumn(&model.Channel{}, "probe_model") {
		return nil
	}
	if err := convergeChannelProbeOverrideZeros(db, "probe_daily_cap", 0); err != nil {
		return err
	}
	if err := convergeChannelProbeOverrideZeros(db, "probe_max_failures", 0); err != nil {
		return err
	}
	return convergeChannelProbeOverrideZeros(db, "probe_model", "")
}

// convergeChannelProbeOverrideZeros 把一列上还是 NULL 的行写成确定的零值。
// 只动 NULL 行：反过来会覆盖运营商后来显式设置的值。
func convergeChannelProbeOverrideZeros(db *gorm.DB, column string, zero any) error {
	res := db.Model(&model.Channel{}).
		Where(column+" IS NULL").
		Update(column, zero)
	if res.Error != nil {
		return fmt.Errorf("migrate channel %s: %w", column, res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infof("migration 030: %d channel(s) converged to %s=%v", res.RowsAffected, column, zero)
	}
	return nil
}
