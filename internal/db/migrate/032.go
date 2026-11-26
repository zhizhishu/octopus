package migrate

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 32,
		Up:      migrateSystemAPIKey,
	})
}

// SystemAPIKeyName 是内置「系统」Key 的名字。前端与运维都靠这个名字认出它；
// 幂等判定不认名字而认 system 标记（见下），改名不会导致重建。
const SystemAPIKeyName = "系统"

// migrateSystemAPIKey 给 api_keys 加 system 布尔列（列本体由 AutoMigrate 按
// model.APIKey.System 加，这里只是防御性补齐），并自动建一把名为「系统」的 API Key。
//
// 这把 Key 的用途：进程内部的渠道测试 / 模型检测改走正常 relay 转发链路时，
// 需要一把真实存在的 Key 来承载这次转发（relay 链路的日志、计费口径、attempts 都以
// api_key_id 记账）。它不是任何用户的 Key：外部鉴权对它恒拒绝
// （middleware.ValidateAPIKey），op 层拒绝删除/停用/编辑，明文也不出列表。
//
// 归属管理员的判定依据（写在代码里是因为这条迁移不能 import op——op 反过来
// import 本包，成环）：系统里没有「固定根用户」常量，既有逻辑（op.UserInit 的
// backfillLegacyOwnership / op.UserDefaultAdmin）的口径是** role=admin 且 id 最小的
// 那个用户**。这里用同一份 SQL 口径直查 users 表。
//
// 全新装库的时序问题：迁移在 db.InitDB 里跑，而初始 admin 是随后 op.UserInit 才建的，
// 这时一个用户都没有。对策：找不到 admin 就用 user_id=0 先建行——op.UserInit 的
// backfillLegacyOwnership 本来就会把 user_id=0 的 Key 全部划给 admin（老库归属
// 回填的既有行为），同一次启动内就完成归属，不需要任何新接线。
//
// 密钥值：与 handlers 建 Key 用的是同一种形态（"sk-"+APP_NAME+"-"+48 位字母数字）。
// 不能用 server/auth.GenerateAPIKey——server/auth import op、op import 本包，环。
// 因此在本地复刻同一算法（字符集与长度逐字一致），差异只剩实现位置。
//
// 幂等：与 031 同一套——同事务先 ensureMigrationRecordTable、查到 version 32 已
// success 就直接返回；否则建行 + upsert 记录一起提交。判定「已建过」认 system 标记
// 不认名字：管理员就算改了名，重跑也不会再造一把。
func migrateSystemAPIKey(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := ensureMigrationRecordTable(tx); err != nil {
			return fmt.Errorf("ensure migration record table: %w", err)
		}
		var rec MigrationRecord
		if err := tx.Where("version = ?", 32).Limit(1).Find(&rec).Error; err != nil {
			return fmt.Errorf("read migration record 032: %w", err)
		}
		if rec.Version == 32 && rec.Status == MigrationRecordStatusSuccess {
			return nil
		}

		if err := ensureSystemKeyColumn(tx); err != nil {
			return err
		}
		if err := seedSystemAPIKey(tx); err != nil {
			return err
		}
		if err := upsertMigrationRecord(tx, 32, MigrationRecordStatusSuccess); err != nil {
			return fmt.Errorf("record migration 032 success: %w", err)
		}
		log.Infof("migration 032: system api key ensured")
		return nil
	})
}

// MigrateSystemAPIKeyForTest 把 032 的 Up 暴露给包内测试（与 031 的同款导出一致）。
func MigrateSystemAPIKeyForTest(db *gorm.DB) error {
	return migrateSystemAPIKey(db)
}

// ensureSystemKeyColumn 防御性补齐 api_keys.system。正常路径下 AutoMigrate 已经按
// model.APIKey.System 把列建出来了（本迁移注册在 AfterAutoMigration），这里只是
// 照顾「直跑 Up 的测试 / 异常中途库」：缺列时补列，有列时什么都不做。
func ensureSystemKeyColumn(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&model.APIKey{}) {
		return nil
	}
	if tx.Migrator().HasColumn(&model.APIKey{}, "system") {
		return nil
	}
	if err := tx.Migrator().AddColumn(&model.APIKey{}, "system"); err != nil {
		return fmt.Errorf("add api_keys.system column: %w", err)
	}
	return nil
}

// seedSystemAPIKey 幂等建行：已有任何一把 system=true 的 Key 就直接返回
// （不验名字、不验归属——system 标记本身就是「这是那把内部 Key」的充分条件，
// 重跑绝不复建、删除也不复活…… 注意：系统 Key 在 op 层根本删不掉，所以「删除不复活」
// 只是针对直接动库的场景，语义与 031 的防复活一致）。
func seedSystemAPIKey(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&model.APIKey{}) {
		return nil
	}
	var existing model.APIKey
	if err := tx.Where("system = ?", true).Limit(1).Find(&existing).Error; err != nil {
		return fmt.Errorf("read existing system api key: %w", err)
	}
	if existing.ID > 0 {
		return nil
	}

	// 归属：role=admin 且 id 最小（与 op.UserDefaultAdmin 同口径）。全新装库时还没有
	// 任何用户——admin 要等同一次启动稍后的 op.UserInit 才建——这时先写 user_id=0，
	// UserInit 的 backfillLegacyOwnership 会把 user_id=0 的 Key 划给新建的 admin。
	ownerID := 0
	if tx.Migrator().HasTable(&model.User{}) {
		var admin model.User
		if err := tx.Where("role = ?", model.UserRoleAdmin).Order("id ASC").Limit(1).Find(&admin).Error; err != nil {
			return fmt.Errorf("read default admin user: %w", err)
		}
		if admin.ID > 0 {
			ownerID = admin.ID
		}
	}

	secret, err := generateSystemAPIKeySecret()
	if err != nil {
		return fmt.Errorf("generate system api key secret: %w", err)
	}
	key := model.APIKey{
		UserID:  ownerID,
		Name:    SystemAPIKeyName,
		APIKey:  secret,
		Enabled: true,
		System:  true,
		Note:    "内置系统 Key：渠道测试/模型检测走正常转发链路专用；外部不可用，勿删勿改。",
	}
	if err := tx.Create(&key).Error; err != nil {
		return fmt.Errorf("create system api key: %w", err)
	}
	if ownerID == 0 {
		log.Infof("migration 032: system api key created with pending ownership (no admin user yet; user init will assign it)")
	}
	return nil
}

// generateSystemAPIKeySecret 复刻 server/auth.GenerateAPIKey 的算法：
// "sk-" + APP_NAME + "-" + 48 位 [0-9a-zA-Z] 密码学随机字符。
// 不能直接用原函数的原因见文件头（import 环）。形状必须与它逐字一致——
// middleware.ValidateAPIKey 按 "sk-"+APP_NAME+"-" 前缀认 Key。
func generateSystemAPIKeySecret() (string, error) {
	const keyChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 48)
	maxI := big.NewInt(int64(len(keyChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, maxI)
		if err != nil {
			return "", err
		}
		b[i] = keyChars[n.Int64()]
	}
	return "sk-" + conf.APP_NAME + "-" + string(b), nil
}
