package migrate

import (
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 26,
		Up:      migratePromptOverrideSwitch,
	})
}

// migratePromptOverrideSwitch 把系统提示词从「两种模式」换成「开关 + 前面/后面」：
//
//	旧模式 append_system（另加一条）/ replace_system（替换原有）/ 空  →  mode = before
//	开关 prompt_override_enabled（AutoMigrate 刚加的列，默认 false） →  有文本才开
//
// 方案、模型路由规则、渠道三张表 + 全局设置都这么改。旧的 replace_system 会删掉客户端
// 自带的 system，新版不再删，归到「开 + 前面」——和以前一样一定生效、排在最前。
// 幂等：只动还是旧模式值的行，跑过一次后都是 before，再跑不会把用户手动关掉的开关打开。
func migratePromptOverrideSwitch(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	legacy := []string{string(model.PromptOverrideModeAppendSystem), string(model.PromptOverrideModeReplaceSystem), ""}
	for _, table := range []any{&model.AccessPlan{}, &model.AccessRouteRule{}, &model.Channel{}} {
		m := db.Migrator()
		if !m.HasTable(table) || !m.HasColumn(table, "prompt_override_mode") || !m.HasColumn(table, "prompt_override_enabled") {
			continue
		}
		res := db.Model(table).
			Where("prompt_override_mode IN ? OR prompt_override_mode IS NULL", legacy).
			Updates(map[string]any{
				"prompt_override_enabled": gorm.Expr("(TRIM(COALESCE(system_prompt_override, '')) <> '')"),
				"prompt_override_mode":    string(model.PromptOverrideModeBefore),
			})
		if res.Error != nil {
			return fmt.Errorf("migrate prompt override switch: %w", res.Error)
		}
		if res.RowsAffected > 0 {
			log.Infof("migration 026: %d row(s) moved to prompt_override_mode=before, switch on where text is set", res.RowsAffected)
		}
	}

	if !db.Migrator().HasTable(&model.Setting{}) {
		return nil
	}
	var mode model.Setting
	if err := db.Where(model.SettingKeyIs(model.SettingKeyPromptOverrideMode)).Limit(1).Find(&mode).Error; err != nil {
		return fmt.Errorf("read %s: %w", model.SettingKeyPromptOverrideMode, err)
	}
	if mode.Key == "" || !isLegacyPromptMode(mode.Value) {
		return nil
	}
	var text model.Setting
	if err := db.Where(model.SettingKeyIs(model.SettingKeyPromptOverrideSystem)).Limit(1).Find(&text).Error; err != nil {
		return fmt.Errorf("read %s: %w", model.SettingKeyPromptOverrideSystem, err)
	}
	enabled := "false"
	if strings.TrimSpace(text.Value) != "" {
		enabled = "true"
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Setting{}).Where(model.SettingKeyIs(model.SettingKeyPromptOverrideMode)).
			Update("value", string(model.PromptOverrideModeBefore)).Error; err != nil {
			return fmt.Errorf("update %s: %w", model.SettingKeyPromptOverrideMode, err)
		}
		var count int64
		if err := tx.Model(&model.Setting{}).Where(model.SettingKeyIs(model.SettingKeyPromptOverrideEnabled)).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			if err := tx.Create(&model.Setting{Key: model.SettingKeyPromptOverrideEnabled, Value: enabled}).Error; err != nil {
				return fmt.Errorf("create %s: %w", model.SettingKeyPromptOverrideEnabled, err)
			}
		} else if err := tx.Model(&model.Setting{}).Where(model.SettingKeyIs(model.SettingKeyPromptOverrideEnabled)).
			Update("value", enabled).Error; err != nil {
			return fmt.Errorf("update %s: %w", model.SettingKeyPromptOverrideEnabled, err)
		}
		log.Infof("migration 026: global prompt mode %q -> before, switch %s", mode.Value, enabled)
		return nil
	})
}

func isLegacyPromptMode(v string) bool {
	return v == "" || model.IsLegacyPromptOverrideMode(model.PromptOverrideMode(v))
}
