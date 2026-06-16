package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 3,
		Up:      migrateMultiUserOwnership,
	})
}

// 003:
// - fill role/status for the legacy single-admin user
// - attach legacy API keys and historical logs to that admin so future can scope data by user
func migrateMultiUserOwnership(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("users") {
		return nil
	}

	var adminID int
	if err := db.Raw("SELECT id FROM users ORDER BY id ASC LIMIT 1").Scan(&adminID).Error; err != nil {
		return fmt.Errorf("failed to locate default admin user: %w", err)
	}
	if adminID == 0 {
		return nil
	}

	if err := db.Exec("UPDATE users SET role = ? WHERE role IS NULL OR role = ''", "admin").Error; err != nil {
		return fmt.Errorf("failed to backfill user role: %w", err)
	}
	if err := db.Exec("UPDATE users SET status = ? WHERE status IS NULL OR status = ''", "active").Error; err != nil {
		return fmt.Errorf("failed to backfill user status: %w", err)
	}
	if err := db.Exec("UPDATE users SET role = ?, status = ? WHERE id = ?", "admin", "active", adminID).Error; err != nil {
		return fmt.Errorf("failed to normalize default admin user: %w", err)
	}

	if db.Migrator().HasTable("api_keys") && db.Migrator().HasColumn("api_keys", "user_id") {
		if err := db.Exec("UPDATE api_keys SET user_id = ? WHERE user_id IS NULL OR user_id = 0", adminID).Error; err != nil {
			return fmt.Errorf("failed to backfill api key owner: %w", err)
		}
	}

	if db.Migrator().HasTable("relay_logs") && db.Migrator().HasColumn("relay_logs", "user_id") {
		if err := db.Exec("UPDATE relay_logs SET user_id = ? WHERE user_id IS NULL OR user_id = 0", adminID).Error; err != nil {
			return fmt.Errorf("failed to backfill relay log owner: %w", err)
		}
	}

	return nil
}
