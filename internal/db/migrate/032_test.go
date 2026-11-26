package migrate

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 注意：本测试为静态编写，未在 Windows 本机运行（环境约束禁止 go test）。

func new032DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus032.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.User{}, &model.APIKey{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func systemKeys032(t *testing.T, db *gorm.DB) []model.APIKey {
	t.Helper()
	keys := make([]model.APIKey, 0)
	if err := db.Where("system = ?", true).Find(&keys).Error; err != nil {
		t.Fatalf("read system keys: %v", err)
	}
	return keys
}

// 已有 admin 的库（升级场景）：迁移建一把系统 Key，归属 role=admin 且 id 最小的用户；
// 密钥形状与 server/auth.GenerateAPIKey 一致；跑两遍不重复建行。
func TestMigration032SeedsSystemKeyForExistingAdmin(t *testing.T) {
	db := new032DB(t)
	users := []model.User{
		{Username: "first", Role: model.UserRoleUser, Status: model.UserStatusActive},
		{Username: "admin2", Role: model.UserRoleAdmin, Status: model.UserStatusActive},
		{Username: "root", Role: model.UserRoleAdmin, Status: model.UserStatusActive},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	// admin2 是 id 最小的 admin（first 不是 admin），归属必须是它。
	if err := migrateSystemAPIKey(db); err != nil {
		t.Fatalf("migrate 032: %v", err)
	}
	keys := systemKeys032(t, db)
	if len(keys) != 1 {
		t.Fatalf("system key count = %d, want 1: %#v", len(keys), keys)
	}
	key := keys[0]
	if key.Name != SystemAPIKeyName {
		t.Fatalf("system key name = %q, want %q", key.Name, SystemAPIKeyName)
	}
	if key.UserID != users[1].ID {
		t.Fatalf("system key owner = %d, want first admin id %d", key.UserID, users[1].ID)
	}
	if !key.Enabled || !key.System {
		t.Fatalf("system key must be enabled+system: %#v", key)
	}
	wantPrefix := "sk-" + conf.APP_NAME + "-"
	if !strings.HasPrefix(key.APIKey, wantPrefix) {
		t.Fatalf("system key secret prefix = %q, want prefix %q", key.APIKey, wantPrefix)
	}
	if got := len(strings.TrimPrefix(key.APIKey, wantPrefix)); got != 48 {
		t.Fatalf("system key secret random part len = %d, want 48", got)
	}

	// 第二轮直跑：不再建行（认 system 标记，不认名字）。
	if err := migrateSystemAPIKey(db); err != nil {
		t.Fatalf("migrate 032 again: %v", err)
	}
	if keys := systemKeys032(t, db); len(keys) != 1 {
		t.Fatalf("second run created another key: %#v", keys)
	}
	var rec MigrationRecord
	if err := db.Where("version = ?", 32).Take(&rec).Error; err != nil || rec.Status != MigrationRecordStatusSuccess {
		t.Fatalf("record 32: %#v %v", rec, err)
	}
}

// 全新装库（还没有任何用户）：迁移仍然建行，user_id=0 挂起归属——
// op.UserInit 的 backfillLegacyOwnership 会把 user_id=0 的 Key 划给后来建的 admin，
// 这里模拟同一件事验证整条链能闭环。
func TestMigration032FreshInstallPendingOwnership(t *testing.T) {
	db := new032DB(t)
	if err := migrateSystemAPIKey(db); err != nil {
		t.Fatalf("migrate 032: %v", err)
	}
	keys := systemKeys032(t, db)
	if len(keys) != 1 {
		t.Fatalf("system key count = %d, want 1 even without any user", len(keys))
	}
	if keys[0].UserID != 0 {
		t.Fatalf("fresh install key must start with pending owner 0, got %d", keys[0].UserID)
	}
	// 模拟 op.UserInit：建 admin，再把 user_id=0 的 Key 划给它（backfillLegacyOwnership 同口径）。
	admin := model.User{Username: "admin", Role: model.UserRoleAdmin, Status: model.UserStatusActive}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.APIKey{}).Where("user_id = 0 OR user_id IS NULL").Update("user_id", admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	keys = systemKeys032(t, db)
	if len(keys) != 1 || keys[0].UserID != admin.ID {
		t.Fatalf("ownership backfill must assign the system key to the admin: %#v", keys)
	}
}

// 删行防复活：直接把系统 Key 行删掉再重跑 Up，不得重建（与 031 同语义——
// 记录已 success 就不再插手；op 层本来也不允许删除系统 Key，这只是兜直库场景）。
func TestMigration032DeletedKeyStaysDeleted(t *testing.T) {
	db := new032DB(t)
	admin := model.User{Username: "admin", Role: model.UserRoleAdmin, Status: model.UserStatusActive}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateSystemAPIKey(db); err != nil {
		t.Fatal(err)
	}
	if n := len(systemKeys032(t, db)); n != 1 {
		t.Fatalf("want 1 system key, got %d", n)
	}
	if err := db.Where("system = ?", true).Delete(&model.APIKey{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateSystemAPIKey(db); err != nil {
		t.Fatal(err)
	}
	if n := len(systemKeys032(t, db)); n != 0 {
		t.Fatalf("deleted system key must stay deleted after rerun, got %d", n)
	}
}

// 方言覆盖：032 全部走 ORM 读写，不得出现方言 SQL 字面量（沿用 031 的源码断言风格）。
func TestMigration032NoDialectSQLLiterals(t *testing.T) {
	file := parseMigrationSource(t, "032.go")
	for _, sql := range execStringLiterals(file) {
		for _, kw := range []string{"CREATE TABLE", "CREATE INDEX", "INSERT INTO", "AUTO_INCREMENT", "MODIFY COLUMN"} {
			if contains(sql, kw) {
				t.Fatalf("032.go 含方言 SQL 字面量 %q：%s", kw, sql)
			}
		}
	}
}
