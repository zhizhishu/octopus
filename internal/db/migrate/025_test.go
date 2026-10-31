package migrate

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new025DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.RelayLog{}, &model.UserAccessSeen{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// 回填只读 relay_logs 的入站 IP / UA：按 (用户, key, IP, UA) 聚合，次数 / 首次 / 最近都对；
// 没有用户、没有 IP、超过回填天数的行不进来；再跑一次不重复。
func TestMigration025BackfillsUserAccessFromLogs(t *testing.T) {
	db := new025DB(t)
	now := time.Now().Unix()
	old := time.Now().AddDate(0, 0, -userAccessBackfillDays-2).Unix()
	logs := []model.RelayLog{
		{ID: 1, UserID: 7, APIKeyID: 3, RequestIP: "203.0.113.5", RequestUserAgent: "claude-cli/2.0.1", Time: now - 600},
		{ID: 2, UserID: 7, APIKeyID: 3, RequestIP: "203.0.113.5", RequestUserAgent: "claude-cli/2.0.1", Time: now - 60},
		{ID: 3, UserID: 7, APIKeyID: 3, RequestIP: "198.51.100.9", RequestUserAgent: "codex_cli_rs/0.50.0", Time: now - 30},
		{ID: 4, UserID: 0, APIKeyID: 3, RequestIP: "192.0.2.1", RequestUserAgent: "curl/8", Time: now},
		{ID: 5, UserID: 7, APIKeyID: 3, RequestIP: "", RequestUserAgent: "curl/8", Time: now},
		{ID: 6, UserID: 7, APIKeyID: 3, RequestIP: "192.0.2.99", RequestUserAgent: "curl/8", Time: old},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateBackfillUserAccessSeen(db); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	var rows []model.UserAccessSeen
	if err := db.Order("ip").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 combos, got %d: %#v", len(rows), rows)
	}
	cli := rows[1]
	if cli.IP != "203.0.113.5" || cli.UserID != 7 || cli.APIKeyID != 3 || cli.UserAgent != "claude-cli/2.0.1" ||
		cli.Requests != 2 || cli.FirstSeen != now-600 || cli.LastSeen != now-60 ||
		cli.SeenKey != model.UserAccessSeenKey(7, 3, "203.0.113.5", "claude-cli/2.0.1") {
		t.Fatalf("bad aggregated row %#v", cli)
	}
	if rows[0].IP != "198.51.100.9" || rows[0].Requests != 1 {
		t.Fatalf("bad second row %#v", rows[0])
	}
}
