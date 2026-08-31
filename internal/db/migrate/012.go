package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 12,
		Up:      migrateAddChannelRaceModeFields,
	})
}

// 012: add race_mode, race_key_concurrency, and race_delay_ms columns to channels.
//
// Why: channels can configure multi-key race mode (concurrent speculative probing),
// with concurrency limit (2..5, default 2) and hedge delay (0..5000ms, default 0).
//
// Safety / correctness:
//   - db.AutoMigrate(&model.Channel{}) (which runs BEFORE this AfterAutoMigrate pass)
//     adds columns for new struct fields on supported backends, so on the common path this
//     migration is a no-op. It is kept as an explicit, dialect-portable guard.
//   - Idempotent: each column is only added when HasColumn reports it missing.
//   - Additive only: defaulted boolean/integer columns, existing channels default race_mode=false,
//     race_key_concurrency=2, race_delay_ms=0.
func migrateAddChannelRaceModeFields(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable(&model.Channel{}) {
		return nil
	}

	columns := []string{"RaceMode", "RaceKeyConcurrency", "RaceDelayMs"}
	for _, field := range columns {
		if db.Migrator().HasColumn(&model.Channel{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&model.Channel{}, field); err != nil {
			return fmt.Errorf("failed to add channels column %s: %w", field, err)
		}
	}
	return nil
}
