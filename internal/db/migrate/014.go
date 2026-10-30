package migrate

import (
	"fmt"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func init() {
	// AlwaysRun 是必需的, 不是优化。
	//
	// 框架对没标 AlwaysRun 的迁移只跑一次: Up() 一返回 nil 就记 success, 之后
	// 每次启动都跳过。而本迁移的回填天生可能半途而废(行太多、库忙、进程被杀),
	// 注释里"留待下次启动继续"的承诺只有靠每次重跑才成立。标上它之后:
	//   - 已补齐 -> 只核对一下投影表没被清空、再把最近一小段尾巴对一遍, 立即返回。
	//   - 没补齐 -> 在启动预算(relayLogSearchStartupBudget)内从游标处接着补,
	//     预算用完就返回 nil, 剩下的交给后台循环(op.RelayLogSearchBackfillStart)
	//     和下一次启动。永远不阻断启动。
	RegisterAfterAutoMigration(Migration{
		Version:   14,
		Up:        migrateBackfillRelayLogSearchIndex,
		AlwaysRun: true,
	})
}

// RelayLogSearchBackfillState 是"存量到底补齐了没有"那唯一一行标记。
//
// 为什么不能只认 migration_records 里的 success: 014 在探测/回填中途失败时**刻意
// 返回 nil**(不让一次可降级的回填故障把服务挡在启动之外), 而框架收到 nil 就记
// success(migrate.go:117-121)。于是 success 只代表"这一遍跑完了", 不代表"投影表
// 完整"。搜索侧拿它当完整性判据时会直接返回空页(internal/op/log_search_index.go),
// 那批没回填的老日志就永久搜不到, 而页面看起来完全正常。
//
// 单独一张表而不是塞进 settings: settings 会被 SettingList / 备份导出带走,
// 换一台机器导入之后"表是完整的"这个断言就是假的。
//
// Cursor 是回填游标: id <= Cursor 的 relay_logs 行都已确认在投影表里(Cursor <= 0 表示
// 一行都还没确认, 不对任何 id 担保)。
//   - 让回填可续跑: 进程被杀、预算用完之后从这里接着走, 不必从头探测。
//     (以前每一轮都用 LEFT JOIN 从最小 id 开始找缺行, 已补过的前缀每批都要重扫一遍,
//     总工作量随行数平方增长, 而且整段都在 InitDB 里同步跑, 18GB 的库启动要等它。)
//   - 让搜索在回填进行中也正确: 投影表只对 id <= Cursor 负责, 大于游标的那段
//     由搜索侧补一次主表 LIKE(见 op.relayLogSearchIndexState)。
type RelayLogSearchBackfillState struct {
	ID       int   `gorm:"primaryKey"`
	Complete bool  `gorm:"not null"`
	Cursor   int64 `gorm:"not null;default:0"`
}

// relayLogSearchBackfillStateRowID 是单行表的主键。
const relayLogSearchBackfillStateRowID = 1

// relayLogSearchBackfillBatch 是一批回填走过的 relay_logs 行数。
//
// 一批: 读 2000 个 id(主键范围扫描) + 投影表同范围的 id + 只对缺的那些行读搜索列
// 再写进窄表。写锁只在最后那几条窄表 INSERT 上, 毫秒级。
const relayLogSearchBackfillBatch = 2000

// relayLogSearchStartupBudget 是启动阶段最多花在回填上的时间(与 021 同一取舍)。
//
// 生产库 18GB: 把每一行的搜索列读一遍不是启动该等的事。预算用完就返回, 服务先起来,
// 后台循环接着补; 补完之前搜索走"投影表 + 游标之后主表 LIKE"的组合, 结果仍然完整。
// 小库(和测试)在预算内就能补完。
const relayLogSearchStartupBudget = 3 * time.Second

// relayLogSearchTailReconcileRows 是已补齐的库每次启动时再核对的"最新若干行"。
//
// 写入路径的投影 upsert 失败是吞掉的(只是暂时搜不到, 不该让落库失败), 这些行一定是
// 最近写的。以前 014 每次启动用 LEFT JOIN 扫全表找它们 —— 在已补齐的大库上那是一整遍
// 全表扫描, 只为了找到 0 行。现在只对最新这一段做同样的核对, 成本有上界。
var relayLogSearchTailReconcileRows = 5000 // var 只为测试能把窗口缩小; 生产不改它

// RelayLogSearchBackfillOptions 控制一次回填调用做多少。
//
// MaxBatches / Budget 为 0 表示不设该上限。两者都为 0 时一直跑到补齐为止。
type RelayLogSearchBackfillOptions struct {
	MaxBatches int
	Budget     time.Duration
}

// relayLogSearchBackfillSave 写入/更新完整性标记与游标。
//
// 失败只记日志: 标记写不上 = 搜索侧一直走主表 LIKE 退路, 慢但正确,
// 不该让启动失败, 也不该让搜索变成空页。
func relayLogSearchBackfillSave(db *gorm.DB, complete bool, cursor int64) {
	if db == nil {
		return
	}
	if !db.Migrator().HasTable(&RelayLogSearchBackfillState{}) || !db.Migrator().HasColumn(&RelayLogSearchBackfillState{}, "Cursor") {
		if err := db.AutoMigrate(&RelayLogSearchBackfillState{}); err != nil {
			log.Warnf("migrate 014: ensure backfill marker table failed: %v", err)
			return
		}
	}
	row := RelayLogSearchBackfillState{ID: relayLogSearchBackfillStateRowID, Complete: complete, Cursor: cursor}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"complete", "cursor"}),
	}).Create(&row).Error; err != nil {
		log.Warnf("migrate 014: write backfill marker (complete=%v cursor=%d) failed: %v", complete, cursor, err)
	}
}

