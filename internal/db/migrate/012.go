package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 12,
		Up:      migrateNormalizeLegacyGroupModes,
	})
}

// 012: normalize any stored legacy group.mode (random=2 / weighted=4 / smart=5) to
// spread (round-robin=1).
//
// Why: those three scheduling modes were retired. The UI already exposes only
// Spread/FillFirst, and GetBalancer folds any non-failover value into the
// capacity-aware Spread strategy — so the deprecated enum constants (plus their
// frontend labels and the modeltest smart-only enrich branch) are now removed. This
// migration rewrites any group still holding a legacy value so the DB only ever
// carries the two canonical modes {spread=1, failover/fillfirst=3}, matching the
// runtime fold and letting the frontend drop its "folded mode" disclosure.
//
// Safety / correctness:
//   - Behaviour-preserving: 2/4/5 already ran as Spread at runtime (GetBalancer's
//     default branch), so rewriting them to 1 changes the stored value only, never
//     routing.
//   - Idempotent: after the first run no row matches `mode NOT IN (1,3)`, so re-runs
//     (and startup replays) are no-ops.
//   - Scoped: failover(3) and canonical spread(1) are left untouched; the WHERE
//     clause guarantees the UPDATE only ever touches legacy rows.
func migrateNormalizeLegacyGroupModes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// Nothing to normalize until the groups table exists.
	if !db.Migrator().HasTable(&model.Group{}) {
		return nil
	}
	result := db.Model(&model.Group{}).
		Where("mode NOT IN ?", []model.GroupMode{model.GroupModeSpread, model.GroupModeFillFirst}).
		Update("mode", model.GroupModeSpread)
	if result.Error != nil {
		return fmt.Errorf("failed to normalize legacy group modes: %w", result.Error)
	}
	return nil
}
