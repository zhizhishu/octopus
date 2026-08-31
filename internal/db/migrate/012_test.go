package migrate

import (
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new012RawTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, dberr := gdb.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return gdb
}

// TestMigrateAddChannelRaceModeFields_LegacyTable tests migration 012 on an existing
// channels table that lacks the race_* columns:
// 1. Executing 012 adds race_mode, race_key_concurrency, and race_delay_ms.
// 2. Re-executing 012 is idempotent and returns nil without error.
// 3. Existing channel rows retrieve default values: race_mode=false, race_key_concurrency=2, race_delay_ms=0.
func TestMigrateAddChannelRaceModeFields_LegacyTable(t *testing.T) {
	gdb := new012RawTestDB(t)

	// Create legacy channels table missing the three race_* columns.
	if err := gdb.Exec(`CREATE TABLE channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE
	)`).Error; err != nil {
		t.Fatalf("create legacy channels table: %v", err)
	}

	if err := gdb.Exec(`INSERT INTO channels (id, name) VALUES (1, 'legacy-channel')`).Error; err != nil {
		t.Fatalf("seed legacy channel: %v", err)
	}

	// Verify columns are absent before migration.
	for _, col := range []string{"RaceMode", "RaceKeyConcurrency", "RaceDelayMs"} {
		if gdb.Migrator().HasColumn(&model.Channel{}, col) {
			t.Fatalf("precondition failed: column %s already exists", col)
		}
	}

	// 1. Run migration 012 for the first time.
	if err := migrateAddChannelRaceModeFields(gdb); err != nil {
		t.Fatalf("first migration run: %v", err)
	}

	// Verify the 3 columns exist.
	for _, col := range []string{"RaceMode", "RaceKeyConcurrency", "RaceDelayMs"} {
		if !gdb.Migrator().HasColumn(&model.Channel{}, col) {
			t.Fatalf("column %s missing after migration", col)
		}
	}

	// 2. Run migration 012 a second time to verify idempotency.
	if err := migrateAddChannelRaceModeFields(gdb); err != nil {
		t.Fatalf("second migration run (idempotency): %v", err)
	}

	// 3. Verify existing channel row reads back expected defaults.
	var ch model.Channel
	if err := gdb.Where("id = ?", 1).First(&ch).Error; err != nil {
		t.Fatalf("load channel after migration: %v", err)
	}
	if ch.RaceMode != false {
		t.Fatalf("ch.RaceMode = %v, want false", ch.RaceMode)
	}
	if ch.RaceKeyConcurrency != 2 {
		t.Fatalf("ch.RaceKeyConcurrency = %d, want 2", ch.RaceKeyConcurrency)
	}
	if ch.RaceDelayMs != 0 {
		t.Fatalf("ch.RaceDelayMs = %d, want 0", ch.RaceDelayMs)
	}
}

// TestMigrateAddChannelRaceModeFields_TableAbsent tests that 012 directly returns nil
// without error when the channels table does not exist.
func TestMigrateAddChannelRaceModeFields_TableAbsent(t *testing.T) {
	gdb := new012RawTestDB(t)

	if gdb.Migrator().HasTable(&model.Channel{}) {
		t.Fatalf("precondition failed: channels table should not exist")
	}

	if err := migrateAddChannelRaceModeFields(gdb); err != nil {
		t.Fatalf("migrateAddChannelRaceModeFields on missing table returned error: %v", err)
	}
}
