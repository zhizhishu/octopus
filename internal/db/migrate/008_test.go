package migrate

import (
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// legacyGroup maps to the same "groups" table as model.Group but WITHOUT the unique
// constraint on name, reproducing a pre-"gorm:unique" table that AutoMigrate never
// retrofitted — the exact state migration 008 exists to repair. Creating the table via
// this struct lets the test insert duplicate names (impossible once the index exists).
type legacyGroup struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"column:name"`
}

func (legacyGroup) TableName() string { return "groups" }

func newGroupIndexTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Close the sqlite handle before t.TempDir()'s cleanup unlinks the file (Windows won't
	// remove a file still held open).
	if sqlDB, dberr := gdb.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := gdb.AutoMigrate(&legacyGroup{}); err != nil {
		t.Fatalf("automigrate legacy groups: %v", err)
	}
	return gdb
}

// TestMigrateAddUniqueGroupNameIndex exercises the full lifecycle 008 must handle now that
// it is AlwaysRun: skip (no error) while duplicates exist so startup isn't blocked, build
// the index once an operator clears the duplicate on a later run, stay idempotent, and
// actually enforce uniqueness afterwards.
func TestMigrateAddUniqueGroupNameIndex(t *testing.T) {
	gdb := newGroupIndexTestDB(t)

	// Two duplicate-named groups (only possible because the legacy table has no unique
	// constraint). 008 must SKIP creating the index and NOT error, so boot isn't blocked.
	if err := gdb.Create(&legacyGroup{Name: "dup"}).Error; err != nil {
		t.Fatalf("seed group 1: %v", err)
	}
	if err := gdb.Create(&legacyGroup{Name: "dup"}).Error; err != nil {
		t.Fatalf("seed group 2: %v", err)
	}

	if err := migrateAddUniqueGroupNameIndex(gdb); err != nil {
		t.Fatalf("run with duplicates: %v", err)
	}
	if gdb.Migrator().HasIndex(&model.Group{}, "uniq_groups_name") {
		t.Fatalf("index created despite duplicate names")
	}

	// Operator merges the duplicate; the AlwaysRun re-run now builds the index.
	var one legacyGroup
	if err := gdb.Where("name = ?", "dup").First(&one).Error; err != nil {
		t.Fatalf("load a duplicate to delete: %v", err)
	}
	if err := gdb.Delete(&legacyGroup{}, one.ID).Error; err != nil {
		t.Fatalf("delete duplicate: %v", err)
	}
	if err := migrateAddUniqueGroupNameIndex(gdb); err != nil {
		t.Fatalf("run after clearing duplicate: %v", err)
	}
	if !gdb.Migrator().HasIndex(&model.Group{}, "uniq_groups_name") {
		t.Fatalf("index not created after duplicate cleared")
	}

	// Idempotent: a further re-run is a clean no-op (guarded by HasIndex).
	if err := migrateAddUniqueGroupNameIndex(gdb); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}

	// The index now enforces uniqueness: a second same-name insert must be rejected.
	if err := gdb.Create(&legacyGroup{Name: "unique-name"}).Error; err != nil {
		t.Fatalf("seed unique group: %v", err)
	}
	if err := gdb.Create(&legacyGroup{Name: "unique-name"}).Error; err == nil {
		t.Fatalf("expected the unique index to reject a duplicate-name insert, got nil error")
	}
}
