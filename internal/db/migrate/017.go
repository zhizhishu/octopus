package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 17,
		Up:      migrateCreateVideoTasks,
	})
}

// 017: 建视频任务表 video_tasks。
//
// 背景：视频生成是异步的——提交后要等几分钟到几十分钟，结果必须比发起它的
// 那次 HTTP 请求、比进程重启活得更久。此前这类任务无处安放，只能同步等，
// 一旦客户端断开或进程重启，已经花掉的生成额度就白花了。这张表把任务本身
// 变成可持久化、可续轮询的一行状态。
//
// 安全性：
//   - 只增不改：新建独立表，不触碰任何既有表/列，失败不影响其它功能。
//   - 幂等：AutoMigrate 建表本身幂等，重复跑无副作用；已存在的行不受影响。
//     新库由 db.go 的 AutoMigrate 按 struct tag 直接建表，这一条迁移是给已存在
//     的库补表的方言无关保险。
//   - 无回填：存量没有视频任务可搬，刻意不做任何数据搬迁。
//
// 注：本表刻意不存渠道密钥，只存 ChannelKeyID 引用（详见 model.VideoTask）。
func migrateCreateVideoTasks(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(&model.VideoTask{}); err != nil {
		return fmt.Errorf("create video_tasks: %w", err)
	}
	return nil
}
