package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 7,
		Up:      migrateAddUniqueEmailIndex,
	})
}

// migrateAddUniqueEmailIndex enforces that a non-empty email is unique across
// users at the database level, complementing the existing application-level
// dedup check.
//
// Existing data must not break: many users (including the default admin) and
// registrations made while email verification is OFF carry an empty email
// (empty string). A naive full unique index would collide on those rows, so a
// partial / filtered unique index restricted to non-empty emails is used, which
// SQLite and PostgreSQL support. MySQL does not support filtered indexes, so it
// is skipped and uniqueness is left to the application-level dedup.
func migrateAddUniqueEmailIndex(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	// Nothing to enforce until the users table and email column exist.
	if !db.Migrator().HasTable("users") {
		return nil
	}
	if !db.Migrator().HasColumn(&model.User{}, "email") {
		return nil
	}

	// Safety pre-check: if duplicate non-empty emails already exist, creating
	// the unique index would fail (and could lock). Skip gracefully and warn;
	// the in-code dedup check still protects new registrations.
	var duplicates int64
	if err := db.Raw(
		"SELECT COUNT(*) FROM (SELECT email FROM users WHERE email <> '' GROUP BY email HAVING COUNT(*) > 1) AS dup",
	).Scan(&duplicates).Error; err != nil {
		return fmt.Errorf("failed to check duplicate emails: %w", err)
	}
	if duplicates > 0 {
		log.Warnf("skipping unique email index: %d duplicate non-empty emails exist; resolve before enforcing uniqueness", duplicates)
		return nil
	}

	switch db.Dialector.Name() {
	case "sqlite":
		return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS uniq_users_email_nonempty ON users(email) WHERE email <> ''").Error
	case "postgres":
		return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS uniq_users_email_nonempty ON users(email) WHERE email <> ''").Error
	case "mysql":
		// MySQL cannot create a filtered (partial) index. A plain unique index
		// would fail on the multiple empty-email rows, so skip and rely on the
		// application-level dedup.
		log.Warnf("mysql: filtered unique email index not supported; relying on application-level dedup")
		return nil
	default:
		log.Warnf("unsupported dialect %q: skipping unique email index", db.Dialector.Name())
		return nil
	}
}
