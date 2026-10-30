package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new023DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.Setting{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func read023(t *testing.T, db *gorm.DB, key model.SettingKey) (string, bool) {
	t.Helper()
	var s model.Setting
	if err := db.Where("key = ?", key).Limit(1).Find(&s).Error; err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return s.Value, s.Key == key
}

func TestMigration023MapsClaudeEffortValues(t *testing.T) {
	cases := map[string]string{
		"auto": "high", "": "high", "AUTO": "high", "ultra": "high",
		"off": "none", "false": "none", "disabled": "none",
		"none": "none", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
		" Max ": "max",
	}
	for in, want := range cases {
		db := new023DB(t)
		if err := db.Create(&model.Setting{Key: model.SettingKeyClaudeCLIReasoningEffort, Value: in}).Error; err != nil {
			t.Fatal(err)
		}
		for run := 0; run < 2; run++ { // second run proves idempotency
			if err := migrateClaudeDefaultEffortSettings(db); err != nil {
				t.Fatalf("%q run %d: %v", in, run, err)
			}
			got, _ := read023(t, db, model.SettingKeyClaudeCLIReasoningEffort)
			if got != want {
				t.Fatalf("%q run %d: got %q want %q", in, run, got, want)
			}
		}
	}
}

func TestMigration023RemovesAutoCompact(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		db := new023DB(t)
		if err := db.Create(&model.Setting{Key: settingKeyLegacyClaudeCLIAutoCompact, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Setting{Key: model.SettingKeyCodexFastMode, Value: "true"}).Error; err != nil {
			t.Fatal(err)
		}
		for run := 0; run < 2; run++ {
			if err := migrateClaudeDefaultEffortSettings(db); err != nil {
				t.Fatalf("run %d: %v", run, err)
			}
		}
		if _, ok := read023(t, db, settingKeyLegacyClaudeCLIAutoCompact); ok {
			t.Fatalf("auto compact (%s) row still present", value)
		}
		if got, _ := read023(t, db, model.SettingKeyCodexFastMode); got != "true" {
			t.Fatalf("unrelated setting changed: %q", got)
		}
	}
}

func TestMigration023NoSettingsRowsIsNoop(t *testing.T) {
	db := new023DB(t)
	if err := migrateClaudeDefaultEffortSettings(db); err != nil {
		t.Fatal(err)
	}
	if _, ok := read023(t, db, model.SettingKeyClaudeCLIReasoningEffort); ok {
		t.Fatal("migration must not create rows")
	}
}
