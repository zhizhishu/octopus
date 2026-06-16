package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 6,
		Up:      migrateFollowClientCLIDefaults,
	})
}

// migrateFollowClientCLIDefaults moves existing installs off the old forced
// Claude/Codex CLI behaviour onto the follow-client defaults:
//   - reasoning effort high -> auto (follow the client's thinking/effort)
//   - auto-compact true -> false (pass through the client's native context_management)
//   - codex fast true -> false (follow the client's text/reasoning)
//
// Only the old forced values are rewritten, so any intentional non-default
// choice (e.g. effort=medium, fast explicitly enabled is left as-is only when
// it was the forced default) is preserved.
func migrateFollowClientCLIDefaults(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("settings") {
		return nil
	}
	replacements := map[model.SettingKey]struct {
		old string
		new string
	}{
		model.SettingKeyClaudeCLIReasoningEffort: {old: "high", new: "auto"},
		model.SettingKeyClaudeCLIAutoCompact:     {old: "true", new: "false"},
		model.SettingKeyCodexFastMode:            {old: "true", new: "false"},
	}
	for key, item := range replacements {
		if err := db.Model(&model.Setting{}).
			Where("key = ? AND value = ?", key, item.old).
			Update("value", item.new).Error; err != nil {
			return err
		}
	}
	return nil
}
