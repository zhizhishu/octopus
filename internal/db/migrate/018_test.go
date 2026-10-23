package migrate

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new018RawTestDB(t *testing.T) *gorm.DB {
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

// 018 建 image_tasks 表，且重复跑无副作用。
//
// 断言**数据库列名**：查询/执行器直接按列名写 WHERE（owner_token_id / expires_at /
// status / cost ...），列名一旦被命名策略改掉就静默失效，按列名断言才抓得住。
func TestMigration018CreatesImageTasks(t *testing.T) {
	gdb := new018RawTestDB(t)

	if gdb.Migrator().HasTable("image_tasks") {
		t.Fatalf("precondition failed: image_tasks already exists")
	}
	if err := migrateCreateImageTasks(gdb); err != nil {
		t.Fatalf("018: %v", err)
	}
	if !gdb.Migrator().HasTable("image_tasks") {
		t.Fatalf("018 did not create image_tasks")
	}

	columns, err := gdb.Migrator().ColumnTypes(&model.ImageTask{})
	if err != nil {
		t.Fatalf("read image_tasks columns: %v", err)
	}
	have := make(map[string]bool, len(columns))
	for _, column := range columns {
		have[column.Name()] = true
	}
	for _, name := range []string{
		"id", "task_id", "created_at", "completed_at", "expires_at", "status",
		"owner_token_id", "owner_user_id", "channel_id", "channel_key_id",
		"model", "endpoint", "request_body", "result_json", "error_message", "cost",
	} {
		if !have[name] {
			t.Fatalf("018 did not create image_tasks.%s (columns: %v)", name, have)
		}
	}

	// task_id 是客户端持有的句柄，重复即灾难：必须是唯一索引。
	if !gdb.Migrator().HasIndex(&model.ImageTask{}, "idx_image_tasks_task_id") {
		t.Fatalf("018 did not create a unique index on image_tasks.task_id")
	}

	seed := model.ImageTask{
		TaskID:       "imgtask_018",
		CreatedAt:    1_700_000_000,
		ExpiresAt:    1_700_086_400,
		Status:       model.ImageTaskStatusProcessing,
		OwnerTokenID: 3,
		OwnerUserID:  4,
		ChannelID:    5,
		ChannelKeyID: 6,
		Model:        "image-test-model",
		Endpoint:     model.ImageTaskEndpointGenerations,
		RequestBody:  `{"model":"image-test-model","prompt":"seed"}`,
		Cost:         0.5,
	}
	if err := gdb.Create(&seed).Error; err != nil {
		t.Fatalf("seed image task: %v", err)
	}

	// 幂等：再跑一次既不报错，也不能动已有行。
	if err := migrateCreateImageTasks(gdb); err != nil {
		t.Fatalf("018 second run: %v", err)
	}
	var stored model.ImageTask
	if err := gdb.Where("id = ?", seed.ID).First(&stored).Error; err != nil {
		t.Fatalf("read seeded image task after re-run: %v", err)
	}
	if stored.TaskID != "imgtask_018" || stored.Status != model.ImageTaskStatusProcessing || stored.Cost != 0.5 {
		t.Fatalf("second run altered the seeded row: %+v", stored)
	}

	// task_id 唯一性真生效（不是"只是有个索引"）。
	dup := model.ImageTask{TaskID: "imgtask_018", Status: model.ImageTaskStatusProcessing}
	if err := gdb.Create(&dup).Error; err == nil {
		t.Fatalf("duplicate task_id must be rejected by the unique index")
	}
}

func TestMigration018NilDB(t *testing.T) {
	if err := migrateCreateImageTasks(nil); err == nil {
		t.Fatalf("018 must reject a nil db")
	}
}
