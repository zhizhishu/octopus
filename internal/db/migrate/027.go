package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 27,
		Up:      migrateChannelProbeEnabledOff,
	})
}

// migrateChannelProbeEnabledOff 把「熔断探测自动开关」在存量渠道上收敛成明确的 false。
//
// 列本身由 AutoMigrate 加（model.Channel.ProbeEnabled，not null default false），这条
// 迁移只负责把老行写成确定的关闭值：
//
//   - 加列时存量行吃不吃 DEFAULT 取决于方言和版本，空的 probe_enabled 读进 bool 只是
//     *碰巧*等于 false——那不该是行为保证；
//   - 语义上也确实要求关闭：探测是真实出站的计费请求，一次升级不该让全站渠道自己开始
//     打上游。所以出厂既关总开关（circuit_probe_enabled），也把每个渠道的开关写成关闭。
//
// 幂等：只动还是空值的行，跑第二次什么都不改；也绝不覆盖运营商后来手动打开的
// probe_enabled=true。
func migrateChannelProbeEnabledOff(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	m := db.Migrator()
	if !m.HasTable(&model.Channel{}) || !m.HasColumn(&model.Channel{}, "probe_enabled") {
		return nil
	}
	// 显式 false：这里刻意不写 `Update("probe_enabled", false)` 之外的任何东西，
	// 也不按渠道类型/能力位去猜——存量渠道的自动探测口径就是「关」。
	res := db.Model(&model.Channel{}).
		Where("probe_enabled IS NULL").
		Update("probe_enabled", false)
	if res.Error != nil {
		return fmt.Errorf("migrate channel probe_enabled: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infof("migration 027: %d channel(s) converged to probe_enabled=false", res.RowsAffected)
	}
	return nil
}