// RelayLogSearchBackfillComplete 报告存量行是否已经真的补齐。
//
// 表不存在(迁移还没跑到)、没有那一行、或标记为 false, 一律算"没补齐" ——
// 宁可让搜索走慢路径, 也不能让它对着缺行的投影表宣布空页。
func RelayLogSearchBackfillComplete(tx *gorm.DB) bool {
	complete, _ := RelayLogSearchBackfillProgress(tx)
	return complete
}

// RelayLogSearchBackfillProgress 返回(是否补齐, 游标)。
//
// 游标的含义: id <= cursor 的 relay_logs 行已确认在投影表里。没有标记行时返回
// (false, 0) —— 投影表对任何 id 都不担保, 搜索必须整段走主表 LIKE。
func RelayLogSearchBackfillProgress(tx *gorm.DB) (bool, int64) {
	if tx == nil || !tx.Migrator().HasTable(&RelayLogSearchBackfillState{}) {
		return false, 0
	}
	var st RelayLogSearchBackfillState
	q := tx.Model(&RelayLogSearchBackfillState{})
	if !tx.Migrator().HasColumn(&RelayLogSearchBackfillState{}, "Cursor") {
		// 老结构(还没加 cursor 列): 只认 complete, 游标当 0。
		q = q.Select("id, complete")
	}
	if err := q.Where("id = ?", relayLogSearchBackfillStateRowID).Take(&st).Error; err != nil {
		return false, 0
	}
	return st.Complete, st.Cursor
}

