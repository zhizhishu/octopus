package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 16,
		Up:      migrateAddChannelCapabilities,
	})
}

// 016: 给 channels 表加 capabilities 列（JSON 切片）+ open_ai_embeddings_path 列。
//
// 背景：渠道能力位把「一个上游账号能接什么流量」从渠道类型里拆出来。此前
// EmbeddingChannelTypes 只有 OpenAIEmbedding，一个既能聊天又能嵌入的上游（火山）
// 必须在系统里建两个渠道、填同一把 key，额度/限流/故障计数各算一套。
//
// 刻意不回填 capabilities：空值 = 存量渠道 = 退回按渠道类型判定，行为与改动前一致。
// 回填反而会冻结当前判定，将来改类型时行为突变。
//
// 幂等：AutoMigrate 加列本身幂等，重复跑无副作用。新库由 db.go 的 AutoMigrate
// 按 struct tag 直接建出这两列，这一条迁移是给已存在的库补列的方言无关保险。
func migrateAddChannelCapabilities(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		return fmt.Errorf("add channels.capabilities: %w", err)
	}
	return nil
}
