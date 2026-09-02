package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 13,
		Up:      migrateBackfillAccessRouteRulePriorityOverridden,
	})
}

// 013: add priority_overridden column to access_route_rules and backfill
// all existing (pre-migration) rows to true.
//
// Why: AccessRouteRule.PriorityOverridden marks a rule whose channel priorities
// are hand-tuned and must not be re-derived by the flat API. Every rule that
// existed before this column was introduced must start with true so that
// historical ordering is preserved; newly created rules use the struct default
// false.
//
// Safety / correctness:
//   - db.AutoMigrate(&model.AccessRouteRule{}) (which runs BEFORE this
//     AfterAutoMigrate pass) already adds the column for the new struct field,
//     so on the common path AddColumn is a no-op. It is kept as an explicit,
//     dialect-portable guard for environments where AutoMigrate may be
//     constrained or skipped.
//   - Idempotent: AddColumn only fires when HasColumn reports the column missing;
//     the UPDATE is unconditional on all rows (setting true on already-true rows
//     is harmless) so re-runs never error and always produce the same result.
//   - Additive only: a single boolean column backfilled to true; no data loss,
//     no schema shape change that affects queries.
func migrateBackfillAccessRouteRulePriorityOverridden(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// Nothing to alter until the access_route_rules table exists.
	if !db.Migrator().HasTable(&model.AccessRouteRule{}) {
		return nil
	}

	// Add column if not present (belt-and-suspenders guard; AutoMigrate may have
	// already done this).
	if !db.Migrator().HasColumn(&model.AccessRouteRule{}, "PriorityOverridden") {
		if err := db.Migrator().AddColumn(&model.AccessRouteRule{}, "PriorityOverridden"); err != nil {
			return fmt.Errorf("failed to add access_route_rules.priority_overridden: %w", err)
		}
	}

	// Backfill every existing row to true so pre-migration rules retain their
	// hand-tuned ordering. New rules (created after the column exists) default
	// to false via the struct tag.
	if err := db.Model(&model.AccessRouteRule{}).
		Where("1 = 1").
		UpdateColumn("priority_overridden", true).Error; err != nil {
		return fmt.Errorf("failed to backfill priority_overridden: %w", err)
	}

	return nil
}