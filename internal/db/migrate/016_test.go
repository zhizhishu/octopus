package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new016RawTestDB(t *testing.T) *gorm.DB {
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

// legacyChannelWithoutCapabilities mirrors the channels table WITHOUT the
// capabilities / open_ai_embeddings_path columns — what existed pre-016.
type legacyChannelWithoutCapabilities struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"unique;not null"`
	Type int
}

func (legacyChannelWithoutCapabilities) TableName() string {
	return "channels"
}

func channelsColumnNames(t *testing.T, gdb *gorm.DB) map[string]bool {
	t.Helper()
	columns, err := gdb.Migrator().ColumnTypes(&model.Channel{})
	if err != nil {
		t.Fatalf("read channels columns: %v", err)
	}
	names := make(map[string]bool, len(columns))
	for _, column := range columns {
		names[column.Name()] = true
	}
	return names
}

// 016 加两列：capabilities（JSON 切片）与 open_ai_embeddings_path。
//
// 断言的是**数据库列名**而不是字段名：op.ChannelUpdate 的 selectFields 直接写这两个
// 列名（"capabilities" / "open_ai_embeddings_path"），列名一旦被 GORM 的命名策略改掉，
// 更新就静默失效——只有按列名断言才抓得住。
func TestMigration016AddsChannelCapabilityColumns(t *testing.T) {
	gdb := new016RawTestDB(t)

	if err := gdb.AutoMigrate(&legacyChannelWithoutCapabilities{}); err != nil {
		t.Fatalf("auto-migrate legacy channels: %v", err)
	}
	if err := gdb.Create(&legacyChannelWithoutCapabilities{ID: 7, Name: "legacy", Type: 4}).Error; err != nil {
		t.Fatalf("seed legacy channel: %v", err)
	}
	legacy := channelsColumnNames(t, gdb)
	if legacy["capabilities"] || legacy["open_ai_embeddings_path"] {
		t.Fatalf("precondition failed: new columns already exist: %v", legacy)
	}

	if err := migrateAddChannelCapabilities(gdb); err != nil {
		t.Fatalf("016: %v", err)
	}
	after := channelsColumnNames(t, gdb)
	for _, name := range []string{"capabilities", "open_ai_embeddings_path"} {
		if !after[name] {
			t.Fatalf("016 did not add channels.%s (columns: %v)", name, after)
		}
	}

	// 存量行读出来必须是空值：空 Capabilities = 退回按渠道类型判定，行为与改动前一致。
	var stored model.Channel
	if err := gdb.Where("id = ?", 7).First(&stored).Error; err != nil {
		t.Fatalf("read legacy channel: %v", err)
	}
	if len(stored.Capabilities) != 0 {
		t.Fatalf("legacy channel capabilities = %v, want empty", stored.Capabilities)
	}
	if stored.OpenAIEmbeddingsPath != "" {
		t.Fatalf("legacy channel openai_embeddings_path = %q, want empty", stored.OpenAIEmbeddingsPath)
	}
	if stored.SupportsCapability(model.CapabilityEmbeddings) {
		t.Fatalf("a legacy volcengine channel must not gain embeddings from the migration")
	}

	// 幂等：重复跑不报错、结果不变。
	if err := migrateAddChannelCapabilities(gdb); err != nil {
		t.Fatalf("016 second run: %v", err)
	}
}

func TestMigration016NilDB(t *testing.T) {
	if err := migrateAddChannelCapabilities(nil); err == nil {
		t.Fatalf("016 must reject a nil db")
	}
}
