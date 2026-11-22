package migrate

import (
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{Version: 28, Up: migrateMCPRequestSource})
}

func migrateMCPRequestSource(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.RelayLog{}) {
		return nil
	}
	for _, field := range []string{"RequestSource", "ImageCount"} {
		if !db.Migrator().HasColumn(&model.RelayLog{}, field) {
			if err := db.Migrator().AddColumn(&model.RelayLog{}, field); err != nil {
				return err
			}
		}
	}
	if !db.Migrator().HasIndex(&model.RelayLog{}, "idx_relay_logs_source_time") {
		return db.Migrator().CreateIndex(&model.RelayLog{}, "idx_relay_logs_source_time")
	}
	return nil
}