// 014: 建 relay_log_search 投影表并回填存量日志的搜索列。
//
// 背景(实测 5 万行): 日志页搜索走 12 个字段的 LIKE '%x%'。前缀通配符注定用不上
// 索引, 只能全表逐行扫描; 而 relay_logs 每行带着 request_content/response_content
// (真实日志常在几百字节到几十 KB), 扫描要为每一行读溢出页。
//   - 在 relay_logs 上搜: 277ms
//   - 在只含搜索列的窄表上搜: 9ms
//   - 只搜 1 列 vs 12 列: 266ms vs 420ms(可见慢的不是字段数, 是回表读大字段)
//
// 所以把参与搜索的列投影成一张窄表, 搜索先在这里拿到 id, 再回主表取那 20 行。
//
// 安全性:
//   - 只增不改: 只建新表 + 灌数据, 不动 relay_logs 任何一列。
//   - 幂等: 建表用 IF NOT EXISTS 语义(AutoMigrate), 回填用 INSERT ... ON CONFLICT
//     DO NOTHING / 先 DELETE 后 INSERT, 重复跑不会重复插入也不会报错。
//   - 分段回填: 一次 INSERT ... SELECT 会在大表上长时间占锁; 按 id 区间分批,
//     每批之间让出, 启动不会因为几百万行日志而卡住。
//   - 可失败降级: 回填中途失败留待下次启动继续(记录只在成功后写入), 而运行时
//     写入路径会继续补齐新行 —— 缺的行只是"暂时搜不到", 不会搜错。
//
// ensureRelayLogTotalAttemptsIndex 给 relay_logs.total_attempts 补索引。
//
// 🔴 这里原来是手写 `CREATE INDEX IF NOT EXISTS ...`。那个语法 **MySQL 不支持**
// （MariaDB 支持，MySQL 从来没有给 CREATE INDEX 加过 IF NOT EXISTS），会直接报
// 1064 语法错。而错误被 `_ =` 吞掉，于是 MySQL 部署上索引永远建不上、也永远没人
// 会知道 —— 两个谓词继续全表扫（10 万行实测 847ms）。本迁移标了 AlwaysRun，
// 每轮启动还会再报一次同样的错。
//
// 为什么不用 `AutoMigrate(&model.RelayLog{})`：它会顺带把**缺失的列也补回来**。
// 本迁移的"半途而废"语义依赖"读批失败就返回 nil、标记留 false"，而测试正是用
// 删列来制造这个失败；AutoMigrate 把列补回去，那个分支就再也走不到，
// 一条守着"别对着缺行表宣布空页"的用例会变绿得毫无意义。
// 索引归索引，列归列 —— 这里只碰索引。
//
// 三方方言分别处理：
//   - sqlite / postgres: 原生支持 IF NOT EXISTS，一条语句即可。
//   - mysql: 没有 IF NOT EXISTS，但 `HasIndex` 能可靠回答"建没建过"，
//     据此决定要不要 CREATE。索引名必须与 GORM 按 struct tag 生成的保持一致
//     （idx_relay_logs_total_attempts），否则 HasIndex 永远查不到、每次启动都重建。
//
// 失败只记日志：索引缺失只是慢，不影响正确性，不该把服务挡在启动之外。
func ensureRelayLogTotalAttemptsIndex(db *gorm.DB) {
	const indexName = "idx_relay_logs_total_attempts"
	switch db.Dialector.Name() {
	case "mysql":
		// GORM 对 MySQL 的默认索引名就是 idx_<table>_<column>，与上面常量一致。
		if db.Migrator().HasIndex(&model.RelayLog{}, indexName) {
			return
		}
		if err := db.Exec("CREATE INDEX " + indexName + " ON relay_logs(total_attempts)").Error; err != nil {
			log.Warnf("migrate 014: ensure relay_logs(total_attempts) index failed: %v", err)
		}
	default:
		if err := db.Exec("CREATE INDEX IF NOT EXISTS " + indexName + " ON relay_logs(total_attempts)").Error; err != nil {
			log.Warnf("migrate 014: ensure relay_logs(total_attempts) index failed: %v", err)
		}
	}
}

func migrateBackfillRelayLogSearchIndex(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("relay_logs") {
		return nil
	}

	// 主 AutoMigrate 名单(db.go)里也有这两张表; 这里再保一次, 是为了让直接调用本函数
	// 的路径(测试、单独跑迁移)不依赖调用顺序。表已存在时这只是一次结构比对。
	if err := db.AutoMigrate(&model.RelayLogSearchIndex{}, &RelayLogSearchBackfillState{}); err != nil {
		return fmt.Errorf("create relay_log_search: %w", err)
	}

	// 顺带给主表的 total_attempts 建索引。
	//
	// 严重程度分桶(warn = total_attempts > 1)和 retried 筛选都是在它上面做的判断,
	// 但这一列此前没有索引, 两个谓词只能全表逐行求值: 实测 10 万行 847ms。
	// 加上索引后同样的谓词 10.5ms。新库由 AutoMigrate 按 struct tag 直接建出,
	// 这里是为了让已经存在的库也补上。失败只记日志不阻断启动(见函数注释)。
	ensureRelayLogTotalAttemptsIndex(db)

	complete, err := ReconcileRelayLogSearchBackfillMarker(db)
	if err != nil {
		log.Warnf("migrate 014: consistency probe failed, will retry next startup: %v", err)
		return nil
	}
	if complete {
		if err := relayLogSearchReconcileTail(db, relayLogSearchTailReconcileRows); err != nil {
			log.Warnf("migrate 014: tail reconcile failed, will retry next startup: %v", err)
		}
		return nil
	}

	if _, err := BackfillRelayLogSearchIndex(db, RelayLogSearchBackfillOptions{Budget: relayLogSearchStartupBudget}); err != nil {
		// 🔴 返回 nil，不是 err。
		//
		// 框架契约(migrate.go:17-19)原文：Up MUST be idempotent and MUST return
		// nil — not an error — when it intentionally can't finish yet, so a
		// transient skip never aborts startup. 而 runMigrationsWithRecord 一收到
		// 非 nil 就记 failed 并 return err(migrate.go:117-121)，AfterAutoMigrate
		// 再 return err，db.InitDB 再 return err，cmd/start.go 直接退出 ——
		// 服务起不来。一次可降级的回填故障(库忙、连接抖动、进程被杀)不该让整个
		// 服务下线。续跑由游标 + AlwaysRun + 后台循环提供, 这里记 warn 留证据。
		log.Warnf("migrate 014: search-index backfill stopped, will retry: %v", err)
	}
	return nil
}

