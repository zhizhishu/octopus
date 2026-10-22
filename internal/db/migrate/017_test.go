package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new017RawTestDB(t *testing.T) *gorm.DB {
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

// 017 建 video_tasks 表，且重复跑无副作用。
//
// 断言的是**数据库列名**：轮询/查询代码直接按列名写 WHERE，列名一旦被 GORM 的
// 命名策略改掉就会静默失效，只有按列名断言才抓得住。
func TestMigration017CreatesVideoTasks(t *testing.T) {
	gdb := new017RawTestDB(t)

	if gdb.Migrator().HasTable("video_tasks") {
		t.Fatalf("precondition failed: video_tasks already exists")
	}

	if err := migrateCreateVideoTasks(gdb); err != nil {
		t.Fatalf("017: %v", err)
	}
	if !gdb.Migrator().HasTable("video_tasks") {
		t.Fatalf("017 did not create video_tasks")
	}

	columns, err := gdb.Migrator().ColumnTypes(&model.VideoTask{})
	if err != nil {
		t.Fatalf("read video_tasks columns: %v", err)
	}
	have := make(map[string]bool, len(columns))
	for _, column := range columns {
		have[column.Name()] = true
	}
	for _, name := range []string{
		"id", "created_at", "updated_at",
		"upstream_task_id", "channel_id", "channel_key_id", "model",
		"owner_token_id", "owner_user_id", "status", "progress",
		"fail_reason", "result_url",
		"submit_time", "start_time", "finish_time", "request_data",
	} {
		if !have[name] {
			t.Fatalf("017 did not create video_tasks.%s (columns: %v)", name, have)
		}
	}

	// 写入一行再重跑：幂等意味着既不能报错，也不能丢/改已有数据。
	seed := model.VideoTask{
		UpstreamTaskID: "cgt-017",
		ChannelID:      7,
		ChannelKeyID:   11,
		Model:          "seedance-1-0-lite-t2v",
		Status:         model.VideoTaskNotStart,
	}
	if err := gdb.Create(&seed).Error; err != nil {
		t.Fatalf("seed video task: %v", err)
	}

	if err := migrateCreateVideoTasks(gdb); err != nil {
		t.Fatalf("017 second run: %v", err)
	}

	var stored model.VideoTask
	if err := gdb.Where("id = ?", seed.ID).First(&stored).Error; err != nil {
		t.Fatalf("read seeded video task after re-run: %v", err)
	}
	if stored.UpstreamTaskID != "cgt-017" || stored.Status != model.VideoTaskNotStart {
		t.Fatalf("second run altered the seeded row: %+v", stored)
	}
	if stored.ChannelKeyID != 11 {
		t.Fatalf("channel_key_id = %d, want 11", stored.ChannelKeyID)
	}
}

func TestMigration017NilDB(t *testing.T) {
	if err := migrateCreateVideoTasks(nil); err == nil {
		t.Fatalf("017 must reject a nil db")
	}
}
