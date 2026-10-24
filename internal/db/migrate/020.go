package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	// 不需要 AlwaysRun：本迁移只建表/补列，没有回填，也没有"这一遍跑不完下次继续"的语义。
	RegisterAfterAutoMigration(Migration{
		Version: 20,
		Up:      migrateAddBatchImage,
	})
}

// 020: 加离线批量生图（/v1/images/batches*）的两张表，以及 channels 上的
// batch_image 配置列。
//
// 背景：批处理是**一种新流量**，既不是聊天也不是"一次生成"，无法复用 image_tasks
// （理由见 internal/model/batch_image.go 顶部）。两张表分别是一批任务与批内子项。
//
// 幂等：AutoMigrate 建表/加列本身幂等，重复跑无副作用。新库由 db.go 的 AutoMigrate
// 按 struct tag 直接建出来，这一条迁移是给已存在的库补表的方言无关保险
// （与 019 同一套先例）。
//
// 刻意不回填：channels.batch_image 为 NULL 表示"这个渠道不承接批处理"，
// 与改动前行为完全一致。回填任何默认值都等于替管理员决定让哪个渠道开始接新流量。
func migrateAddBatchImage(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(
		&model.BatchImageJob{},
		&model.BatchImageItem{},
	); err != nil {
		return fmt.Errorf("add batch image tables: %w", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		return fmt.Errorf("add channels batch_image column: %w", err)
	}
	// 幂等键的唯一性必须由数据库裁决（并发"先查再插"必然漏）。
	// 用**部分索引**只约束非空 key：空 key 语义是"每次都是新批次"，
	// 若用普通唯一索引，所有未传 key 的行会互相撞（它们都是 ''）。
	//
	// IF NOT EXISTS 让重复执行无副作用；SQLite / PostgreSQL / MySQL 8 都支持
	// 语法子集（MySQL 不支持 partial index，故退化成普通唯一索引会误伤空串，
	// 那里改用生成列太重——所以失败只告警，不阻断启动：业务侧仍有回读兜底）。
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_batch_image_idem_unique
		ON batch_image_jobs (owner_user_id, owner_token_id, idempotency_key)
		WHERE idempotency_key <> ''`).Error; err != nil {
		log.Warnf("migrate 020: partial unique index for batch image idempotency not created (dialect may not support it): %v", err)
	}
	return nil
}
