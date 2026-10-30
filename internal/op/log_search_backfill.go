package op

import (
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// relayLogSearchBackfillOnce 保证后台回填只启动一次。测试不调用
// RelayLogSearchBackfillStart，所以测试二进制里没有这条写库循环
// （和 relay log flusher / severity 回填同一条理由：没人管的后台写会撞临时库）。
var relayLogSearchBackfillOnce sync.Once

// relayLogSearchBackgroundBatches 是后台每一轮调用最多走几批（每批 2000 行）。
// 一轮之后让出一小会儿，落库和页面查询才不会被回填独占。
const relayLogSearchBackgroundBatches = 1

// RelayLogSearchBackfillStart 在服务起来之后继续把搜索投影表补完。
//
// 启动迁移（migrate 014）只在 3 秒预算内补一段，避免 18GB 的库把进程挡在监听之前。
// 这里从同一个游标接着走，补完就退出；没补完的部分下次启动还会接着做。
// 补完之前搜索走"投影表 + 游标之后主表 LIKE"的组合（见 relayLogSearchIndexState），
// 结果完整，只是没命中时会慢一些。
func RelayLogSearchBackfillStart() {
	relayLogSearchBackfillOnce.Do(func() {
		safe.SafeGo("relay-log-search-backfill", relayLogSearchBackfillLoop)
	})
}

func relayLogSearchBackfillLoop() {
	passes := 0
	failures := 0
	for {
		conn := db.GetDB()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}
		passes++
		complete, err := migrate.BackfillRelayLogSearchIndex(conn, migrate.RelayLogSearchBackfillOptions{
			MaxBatches: relayLogSearchBackgroundBatches,
		})
		if err != nil {
			failures++
			log.Warnf("relay log search backfill: %v", err)
			// 连续失败（列被改、库坏）不要每 5 秒刷一条日志刷到天荒地老：退避到 5 分钟封顶。
			backoff := time.Duration(failures) * 5 * time.Second
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
			time.Sleep(backoff)
			continue
		}
		failures = 0
		if complete {
			if passes > 1 {
				log.Infof("relay log search backfill complete after %d batches", passes-1)
			}
			return
		}
		if passes == 1 || passes%50 == 0 {
			_, cursor := migrate.RelayLogSearchBackfillProgress(conn)
			log.Infof("relay log search backfill: %d batches so far, cursor id %d", passes, cursor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
