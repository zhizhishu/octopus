package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 8,
		Up:      migrateAddUniqueGroupNameIndex,
		// Re-run on every startup: while duplicate names still exist the index can't be
		// created, so this must retry once an operator merges/renames them. A one-shot
		// record would strand it as "done" forever and never build the index. The body is
		// idempotent (dup pre-check + HasIndex guard).
		AlwaysRun: true,
	})
}

// migrateAddUniqueGroupNameIndex enforces that group (model pool) names are unique
// at the database level.
//
// Group.Name already carries a `gorm:"unique"` tag, but GORM AutoMigrate does NOT
// retrofit a unique index onto a pre-existing table/column — so any groups table
// created before the tag was added carries NO enforced uniqueness. That is exactly
// how two "claude-opus-4-8" pools were able to coexist, and why op.GroupCreate /
// groupGetOrCreateAuto (which lean on ON CONFLICT / the DB constraint for dedup)
// could silently insert duplicates.
//
// This mirrors the users.email migration (007): pre-check for duplicate names, skip
// gracefully with a warning if any still exist (the application-level dedup in
// op.GroupCreate keeps protecting new pools), otherwise create the unique index.
// Unlike email, group names are NOT NULL and non-empty, so a plain (non-filtered)
// unique index is used, which every supported dialect — including MySQL — accepts.
func migrateAddUniqueGroupNameIndex(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	// Nothing to enforce until the groups table and name column exist.
	if !db.Migrator().HasTable("groups") {
		return nil
	}
	if !db.Migrator().HasColumn(&model.Group{}, "name") {
		return nil
	}

	// Safety pre-check: creating a unique index while duplicate names exist would
	// fail (and could lock) — and a migration error here would abort DB init and
	// keep the whole server from starting. So detect duplicates the boring, bullet-
	// proof way: pluck all names (the groups table is tiny) and dedup in Go, no
	// GROUP BY/HAVING SQL to trip over across dialects. If any duplicate remains,
	// skip + warn (the app-level dedup in op.GroupCreate still guards new pools);
	// once merged/renamed, a later start creates the index.
	var names []string
	if err := db.Model(&model.Group{}).Pluck("name", &names).Error; err != nil {
		return fmt.Errorf("failed to load group names: %w", err)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, dup := seen[name]; dup {
			log.Warnf("skipping unique group name index: duplicate group name(s) exist; merge/rename them before enforcing uniqueness")
			return nil
		}
		seen[name] = struct{}{}
	}

	// Idempotent across the AlwaysRun re-executions: once the index exists this is a no-op,
	// so even MySQL's non-"IF NOT EXISTS" create is never re-issued against an existing one.
	if db.Migrator().HasIndex(&model.Group{}, "uniq_groups_name") {
		return nil
	}

	switch db.Dialector.Name() {
	case "sqlite", "postgres":
		return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_groups_name ON "groups" (name)`).Error
	case "mysql":
		// Guarded by HasIndex above, so a plain create (MySQL lacks IF NOT EXISTS) is safe.
		return db.Exec("CREATE UNIQUE INDEX uniq_groups_name ON `groups` (name)").Error
	default:
		log.Warnf("unsupported dialect %q: skipping unique group name index", db.Dialector.Name())
		return nil
	}
}
