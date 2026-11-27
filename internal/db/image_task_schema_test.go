package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// oldImageTask 是迁移 018 初次建表时的结构：还没有 next_attempt_at。
// 结构体冻结在这里而不是直接复用 model.ImageTask，否则模型再加列时这条测试
// 就不再模拟“老库”。
type oldImageTask struct {
	ID               int64   `gorm:"primaryKey;autoIncrement"`
	TaskID           string  `gorm:"size:64;uniqueIndex;default:''"`
	CreatedAt        int64   `gorm:"index"`
	CompletedAt      int64   `gorm:"default:0"`
	ExpiresAt        int64   `gorm:"index"`
	Status           string  `gorm:"size:16;index;default:'processing'"`
	OwnerTokenID     int     `gorm:"index;default:0"`
	OwnerUserID      int     `gorm:"index;default:0"`
	ChannelID        int     `gorm:"index;default:0"`
	ChannelKeyID     int     `gorm:"default:0"`
	Model            string  `gorm:"size:255;default:''"`
	Endpoint         string  `gorm:"size:32;default:''"`
	RequestBody      string  `gorm:"type:text"`
	RequestModel     string  `gorm:"size:255;default:''"`
	RequestIP        string  `gorm:"size:128;default:''"`
	RequestUserAgent string  `gorm:"size:512;default:''"`
	ResultJSON       string  `gorm:"type:text"`
	ErrorMessage     string  `gorm:"type:text"`
	Cost             float64 `gorm:"default:0"`
	BillingMode      string  `gorm:"size:16;default:''"`
	ImageCount       int     `gorm:"default:0"`
	BillingSnapshot  string  `gorm:"type:text"`
}

func (oldImageTask) TableName() string { return "image_tasks" }

// TestInitDBAlignsOldImageTaskTable 守“next_attempt_at 由每次启动的主 AutoMigrate 补齐”。
//
// 这列是执行器查询条件的一部分，不是可有可无的元数据。老库里的迁移 018 早已记为
// 成功，编号迁移不会再跑第二遍；如果 ImageTask 被意外移出主 AutoMigrate 名单，
// 线上会在 ImageTaskListExecutable 里报 no column named next_attempt_at。
func TestInitDBAlignsOldImageTaskTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-image-tasks.db")

	raw, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := raw.AutoMigrate(&oldImageTask{}); err != nil {
		t.Fatalf("create old image_tasks schema: %v", err)
	}
	if raw.Migrator().HasColumn(&oldImageTask{}, "next_attempt_at") {
		t.Fatal("fixture is wrong: the old schema already has next_attempt_at")
	}
	now := time.Now().Unix()
	if err := raw.Exec(
		"INSERT INTO image_tasks (task_id, created_at, expires_at, status, owner_token_id, owner_user_id, channel_id, model, request_body) VALUES (?,?,?,?,?,?,?,?,?)",
		"img-old", now-60, now+3600, model.ImageTaskStatusProcessing, 11, 22, 33, "image-model", `{"prompt":"old"}`,
	).Error; err != nil {
		t.Fatalf("seed an old image task: %v", err)
	}
	if err := raw.AutoMigrate(&migrate.MigrationRecord{}); err != nil {
		t.Fatalf("create migration record table: %v", err)
	}
	if err := raw.Create(&migrate.MigrationRecord{Version: 18, Status: migrate.MigrationRecordStatusSuccess}).Error; err != nil {
		t.Fatalf("mark migration 018 as applied: %v", err)
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatalf("raw db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	if err := db.InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	handle := db.GetDB()
	if handle == nil {
		t.Fatal("InitDB 之后 GetDB 为空")
	}
	t.Cleanup(func() {
		if sqlDB, err := handle.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	if !handle.Migrator().HasColumn(&model.ImageTask{}, "next_attempt_at") {
		t.Fatal("main AutoMigrate did not add image_tasks.next_attempt_at to the old table")
	}
	if !handle.Migrator().HasIndex(&model.ImageTask{}, "idx_image_tasks_next_attempt_at") {
		t.Fatal("main AutoMigrate did not add the next_attempt_at index")
	}

	var stored model.ImageTask
	if err := handle.Where("task_id = ?", "img-old").First(&stored).Error; err != nil {
		t.Fatalf("read the pre-existing image task: %v", err)
	}
	if stored.NextAttemptAt != 0 {
		t.Fatalf("pre-existing row NextAttemptAt = %d, want the default 0 (immediately schedulable)", stored.NextAttemptAt)
	}

	executable, err := op.ImageTaskListExecutable(context.Background(), now, 20)
	if err != nil {
		t.Fatalf("list executable image tasks: %v", err)
	}
	if len(executable) != 1 || executable[0].TaskID != "img-old" {
		t.Fatalf("upgraded pre-existing row is not schedulable: %+v", executable)
	}
}