// ReconcileRelayLogSearchBackfillMarker 核对"已补齐"标记有没有撒谎, 返回核对后的 complete。
//
// 标记说"已补齐", 但投影表可能被删过/清空过(主 AutoMigrate 名单会把删掉的表重新建成
// 空表)。主表有行而投影表一行都没有 = 标记在撒谎, 搜索会对着空表宣布"没有匹配"。
// 这时把标记改回 (false, 0), 回填从头走。两条 LIMIT 1 探测, 大库上也是毫秒级。
//
// db.InitDB 在主 AutoMigrate 之后直接调它, 不经过迁移框架: 迁移在一个进程里只跑一次、
// 而且可能因为执行记录被跳过, 这条核对不能押在迁移的执行历史上(与把表放进主名单同一条教训)。
func ReconcileRelayLogSearchBackfillMarker(db *gorm.DB) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("db is nil")
	}
	complete, _ := RelayLogSearchBackfillProgress(db)
	if !complete || !db.Migrator().HasTable("relay_logs") || !db.Migrator().HasTable(&model.RelayLogSearchIndex{}) {
		return complete, nil
	}
	hasLogs, err := relayLogSearchTableHasRows(db, "relay_logs")
	if err != nil {
		return complete, err
	}
	hasIndex, err := relayLogSearchTableHasRows(db, "relay_log_search")
	if err != nil {
		return complete, err
	}
	if hasLogs && !hasIndex {
		log.Warnf("relay_log_search is empty but relay_logs is not; restarting the search-index backfill from the beginning")
		relayLogSearchBackfillSave(db, false, 0)
		return false, nil
	}
	return true, nil
}

func relayLogSearchTableHasRows(db *gorm.DB, table string) (bool, error) {
	var ids []int64
	if err := db.Table(table).Select("id").Limit(1).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// BackfillRelayLogSearchIndex 从游标处按 id 升序分批补投影行。
//
// 返回 complete=true 表示游标已走到主表末尾、标记已置 true。complete=false 且
// err=nil 表示碰到了 MaxBatches / Budget, 调用方稍后再调。err != nil 是读或写失败:
// 已提交的批和游标留在库里, 下一次从游标接着做。
//
// 每批:
//  1. 主表按主键取游标之后的 2000 个 id(rowid 范围扫描, 不读正文);
//  2. 投影表取同一 id 区间已有的 id(窄表主键范围);
//  3. 只对缺的那些读搜索列, ON CONFLICT DO NOTHING 写进窄表;
//  4. 游标推进到这批最后一个 id, 和 complete=false 一起落库。
//
// 幂等: 重复跑只会在第 2 步发现"都有了"然后推进游标。并发安全: 写入路径对新行做
// upsert, 与这里的 DO NOTHING 撞主键时谁先谁后结果都一样(两边投影的是同一行)。
func BackfillRelayLogSearchIndex(db *gorm.DB, opt RelayLogSearchBackfillOptions) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("relay_logs") {
		relayLogSearchBackfillSave(db, true, 0)
		return true, nil
	}
	if !db.Migrator().HasTable(&model.RelayLogSearchIndex{}) {
		if err := db.AutoMigrate(&model.RelayLogSearchIndex{}); err != nil {
			return false, fmt.Errorf("create relay_log_search: %w", err)
		}
	}
	complete, cursor := RelayLogSearchBackfillProgress(db)
	if complete {
		return true, nil
	}
	started := time.Now()
	batches := 0
	for {
		ids, err := relayLogSearchNextIDs(db, cursor, relayLogSearchBackfillBatch)
		if err != nil {
			return false, err
		}
		if len(ids) == 0 {
			relayLogSearchBackfillSave(db, true, cursor)
			return true, nil
		}
		if _, err := relayLogSearchFillRange(db, ids); err != nil {
			return false, err
		}
		cursor = ids[len(ids)-1]
		if len(ids) < relayLogSearchBackfillBatch {
			// 不足一批 = 已走到主表末尾。
			relayLogSearchBackfillSave(db, true, cursor)
			return true, nil
		}
		relayLogSearchBackfillSave(db, false, cursor)
		relayLogSearchCheckpoint(db)
		batches++
		if opt.MaxBatches > 0 && batches >= opt.MaxBatches {
			return false, nil
		}
		if opt.Budget > 0 && time.Since(started) >= opt.Budget {
			log.Infof("migrate 014: search-index backfill paused at id %d after %d batches (budget %s); it resumes in the background and on next startup", cursor, batches, opt.Budget)
			return false, nil
		}
	}
}

