package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 5,
		Up:      migrateClaudeCodeHeaderDefaults,
	})
}

func migrateClaudeCodeHeaderDefaults(db *gorm.DB) error {
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
		model.SettingKeyClaudeHeaderUserAgent: {
			old: "claude-cli/2.1.126 (external, claude-vscode, agent-sdk/0.2.126)",
			new: "claude-cli/2.1.168 (external, sdk-cli)",
		},
		model.SettingKeyClaudeHeaderPackage: {
			old: "0.81.0",
			new: "0.94.0",
		},
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
