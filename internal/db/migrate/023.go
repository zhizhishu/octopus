package migrate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

// settingKeyLegacyClaudeCLIAutoCompact is the removed "自动压缩上下文" switch. It injected
// context_management clear_thinking keep=all, which is a no-op for current models (keep=all
// keeps everything) and a 400 when thinking is off, so it has no replacement.
const settingKeyLegacyClaudeCLIAutoCompact model.SettingKey = "claude_cli_auto_compact"

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 23,
		Up:      migrateClaudeDefaultEffortSettings,
	})
}

// migrateClaudeDefaultEffortSettings moves the saved Claude thinking settings onto the
// "默认思考强度" semantics (client first; fill only when the client sent nothing):
//
//	claude_cli_reasoning_effort: auto / "" / unknown -> high   (auto never filled anything and
//	                                                            the UI promised "default high")
//	                             off / false / disabled -> none
//	                             none/low/medium/high/xhigh/max -> unchanged
//	claude_cli_auto_compact:     row deleted (a log line is written if it was on)
//
// Idempotent: every output value is a fixed point of the mapping and the deleted row stays
// deleted, so running it again changes nothing.
func migrateClaudeDefaultEffortSettings(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("settings") {
		return nil
	}
	var effort model.Setting
	err := db.Where(model.SettingKeyIs(model.SettingKeyClaudeCLIReasoningEffort)).Take(&effort).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		return fmt.Errorf("read %s: %w", model.SettingKeyClaudeCLIReasoningEffort, err)
	default:
		next := migratedClaudeDefaultEffort(effort.Value)
		if next != effort.Value {
			if err := db.Model(&model.Setting{}).Where(model.SettingKeyIs(model.SettingKeyClaudeCLIReasoningEffort)).
				Update("value", next).Error; err != nil {
				return fmt.Errorf("update %s: %w", model.SettingKeyClaudeCLIReasoningEffort, err)
			}
			log.Infof("migration 023: %s %q -> %q (client's own thinking choice still wins; %q = never fill)",
				model.SettingKeyClaudeCLIReasoningEffort, effort.Value, next, "none")
		}
	}

	var compact model.Setting
	err = db.Where(model.SettingKeyIs(settingKeyLegacyClaudeCLIAutoCompact)).Take(&compact).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", settingKeyLegacyClaudeCLIAutoCompact, err)
	}
	if strings.EqualFold(strings.TrimSpace(compact.Value), "true") {
		log.Infof("migration 023: removed setting %s (was on). It only injected context_management clear_thinking keep=all, "+
			"which keeps everything on current Claude models and caused 400 when thinking was off; nothing replaces it",
			settingKeyLegacyClaudeCLIAutoCompact)
	}
	if err := db.Where(model.SettingKeyIs(settingKeyLegacyClaudeCLIAutoCompact)).Delete(&model.Setting{}).Error; err != nil {
		return fmt.Errorf("delete %s: %w", settingKeyLegacyClaudeCLIAutoCompact, err)
	}
	return nil
}

func migratedClaudeDefaultEffort(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case "none", "low", "medium", "high", "xhigh", "max":
		return v
	case "off", "false", "disabled":
		return "none"
	default:
		return model.ClaudeDefaultEffortDefault
	}
}