// relayLogSearchReconcileTail 对最新 n 行再做一次"缺就补", 不动标记与游标。
func relayLogSearchReconcileTail(db *gorm.DB, n int) error {
	var start []int64
	if err := db.Table("relay_logs").Select("id").Order("id DESC").Offset(n-1).Limit(1).Pluck("id", &start).Error; err != nil {
		return err
	}
	var cursor int64
	if len(start) > 0 {
		cursor = start[0] - 1
	}
	for {
		ids, err := relayLogSearchNextIDs(db, cursor, relayLogSearchBackfillBatch)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := relayLogSearchFillRange(db, ids); err != nil {
			return err
		}
		if len(ids) < relayLogSearchBackfillBatch {
			return nil
		}
		cursor = ids[len(ids)-1]
	}
}

// relayLogSearchNextIDs 取 id > after 的下一批主表 id。after <= 0 表示"还没走过任何行",
// 不设下界 —— 否则 id 为 0 的行(手工导入/测试数据)永远不会被回填。
func relayLogSearchNextIDs(db *gorm.DB, after int64, n int) ([]int64, error) {
	var ids []int64
	q := db.Table("relay_logs").Select("id")
	if after > 0 {
		q = q.Where("id > ?", after)
	}
	err := q.Order("id ASC").Limit(n).Pluck("id", &ids).Error
	return ids, err
}

// relayLogSearchFillRange 把 ids(升序)里投影表还没有的那些补上, 返回补了几行。
func relayLogSearchFillRange(db *gorm.DB, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var have []int64
	if err := db.Table("relay_log_search").Select("id").
		Where("id >= ? AND id <= ?", ids[0], ids[len(ids)-1]).
		Pluck("id", &have).Error; err != nil {
		return 0, err
	}
	haveSet := make(map[int64]struct{}, len(have))
	for _, id := range have {
		haveSet[id] = struct{}{}
	}
	missing := make([]int64, 0, len(ids)-len(have))
	for _, id := range ids {
		if _, ok := haveSet[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	var src []model.RelayLog
	if err := db.Select(
		"id, user_name, request_api_key_name, request_model_name, actual_model_name, "+
			"upstream_response_model, channel_name, request_endpoint, request_path, "+
			"session_key, error, error_code, request_user_agent, channel_id",
	).Where("id IN ?", missing).Find(&src).Error; err != nil {
		return 0, err
	}
	if len(src) == 0 {
		// 这批行在两次读之间被清理掉了: 没有要补的。
		return 0, nil
	}
	rows := make([]model.RelayLogSearchIndex, 0, len(src))
	for _, l := range src {
		rows = append(rows, model.RelayLogSearchIndex{
			ID:                    l.ID,
			UserName:              l.UserName,
			RequestAPIKeyName:     l.RequestAPIKeyName,
			RequestModelName:      l.RequestModelName,
			ActualModelName:       l.ActualModelName,
			UpstreamResponseModel: l.UpstreamResponseModel,
			ChannelName:           l.ChannelName,
			RequestEndpoint:       l.RequestEndpoint,
			RequestPath:           l.RequestPath,
			SessionKey:            l.SessionKey,
			Error:                 l.Error,
			ErrorCode:             l.ErrorCode,
			RequestUserAgent:      l.RequestUserAgent,
			ChannelId:             l.ChannelId,
		})
	}
	// 只插不存在的: OnConflict DoNothing 在 sqlite/mysql/pg 上都可用,
	// 避免与写入路径同时插入时撞主键。
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
}

func relayLogSearchCheckpoint(db *gorm.DB) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "sqlite" {
		return
	}
	// 每批提交后做一次被动 checkpoint, 避免回填把 WAL 撑大。PASSIVE 不等其他读者。
	_ = db.Exec("PRAGMA wal_checkpoint(PASSIVE)").Error
}
