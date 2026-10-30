package db_test

import (
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 新表走启动时的 AutoMigrate 名单（不是一次性迁移），老库升级同样会建出来。
func TestInitDBCreatesTokenizerBaselines(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "t.db"), false); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	handle := db.GetDB()
	t.Cleanup(func() { _ = db.Close() })
	if !handle.Migrator().HasTable(&model.TokenizerBaseline{}) {
		t.Fatal("tokenizer_baselines missing after InitDB")
	}
	a := model.TokenizerBaseline{Model: "claude-x", Counts: "{}"}
	if err := handle.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	b := model.TokenizerBaseline{Model: "claude-x", Counts: "{}"}
	if err := handle.Create(&b).Error; err == nil {
		t.Fatal("model must be unique")
	}
}
