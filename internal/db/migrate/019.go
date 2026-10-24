package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	// 不需要 AlwaysRun：本迁移只补列，没有回填，也没有"这一遍跑不完下次继续"的语义
	// （对比 014 的回填——那里才是 AlwaysRun 的唯一理由）。加完列即完成。
	RegisterAfterAutoMigration(Migration{
		Version: 19,
		Up:      migrateAddChannelPathOverrideColumns,
	})
}

// 019: 给 channels 表补三列通用路径逃生口 —— open_ai_images_path /
// open_ai_videos_path / open_ai_tasks_path。
//
// 背景：渠道早就有 open_ai_chat_path / open_ai_models_path / open_ai_embeddings_path
// 三个逃生口（因为上游的真实路径不一定是 /v1/…）。图片与视频的上游路径此前是
// 模板硬拼的，渠道级改不了，于是"别人给一个上游地址 + 一串密钥"接不上那些把图片/
// 视频挂在别的路径上的上游。这三列把同一套先例补齐。
//
// 刻意不回填：空值 = 存量渠道 = 沿用 /v1/images、/v1/videos、与创建同前缀的查询
// 路径，行为与改动前逐字节一致。回填反而会把某个猜测写死进库里。
//
// 幂等：AutoMigrate 加列本身幂等，重复跑无副作用。新库由 db.go 的 AutoMigrate
// 按 struct tag 直接建出这三列，这一条迁移是给已存在的库补列的方言无关保险。
func migrateAddChannelPathOverrideColumns(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		return fmt.Errorf("add channels path override columns: %w", err)
	}
	return nil
}
