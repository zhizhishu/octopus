package op

import (
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// relayLogSeverityBackfillOnce 保证后台回填只启动一次。测试不调用
// RelayLogSeverityBackfillStart，所以测试二进制里没有这条写库循环
// （和 relay log flusher 同一条理由：没人管的后台写会撞临时库）。
var relayLogSeverityBackfillOnce sync.Once

// RelayLogSeverityBackfillStart 在服务起来之后继续把 severity=0 的旧行补完。
//
// 启动迁移只做一小段（migrate.relayLogSeverityStartupBudget），避免 18GB 的库
// 把进程挡在监听之前。这里一批 2000 行，批与批之间让出写锁，让落库和页面查询
// 还能进得去。补完就退出；没补完的部分下次启动还会接着做。
func RelayLogSeverityBackfillStart() {
	relayLogSeverityBackfillOnce.Do(func() {
		safe.SafeGo("relay-log-severity-backfill", relayLogSeverityBackfillLoop)
	})
}

func relayLogSeverityBackfillLoop() {
	passes := 0
	for {
		conn := db.GetDB()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}
		passes++
		complete, err := migrate.BackfillRelayLogSeverity(conn, migrate.RelayLogSeverityBackfillOptions{
			MaxRows: migrateRelayLogSeverityBackgroundBatch,
		})
		if err != nil {
			log.Warnf("relay log severity backfill: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if complete {
			if passes > 1 {
				log.Infof("relay log severity backfill complete after %d batches", passes-1)
			}
			return
		}
		if passes == 1 || passes%25 == 0 {
			log.Infof("relay log severity backfill: %d batches of up to %d rows so far", passes, migrateRelayLogSeverityBackgroundBatch)
		}
		// 一批是几千行的整行重写。让出一小会儿，落库和计数才不会被回填独占写锁。
		time.Sleep(20 * time.Millisecond)
	}
}

// 与迁移里的批大小相同。放在 op 而不是去引用 migrate 的未导出常量。
const migrateRelayLogSeverityBackgroundBatch = 2000
