package migrate

import (
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	// 不需要 AlwaysRun：本迁移只建表，没有回填，也没有"这一遍跑不完下次继续"的语义
	// （对比 014 的回填——那里才是 AlwaysRun 的唯一理由）。建完即完成。
	RegisterAfterAutoMigration(Migration{
		Version: 18,
		Up:      migrateCreateImageTasks,
	})
}

// 018: 建图片异步任务表 image_tasks。
//
// 背景：/v1/images/generations 原本是同步的——客户端发请求，我们等上游画完再一次性
// 返回。图片生成动辄 20–60 秒，这段时间客户端连接一直挂着、我们一个 goroutine 一直
// 占着；断了之后用户不知道图到底画成没有。异步版立刻返回 202 + 任务号，把"这次生成"
// 变成可持久化、可续查的一行状态，必须比发起它的那次 HTTP 请求、比进程重启活得更久。
//
// 安全性：
//   - 只增不改：新建独立表，不触碰任何既有表/列，失败不影响其它功能。
//   - 幂等：AutoMigrate 建表本身幂等，重复跑无副作用；已存在的行不受影响。
//   - 无回填：存量没有异步图片任务可搬，刻意不做任何数据搬迁。
//
// 注：本表刻意不存渠道密钥，只存 ChannelKeyID 引用（详见 model.ImageTask）。
func migrateCreateImageTasks(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(&model.ImageTask{}); err != nil {
		return fmt.Errorf("create image_tasks: %w", err)
	}
	return nil
}
