package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 32,
		Up:      migrateCreativePNGColumns,
	})
}

// migrateCreativePNGColumns 把鹈鹕图自动存 PNG 的两列（png_data / png_size）在
// 存量行上收敛成确定的「还没有 PNG」：png_size 的 NULL 一律写成 0。
//
// 列本身由 AutoMigrate 加（model.ModelAuditCreative.PNGData / PNGSize，表本来就在
// db.go 的 autoMigrateModels 名单里）；这条迁移只负责老行的语义收敛，理由同 030：
// 加列时存量行吃不吃 DEFAULT 取决于方言和版本，而 png_size = 0 不只是展示——
// op.ModelAuditCreativeSetPNG 的防重复上传守卫（WHERE png_size = 0）和前端的
// 「还要不要传」判断都拿它当事实，NULL 会让这些行永远传不进去。
// png_data（字节列）NULL 就是「没有图」，不需要收敛。
//
// 幂等：只动还是 NULL 的行，跑第二次什么都不改。
func migrateCreativePNGColumns(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	m := db.Migrator()
	if !m.HasTable(&model.ModelAuditCreative{}) {
		return nil
	}
	if !m.HasColumn(&model.ModelAuditCreative{}, "PNGSize") {
		if err := m.AddColumn(&model.ModelAuditCreative{}, "PNGSize"); err != nil {
			return fmt.Errorf("migrate creative png_size: add column: %w", err)
		}
	}
	// 列名是本文件里的字面量，不含外部输入；用 Sprintf 拼而不是运算符拼接，
	// 躲开不同 gofmt 版本对二元运算符空格规则的分歧（同 029）。
	res := db.Model(&model.ModelAuditCreative{}).
		Where("png_size IS NULL").
		Update("png_size", 0)
	if res.Error != nil {
		return fmt.Errorf("migrate creative png_size: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Infof("migration 032: %d creative row(s) converged to png_size=0", res.RowsAffected)
	}
	return nil
}
