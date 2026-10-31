package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new026DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

type promptRow struct {
	Mode    string `gorm:"column:prompt_override_mode"`
	Enabled bool   `gorm:"column:prompt_override_enabled"`
}

// 旧库（没有开关列）→ AutoMigrate 加列 → 026：旧模式都变 before，有文本的开、空文本的关；
// 已经是 before/after 的行不动；跑两次结果一样，不会把手动关掉的重新打开。
func TestMigration026PromptSwitchFromLegacyRows(t *testing.T) {
	db := new026DB(t)
	// The pre-switch schema: same tables, no prompt_override_enabled column.
	for _, ddl := range []string{
		`CREATE TABLE access_plans (id integer PRIMARY KEY, slug text UNIQUE NOT NULL, display_name text NOT NULL, system_prompt_override text, prompt_override_mode text DEFAULT 'append_system')`,
		`CREATE TABLE access_route_rules (id integer PRIMARY KEY, route_profile_id integer NOT NULL, request_model text NOT NULL, system_prompt_override text, prompt_override_mode text DEFAULT 'append_system')`,
		`CREATE TABLE channels (id integer PRIMARY KEY, name text, system_prompt_override text, prompt_override_mode text DEFAULT 'append_system')`,
		`INSERT INTO access_plans VALUES (1,'a','a','plan text','append_system'), (2,'b','b','  ','append_system'), (3,'c','c','swap','replace_system'), (4,'d','d','',NULL)`,
		`INSERT INTO access_route_rules VALUES (1,1,'m1','route text','replace_system'), (2,1,'m2','','append_system')`,
		`INSERT INTO channels VALUES (1,'c1','chan text','append_system'), (2,'c2',NULL,'append_system')`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	if err := db.AutoMigrate(&model.AccessPlan{}, &model.AccessRouteRule{}, &model.Channel{}, &model.Setting{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	db.Create(&model.Setting{Key: model.SettingKeyPromptOverrideMode, Value: "replace_system"})
	db.Create(&model.Setting{Key: model.SettingKeyPromptOverrideSystem, Value: "global text"})

	if err := migratePromptOverrideSwitch(db); err != nil {
		t.Fatal(err)
	}
	// The admin turns plan 1 off afterwards; a second run must not turn it back on.
	db.Exec(`UPDATE access_plans SET prompt_override_enabled = false WHERE id = 1`)
	if err := migratePromptOverrideSwitch(db); err != nil {
		t.Fatal(err)
	}

	check := func(table string, want []promptRow) {
		t.Helper()
		var got []promptRow
		db.Table(table).Order("id").Find(&got)
		if len(got) != len(want) {
			t.Fatalf("%s: %#v", table, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s row %d: got %#v want %#v", table, i+1, got[i], want[i])
			}
		}
	}
	check("access_plans", []promptRow{{"before", false}, {"before", false}, {"before", true}, {"before", false}})
	check("access_route_rules", []promptRow{{"before", true}, {"before", false}})
	check("channels", []promptRow{{"before", true}, {"before", false}})

	var s model.Setting
	db.Where("key = ?", model.SettingKeyPromptOverrideMode).Take(&s)
	if s.Value != "before" {
		t.Fatalf("global mode %q", s.Value)
	}
	var sw model.Setting
	db.Where("key = ?", model.SettingKeyPromptOverrideEnabled).Take(&sw)
	if sw.Value != "true" {
		t.Fatalf("global switch %q", sw.Value)
	}
}

func TestMigration026LeavesNewValuesAlone(t *testing.T) {
	db := new026DB(t)
	if err := db.AutoMigrate(&model.Channel{}, &model.Setting{}); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO channels (id, name, system_prompt_override, prompt_override_mode, prompt_override_enabled) VALUES (1,'x','t','after',false)`)
	db.Create(&model.Setting{Key: model.SettingKeyPromptOverrideMode, Value: "after"})
	if err := migratePromptOverrideSwitch(db); err != nil {
		t.Fatal(err)
	}
	var got promptRow
	db.Table("channels").Take(&got)
	if got != (promptRow{"after", false}) {
		t.Fatalf("%#v", got)
	}
	var n int64
	db.Model(&model.Setting{}).Where("key = ?", model.SettingKeyPromptOverrideEnabled).Count(&n)
	if n != 0 {
		t.Fatalf("switch row must be left to the defaults loader, got %d", n)
	}
}
