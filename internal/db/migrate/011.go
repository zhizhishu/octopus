package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 11,
		Up:      migrateAddRelayLogEffortUserChannelKey,
	})
}

// 011: add the reasoning_effort, user_name and channel_key_remark columns to relay_logs.
//
// Why: the admin request-log view now surfaces, per entry, the EFFECTIVE reasoning
// effort (after any gpt-5.6 auto-bump), the name of the user who made the request, and
// the remark of the channel key actually used. Previously the row only carried the
// numeric user_id / api_key_id and no effort at all.
//
// Safety / correctness:
//   - db.AutoMigrate(&model.RelayLog{}) (which runs BEFORE this AfterAutoMigrate pass)
//     already adds columns for new struct fields, so on the common path this migration is
//     a no-op. It is kept as an explicit, dialect-portable belt-and-suspenders guard so
//     the columns are guaranteed present even if AutoMigrate is ever constrained/skipped.
//   - Idempotent: each column is only added when HasColumn reports it missing, so re-runs
//     (and the AutoMigrate overlap) never attempt a duplicate ADD COLUMN.
//   - Cross-dialect: db.Migrator().AddColumn resolves the column name and SQL type from
//     the struct tag and emits the right ALTER TABLE for sqlite / postgres / mysql, just
//     like the rest of the framework.
//   - Additive only: new nullable/defaulted text columns, no data rewrite, no shape change
//     to any request/response the relay sends or returns.
func migrateAddRelayLogEffortUserChannelKey(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// Nothing to alter until the relay_logs table exists.
	if !db.Migrator().HasTable(&model.RelayLog{}) {
		return nil
	}

	// Struct field names (GORM resolves each to its db column + type from the field tag).
	columns := []string{"UserName", "ReasoningEffort", "ChannelKeyRemark"}
	for _, field := range columns {
		if db.Migrator().HasColumn(&model.RelayLog{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&model.RelayLog{}, field); err != nil {
			return fmt.Errorf("failed to add relay_logs column %s: %w", field, err)
		}
	}
	return nil
}
