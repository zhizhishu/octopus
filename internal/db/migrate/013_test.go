package migrate

import (
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new013RawTestDB(t *testing.T) *gorm.DB {
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

// legacyAccessRouteRule is a struct that mirrors the access_route_rules table
// WITHOUT the priority_overridden column — exactly what existed pre-migration.
type legacyAccessRouteRule struct {
	ID             int    `gorm:"primaryKey"`
	RouteProfileID int    `gorm:"not null;index"`
	RequestModel   string `gorm:"not null"`
}

func (legacyAccessRouteRule) TableName() string {
	return "access_route_rules"
}

// TestMigrateBackfillAccessRouteRulePriorityOverridden_LegacyTable tests
// migration 013 on an existing access_route_rules table that lacks the
// priority_overridden column:
//  1. Executing 013 adds the column and backfills existing rows to true.
//  2. Re-executing 013 is idempotent and returns nil without error.
func TestMigrateBackfillAccessRouteRulePriorityOverridden_LegacyTable(t *testing.T) {
	gdb := new013RawTestDB(t)

	// Create legacy access_route_rules table without priority_overridden.
	if err := gdb.AutoMigrate(&legacyAccessRouteRule{}); err != nil {
		t.Fatalf("auto-migrate legacy access_route_rules: %v", err)
	}

	// Verify the column is absent pre-migration.
	if gdb.Migrator().HasColumn(&model.AccessRouteRule{}, "PriorityOverridden") {
		t.Fatalf("precondition failed: priority_overridden column already exists")
	}

	// Insert two historical rows.
	for _, rule := range []legacyAccessRouteRule{
		{ID: 1, RouteProfileID: 10, RequestModel: "gpt-4"},
		{ID: 2, RouteProfileID: 20, RequestModel: "gpt-3.5-turbo"},
	} {
		if err := gdb.Create(&rule).Error; err != nil {
			t.Fatalf("seed legacy rule id=%d: %v", rule.ID, err)
		}
	}

	// 1. Run migration 013 for the first time.
	if err := migrateBackfillAccessRouteRulePriorityOverridden(gdb); err != nil {
		t.Fatalf("first migration run: %v", err)
	}

	// 2. Verify the column now exists.
	if !gdb.Migrator().HasColumn(&model.AccessRouteRule{}, "PriorityOverridden") {
		t.Fatalf("priority_overridden column missing after migration")
	}

	// 3. Verify both historical rows have priority_overridden = true.
	var countTrue int64
	if err := gdb.Model(&model.AccessRouteRule{}).
		Where("priority_overridden = ?", true).
		Count(&countTrue).Error; err != nil {
		t.Fatalf("count true rows: %v", err)
	}
	if countTrue != 2 {
		t.Fatalf("got %d rows with priority_overridden=true, want 2", countTrue)
	}

	// 4. Run migration 013 a second time to verify idempotency.
	if err := migrateBackfillAccessRouteRulePriorityOverridden(gdb); err != nil {
		t.Fatalf("second migration run (idempotency): %v", err)
	}

	// 5. After re-run, column still exists and the count is unchanged.
	if !gdb.Migrator().HasColumn(&model.AccessRouteRule{}, "PriorityOverridden") {
		t.Fatalf("priority_overridden column disappeared after second run")
	}
	var countTrueAfter int64
	if err := gdb.Model(&model.AccessRouteRule{}).
		Where("priority_overridden = ?", true).
		Count(&countTrueAfter).Error; err != nil {
		t.Fatalf("count true rows after second run: %v", err)
	}
	if countTrueAfter != 2 {
		t.Fatalf("after second run got %d rows with priority_overridden=true, want 2", countTrueAfter)
	}
}

// TestMigrateBackfillAccessRouteRulePriorityOverridden_TableAbsent tests that
// 013 directly returns nil without error when the access_route_rules table does
// not exist.
func TestMigrateBackfillAccessRouteRulePriorityOverridden_TableAbsent(t *testing.T) {
	gdb := new013RawTestDB(t)

	if gdb.Migrator().HasTable(&model.AccessRouteRule{}) {
		t.Fatalf("precondition failed: access_route_rules table should not exist")
	}

	if err := migrateBackfillAccessRouteRulePriorityOverridden(gdb); err != nil {
		t.Fatalf("migrateBackfill... on missing table returned error: %v", err)
	}
}