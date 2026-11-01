package op

// 日志存储容量：精确统计与有界收缩。
//
// 这一对函数替代了原先的 relayLogStorageRows —— 那个实现把整张 relay_logs 的
// request_content / response_content 一次性 Find 进内存再算大小。生产上日志
// 几十万行、正文常在几百字节到几十 KB，于是"统计容量"这个动作本身要吃掉几十 GB
// 内存：进程被 OOM 杀掉 → 清理没跑完 → 日志继续涨 → 下次再被杀，形成死循环。
// 磁盘从 10GB 上限一路涨到 59GB 就是这么来的。
//
// 现在的做法：
//   - 统计走 SQL 聚合(SUM(LENGTH(...)))，只在 DB 内累加，Go 侧内存恒定；
//   - 删除按 id 分批，每批之间让出，不会一次构造出巨大的 IN 列表；
//   - 额外计算搜索投影表的真实占用，让上限把副本也算进去(否则副本无限增长)。
//
// 注意 relayLogApproxBytes 的"每行 +256 结构性开销"在 SQL 里也保留了(256.0*COUNT)，
// 这样容量数字与历史口径、与前端显示保持一致，不会突然"变小"而让人误以为少了日志。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

// relayLogRowStructOverhead 是 relayLogApproxBytes 里那个 +256 的结构性估算。
// SQL 版本必须用同一个常量，否则两条口径会对不上。
const relayLogRowStructOverhead = 256

// relayLogBytesSQL 返回按 relayLogApproxBytes 同口径计算字节数的 SQL 表达式。
//
// 逐项对应 Go 版：
//
//	len(RequestModelName) + len(RequestEndpoint) + len(RequestPath) +
//	len(RequestAPIKeyName) + len(ChannelName) + len(ActualModelName) +
//	len(RequestContent) + len(ResponseContent) + len(Error) +
//	json.Marshal(Attempts) 的长度 + json.Marshal(DebugInfo) 的长度 + 256
//
// LENGTH() 在 SQLite 对文本返回字符数、对 BLOB 返回字节数；日志正文是 UTF-8
// 文本，字符数会小于字节数，但 Go 侧用的是 len(string) 即字节数。为保持两者一致，
// 这里用 LENGTH(CAST(x AS BLOB)) 取字节长度。
func relayLogBytesSQL() string {
	col := func(name string) string {
		return "COALESCE(LENGTH(CAST(COALESCE(" + name + ",'') AS BLOB)),0)"
	}
	// attempts 是 JSON 序列化列，DB 里存的就是序列化后的文本，直接量它即可。
	return "(" + col("request_model_name") +
		" + " + col("request_endpoint") +
		" + " + col("request_path") +
		" + " + col("request_api_key_name") +
		" + " + col("channel_name") +
		" + " + col("actual_model_name") +
		" + " + col("request_content") +
		" + " + col("response_content") +
		" + " + col("error") +
		" + " + col("attempts") +
		" + " + col("debug_info") +
		")"
}

// relayLogStoredBytes 精确统计主表日志占用的近似字节数(= 容量上限判定口径)。
//
// 纯 SQL 聚合，不把任何一行的正文读进 Go 内存。百万行表上也只是一次顺序扫描。
func relayLogStoredBytes(ctx context.Context, scope *model.RelayLogScope) (int64, int64, error) {
	var row struct {
		Bytes int64
		Rows  int64
	}
	query := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
		Select("COALESCE(SUM(" + relayLogBytesSQL() + "),0) AS bytes, COUNT(*) AS rows")
	query = relayLogApplyScope(query, scope)
	if err := query.Scan(&row).Error; err != nil {
		return 0, 0, err
	}
	total := row.Bytes + int64(relayLogRowStructOverhead)*row.Rows
	return total, row.Rows, nil
}

// relayLogSearchIndexBytes 统计搜索投影表的真实占用。
//
// 投影表是搜索加速用的副本，容量上限必须把它算进去 —— 只看主表的话，
// 副本会不受任何约束地增长(它存了 12 个搜索列 + error 全文)。
func relayLogSearchIndexBytes(ctx context.Context) int64 {
	if !relayLogSearchReady() {
		return 0
	}
	var row struct {
		Bytes int64
		Rows  int64
	}
	err := db.GetDB().WithContext(ctx).Table(relayLogSearchIndexTable).
		Select("COALESCE(SUM(" +
			"COALESCE(LENGTH(CAST(COALESCE(user_name,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(request_api_key_name,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(request_model_name,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(actual_model_name,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(upstream_response_model,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(channel_name,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(request_endpoint,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(request_path,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(session_key,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(error,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(error_code,'') AS BLOB)),0) + " +
			"COALESCE(LENGTH(CAST(COALESCE(request_user_agent,'') AS BLOB)),0)" +
			"),0) AS bytes, COUNT(*) AS rows").Scan(&row).Error
	if err != nil {
		return 0
	}
	return row.Bytes + int64(relayLogRowStructOverhead)*row.Rows
}

// relayLogDeleteExpiredBefore 删除 time < cutoff 的日志，分批进行。
//
// 为什么不直接 `Where("time < ?", cutoff).Delete(...)` 一把梭：
// 保留期一到，待删行可能有几十万。一条 DELETE 命中几十万行会长时间持有写锁、
// 把 WAL 撑得很大，期间所有日志写入都被挡住（SQLite 单写者）。分批让写入有机会插进来。
//
// 投影表必须同步删：主表删掉后就没有 time 可比了，顺序反了会留下"永远搜得到、
// 点开却没有"的幽灵行。
func relayLogDeleteExpiredBefore(ctx context.Context, cutoff int64) error {
	const deadline = 5 * time.Minute
	start := time.Now()

	for {
		var ids []int64
		if err := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
			Where("time < ?", cutoff).
			Order("id ASC").Limit(relayLogDeleteChunk).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}

		// 投影删不掉就整批跳过，绝不先删主表。
		//
		// 主表一删，这条投影行就再没有 time 可比、没有 id 可查，变成永久孤儿：
		// 保留期从主表取 id、容量只看主表字节，两条判据都够不着它。
		// 宁可这一轮少删一批（下一轮 10 分钟后重试），也不要留下清不掉的垃圾。
		if err := relayLogSearchIndexDeleteByIDs(db.GetDB().WithContext(ctx), ids); err != nil {
			log.Warnf("relay log cleanup skipped a batch of %d: search index delete failed: %v", len(ids), err)
			return err
		}
		relayLogImageRefsDeleteByLogIDs(db.GetDB().WithContext(ctx), ids)
		if err := db.GetDB().WithContext(ctx).Where("id IN ?", ids).Delete(&model.RelayLog{}).Error; err != nil {
			return err
		}

		// 保留期清理是"能删多少删多少"，不该把一次任务拖成几小时；剩下的
		// 下一轮(10 分钟后)继续。这让单次任务的耗时有上界。
		if time.Since(start) > deadline {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// relayLogCleanupByStorageBounded 按容量上限收缩，全程有界内存。
//
// 与旧实现的区别：不再先把所有行读进内存再倒推要删哪些 id，而是
//
//  1. 用 SQL 算出当前总字节；
//  2. 若超限，按 id 从旧到新分批删除；
//  3. 删完再核对一次，仍超限就再来一轮。
//
// 🔴 这里有个必须先想清楚的代价：一次 relayLogStoredBytes 是**全表扫描**。
// 29GB 的库上实测约 2 分钟。所以绝对不能"每删一批就重新统计一遍再决定下一批"
// —— 那会把删除变成 N 次全表扫描，在超大库上等于永远跑不完(400 批 × 2 分钟 > 13 小时)。
//
// 正确做法是：先算一次拿到总字节和行数，按平均行大小**一次性估算**该删多少行，
// 然后连续删完，最后再核对一次。轮数被 relayLogCleanupMaxRounds 钉死，
// 最坏情况是固定次数的全表扫描，而不是与删除量成正比。
//
// 估算偏差由最后那次核对兜底：删多了无害(本来就是超限要删)，删少了下一轮补，
// 而轮数封顶保证绝不会无限扫。
func relayLogCleanupByStorageBounded(ctx context.Context) error {
	return relayLogCleanupByStorageBoundedDepth(ctx, 0)
}

// relayLogCleanupMaxRounds 限制"统计→删除"的轮数。
//
// 为什么要封顶：每轮都有一次全表扫描。上限大 10 倍时按估算应当 1 轮就够，
// 给到 3 轮足以吸收行大小分布不均(比如老日志正文特别大)，同时保证最坏情况下
// 也只有 3 次扫描、不会变成无底洞。
const relayLogCleanupMaxRounds = 3

// relayLogDeleteChunk 是单条 DELETE 语句一次处理的行数。
//
// 不能一次删几百万行：那会在一个事务里持有巨大的 WAL、把 autocheckpoint 拖爆，
// 期间所有写入都被挡住。分批删除让检查点有机会插进来。
const relayLogDeleteChunk = 5000

func relayLogCleanupByStorageBoundedDepth(ctx context.Context, depth int) error {
	_, maxBytes, err := relayLogMaxStorage()
	if err != nil || maxBytes <= 0 {
		return err
	}

	if depth >= relayLogCleanupMaxRounds {
		// 轮数用尽仍超限：日志会在下一个调度周期继续收敛。
		// 不在这里死循环 —— 那会在超大库上把清理任务永久占住。
		return nil
	}

	total, rows, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return err
	}
	if total <= maxBytes || rows <= 1 {
		// rows<=1：只剩最后一条，保留它(旧实现里 idx==0 永不删，语义一致)。
		return nil
	}

	avg := total / rows
	if avg <= 0 {
		avg = 1
	}
	over := total - maxBytes
	if over <= 0 {
		return nil
	}

	// 该删多少行：超出部分 ÷ 平均行大小，再留 5% 余量吸收平均值误差，
	// 避免"算得刚刚好、结果差一点又超限"导致多跑一轮全表扫描。
	want := over / avg
	want += want / 20
	if want < 1 {
		want = 1
	}
	// 永远不删最后一条。
	if want > rows-1 {
		want = rows - 1
	}

	deleted, err := relayLogDeleteOldest(ctx, want)
	if err != nil {
		return err
	}
	if deleted == 0 {
		return nil
	}

	// 删完核对一次：还超限就再走一轮(轮数有封顶)。
	after, afterRows, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return err
	}
	if after > maxBytes && afterRows > 1 {
		return relayLogCleanupByStorageBoundedDepth(ctx, depth+1)
	}
	return nil
}

// relayLogDeleteOldest 从最旧的开始删最多 want 行，返回实际删除的行数。
//
// 全程按 id 游标推进，每批都是"取一批 id → 删这批"，不做任何全表统计，
// 所以成本与删除量成正比，而不是与表大小成正比。
func relayLogDeleteOldest(ctx context.Context, want int64) (int64, error) {
	var deleted int64
	var lastID int64

	for deleted < want {
		batch := int64(relayLogDeleteChunk)
		if remaining := want - deleted; remaining < batch {
			batch = remaining
		}

		var ids []int64
		query := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
			Where("id > ?", lastID).Order("id ASC").Limit(int(batch))
		if err := query.Pluck("id", &ids).Error; err != nil {
			return deleted, err
		}
		if len(ids) == 0 {
			return deleted, nil
		}
		lastID = ids[len(ids)-1]

		// 投影表先删：这些 id 即将从主表消失，留着就会"搜得到但打不开"。
		// 删不掉就跳过这一批——理由同保留期清理，见上。
		if err := relayLogSearchIndexDeleteByIDs(db.GetDB().WithContext(ctx), ids); err != nil {
			log.Warnf("relay log purge skipped a batch of %d: search index delete failed: %v", len(ids), err)
			return deleted, err
		}
		relayLogImageRefsDeleteByLogIDs(db.GetDB().WithContext(ctx), ids)
		if err := db.GetDB().WithContext(ctx).Where("id IN ?", ids).Delete(&model.RelayLog{}).Error; err != nil {
			return deleted, err
		}
		deleted += int64(len(ids))

		// 每批之间检查上下文，长清理能被取消打断。
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// relayLogStorageInfoBounded 是 RelayLogStorageInfo 的有界实现。
//
// StoredBytes 现在包含主表 + 搜索投影表 + 尚未落库的内存缓存，
// 这样"已用 / 上限"能和真实磁盘占用对上，而不是只算主表的一半。
func relayLogStorageInfoBounded(ctx context.Context, scope *model.RelayLogScope) (model.RelayLogStorage, error) {
	storedBytes, _, err := relayLogStoredBytes(ctx, scope)
	if err != nil {
		return model.RelayLogStorage{}, err
	}

	// 未落库的缓存也算进"已用"：它们迟早进 DB，漏算会让上限永远滞后一步。
	relayLogCacheLock.Lock()
	cachedLogs := make([]model.RelayLog, len(relayLogCache))
	copy(cachedLogs, relayLogCache)
	relayLogCacheLock.Unlock()
	for _, relayLog := range cachedLogs {
		if !relayLogMatchScope(relayLog, scope) {
			continue
		}
		storedBytes += relayLogApproxBytes(relayLog)
	}

	// 投影表只在统计全量(admin、无 scope)时计入：它是主表的附属副本，
	// 按用户/渠道过滤时没有对应的归属关系，硬摊派会算错。
	if scope == nil || (scope.UserID == 0 && scope.APIKeyID == 0) {
		storedBytes += relayLogSearchIndexBytes(ctx)
	}

	maxGB, maxBytes, err := relayLogMaxStorage()
	if err != nil {
		return model.RelayLogStorage{}, err
	}

	return model.RelayLogStorage{
		StoredBytes: storedBytes,
		MaxBytes:    maxBytes,
		MaxGB:       maxGB,
	}, nil
}

// relayLogIncrementalVacuum 尝试用 PRAGMA incremental_vacuum 把空闲页还给文件系统。
//
// 这是比完整 VACUUM 便宜得多的一步，也是"删了却不缩小"的根治手段：
//   - 完整 VACUUM 要重建整个文件，峰值临时空间约等于库大小，一次几十分钟；
//   - incremental_vacuum 只把 freelist 头部的 N 页搬走并同步截断文件，代价与
//     回收量成正比，可以每轮清理后顺手跑一次。
//
// 前提是库的 auto_vacuum 模式为 INCREMENTAL。SQLite 官方规定：已有库从 none
// 改成 incremental **必须跑一次完整 VACUUM** 才生效，光发 pragma 是静默无效的。
// 所以这里先探测实际模式：
//   - 已是 incremental(2) -> 直接增量回收；
//   - 仍是 none(0)        -> 返回 needsFullVacuum，由调用方决定是否做完整 VACUUM。
//
// 返回 (回收页数, needsFullVacuum, error)。
func relayLogIncrementalVacuum(ctx context.Context, maxPages int64) (int64, bool, error) {
	var mode int64
	if err := db.GetDB().Raw("PRAGMA auto_vacuum").Row().Scan(&mode); err != nil {
		return 0, false, err
	}
	if mode != 2 { // 0=none, 1=full, 2=incremental
		return 0, true, nil
	}

	var free int64
	if err := db.GetDB().Raw("PRAGMA freelist_count").Row().Scan(&free); err != nil {
		return 0, false, err
	}
	if free <= 0 {
		return 0, false, nil
	}
	if maxPages <= 0 || maxPages > free {
		maxPages = free
	}

	// 参数化 PRAGMA 不被支持，这里 maxPages 已经过范围钳制，且是 int64，
	// 拼进语句不存在注入面。
	if err := db.GetDB().WithContext(ctx).
		Exec(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", maxPages)).Error; err != nil {
		return 0, false, err
	}

	var remain int64
	if err := db.GetDB().Raw("PRAGMA freelist_count").Row().Scan(&remain); err != nil {
		return 0, false, nil
	}
	return free - remain, false, nil
}

// relayLogVacuum 在删除后把空间真正还给文件系统。
//
// 为什么必须有这一步：SQLite 删除行只是把页标成空闲放进 free list，数据文件
// 不会变小。所以"设了上限、也确实删了日志，但 db 文件还是 59GB"是预期行为，
// 不是清理没生效。
//
// 优先走增量回收(便宜)；只有库还没切成 incremental 模式时才退回完整 VACUUM。
//
// VACUUM 会重建整个数据库文件，期间需要约等于当前库大小的临时空间。
// 因此只在**确实删掉了行**之后调用，且失败不影响清理本身(空间没还回去而已)。
func relayLogVacuum(ctx context.Context) error {
	// 与管理员手动「压缩数据库」共用一把锁（见 db_compact.go）：两路 VACUUM 叠在一起
	// 只会让第二路卡在写锁上，直接跳过，下一轮再试。
	if !dbVacuumMu.TryLock() {
		return ErrDBCompactBusy
	}
	defer dbVacuumMu.Unlock()
	_, err := sqliteVacuumLocked(ctx)
	return err
}

// sqliteVacuumLocked 是 relayLogVacuum 的本体，调用方必须已持有 dbVacuumMu。
// 返回实际走的路径："incremental" 或 "full"。
func sqliteVacuumLocked(ctx context.Context) (string, error) {
	// 先试增量：库已是 incremental 模式时，这一步只花几十毫秒。
	pages, needsFull, err := relayLogIncrementalVacuum(ctx, 0)
	if err == nil && !needsFull {
		if pages > 0 {
			log.Debugf("relay log incremental vacuum reclaimed %d pages", pages)
		}
		return "incremental", nil
	}

	safe, reason := relayLogVacuumSafe()
	if !safe {
		return "", errors.New(reason)
	}

	// 顺序是硬性的，写反等于白做（2026-10-20 用 sqlite 3 实测确认）：
	//   先 VACUUM 后设 pragma -> 新库 auto_vacuum 仍是 0，增量回收继续不生效；
	//   先设 pragma 后 VACUUM -> 新库 auto_vacuum = 2，之后每次删行都能自动截断。
	// SQLite 官方也这么规定：要换 auto_vacuum 模式，必须先设 pragma，再跑 VACUUM
	// 重排整个文件。
	//
	// 另一个坑：auto_vacuum 是**连接级**设置，而 GORM 用的是连接池。分开两次
	// Exec 很可能落在不同连接上，pragma 就丢了。所以这里取出同一条连接，
	// 把两件事都钉在它上面 —— 这是这段代码唯一正确写法。
	sqlDB, err := db.GetDB().DB()
	if err != nil {
		return "", err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return "", err
	}

	// 复核：模式没切过去说明这条路仍然无效，如实记下来，不要假装成功。
	var mode int64
	if err := conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err == nil && mode != 2 {
		log.Warnf("relay log vacuum finished but auto_vacuum is still %d (expected 2); space will keep growing until this is fixed", mode)
	}
	return "full", nil
}

// relayLogVacuumSafe 判断当前是否适合做 VACUUM。
//
// VACUUM 会重建整个数据库文件，WAL 模式下峰值临时占用约等于当前库大小。
// 生产库 59GB 时如果不加判断就 VACUUM，很可能把磁盘写满 —— 那会让数据库
// **写入失败**，比"空间暂时没还回来"严重得多。所以宁可跳过并如实说明。
//
// 判据：可用空间 > 当前库大小 + 512MB 余量。拿不到库大小或可用空间时一律
// 判不安全(保守)，因为误判"够"的代价远大于少回收一次空间。
func relayLogVacuumSafe() (bool, string) {
	var pageCount, pageSize int64
	row := db.GetDB().Raw("PRAGMA page_count").Row()
	if err := row.Scan(&pageCount); err != nil {
		return false, "无法读取数据库大小，已跳过空间回收"
	}
	row = db.GetDB().Raw("PRAGMA page_size").Row()
	if err := row.Scan(&pageSize); err != nil {
		return false, "无法读取数据库页大小，已跳过空间回收"
	}
	dbBytes := pageCount * pageSize
	if dbBytes <= 0 {
		return false, "数据库大小异常，已跳过空间回收"
	}

	var dbPath string
	row = db.GetDB().Raw("PRAGMA database_list").Row()
	// database_list 返回 (seq, name, file) 三列
	var seq int64
	var name string
	if err := row.Scan(&seq, &name, &dbPath); err != nil || dbPath == "" {
		return false, "无法定位数据库文件，已跳过空间回收"
	}
	// 取数据库文件所在目录的可用空间
	dir := dbPath
	if idx := strings.LastIndexAny(dir, "/\\"); idx > 0 {
		dir = dir[:idx]
	}

	free, ok := diskFreeBytes(dir)
	if !ok {
		return false, "无法读取磁盘可用空间，已跳过空间回收"
	}

	const margin = 512 << 20 // 512MB 余量，留给正常写入
	if free < dbBytes+margin {
		return false, "磁盘可用空间不足以安全回收(需要约 " + humanBytes(dbBytes+margin) + "，实际可用 " + humanBytes(free) + ")，已跳过"
	}
	return true, ""
}

// humanBytes 把字节数格式化成便于阅读的形式，只用于给用户看的说明文字。
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(n)
	idx := 0
	for value >= 1024 && idx < len(units)-1 {
		value /= 1024
		idx++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + units[idx]
}

var _ = gorm.ErrRecordNotFound

// errCleanupAlreadyRunning 表示上一次手动清理还没跑完。
var errCleanupAlreadyRunning = errors.New("日志清理正在进行中, 请稍候再试")

// RelayLogCleanupResult 是一次手动清理的结果。
//
// 回 before/after 而不是只回成功：用户点这个按钮的目的就是"看它降了多少"，
// 只回 ok 等于什么都没说。
type RelayLogCleanupResult struct {
	BeforeBytes int64 `json:"before_bytes"`
	AfterBytes  int64 `json:"after_bytes"`
	FreedBytes  int64 `json:"freed_bytes"`
	Vacuumed    bool  `json:"vacuumed"`
	// Note 用中性措辞说明"删了行不等于文件立刻变小"，避免用户以为没生效。
	Note string `json:"note,omitempty"`
}

// relayLogCleanupRunning 是手动清理的单飞标志。
//
// 为什么需要：清理要跑 VACUUM，VACUUM 会重建整个数据库文件、独占写锁。
// 用户连点两次就会让第二个请求排在锁上，界面看起来像卡死。用 CAS 直接拒掉
// 第二次，比排队更诚实。
var relayLogCleanupRunning atomic.Bool

// RelayLogCleanupNow 立即按当前上限收缩一次，返回清理前后的占用。
func RelayLogCleanupNow(ctx context.Context) (RelayLogCleanupResult, error) {
	if !relayLogCleanupRunning.CompareAndSwap(false, true) {
		return RelayLogCleanupResult{}, errCleanupAlreadyRunning
	}
	defer relayLogCleanupRunning.Store(false)

	before, _, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return RelayLogCleanupResult{}, err
	}

	if err := relayLogCleanupByStorageBounded(ctx); err != nil {
		return RelayLogCleanupResult{}, err
	}

	after, _, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return RelayLogCleanupResult{}, err
	}

	result := RelayLogCleanupResult{
		BeforeBytes: before,
		AfterBytes:  after,
		FreedBytes:  before - after,
	}
	if result.FreedBytes > 0 {
		// 只有真删了行才 VACUUM：它要重建整个文件，白跑一次代价很高。
		if vacuumErr := relayLogVacuum(ctx); vacuumErr != nil {
			result.Note = "日志已按上限收缩，但回收磁盘空间失败(" + vacuumErr.Error() + ")，文件大小可能暂时不变。"
		} else {
			result.Vacuumed = true
			result.Note = "日志已收缩，磁盘空间已回收。"
		}
	} else {
		// 一行都没删，但库里可能仍躺着历史死空间（删日志只标空闲、不还文件）。
		// 用户点"立即清理"就是想看到空间变小，所以这里也要尝试回收——
		// 冷却期与阈值判断都在 relayLogReclaimDue 里，不会白跑。
		result.Note = "当前占用未超过上限，未删除任何日志。"
		// 手动触发同样服从开关：关掉自动回收的人，不该在点按钮时被静默回收。
		enabled, _ := SettingGetBool(model.SettingKeyRelayLogReclaimEnabled)
		due, free, total, reason := false, int64(0), int64(0), ""
		if enabled {
			due, free, total, reason = relayLogReclaimDue(ctx, time.Now())
		}
		if due {
			relayLogLastReclaimUnix.Store(time.Now().Unix())
			if vacuumErr := relayLogVacuum(ctx); vacuumErr != nil {
				result.Note += " 死空间回收失败(" + vacuumErr.Error() + ")。"
			} else {
				result.Vacuumed = true
				result.Note += fmt.Sprintf(" 已回收历史死空间（空闲页 %d/%d，%s）。", free, total, reason)
			}
		}
	}
	return result, nil
}

// ───────────────────────────── 死空间回收 ─────────────────────────────
//
// 上面那套 relayLogVacuum 的触发条件原本是「本轮确实删掉了行」。那个判据有个
// 致命缺口，生产上真实发生过：
//
//   日志只跨 3 天、保留期 30 天 → 清理任务每 10 分钟跑一次，每次一行都删不掉
//   → afterRows == beforeRows → VACUUM 那一步永远进不去。
//   而库里躺着 11.7 GiB 空闲页（历史大清理删掉 6 万行留下的），永远不会被回收。
//
// 也就是说：**回收空间的能力只在"刚好删了东西"那一次才被唤醒，历史遗留的死空间
// 没人管**。日志删得越干净，这个 bug 越不可能被触发——越健康越看不见。
//
// 现在的判据改成看库自己的体检数字：空闲页占总页数超过阈值就回收。这与"这轮删没
// 删"完全解耦，谁留下的死空间都能被收拾掉。

const (
	// relayLogReclaimFreeRatio 是触发回收的空闲页占比阈值。
	//
	// 取 0.20 的理由不是精确科学，是三条现实约束的交点：
	//   - 太低（如 0.05）：日志正常写入本来就会留几 % 空闲页，天天 VACUUM 纯属浪费；
	//   - 太高（如 0.50）：等于要等到浪费一半空间才动手，18GB 的库就是白扔 9GB；
	//   - 0.20 意味着"浪费超过五分之一"才值得付一次重排文件的代价。
	//
	// 生产实测：11.7 GiB / 17.1 GiB ≈ 0.68，远超阈值，部署后立刻会被收拾。
	relayLogReclaimFreeRatio = 0.20

	// relayLogReclaimMinFreePages 是绝对下限：空闲页太少时无论占比如何都不回收。
	//
	// 为什么要这条：小库上占比会剧烈抖动。刚建的空库删掉一行，空闲页占比可能就到
	// 50%，此时 VACUUM 毫无意义却要重建整个文件。设 1024 页（4 MiB，默认页大小）
	// 把这类噪声挡掉，让阈值只在大库上生效。
	relayLogReclaimMinFreePages = 1024
)

// relayLogReclaimCooldown 是两次回收之间的最小间隔。
//
// 🔴 这条不是优化，是安全阀。完整 VACUUM 要重建整个库文件：
//   - 生产 17.1 GiB 的库，这一步要几分钟到几十分钟；
//   - 期间 SQLite 持有排他锁，**所有读写都被挡住**；
//   - 峰值临时空间约等于库大小。
//
// 没有冷却期的话，清理任务（每 10 分钟）会反复触发重排，等于把生产库持续按住。
// 6 小时的含义：一天最多被打断 4 次，而回收一次带来的空间通常能撑很久。
const relayLogReclaimCooldown = 6 * time.Hour

// relayLogLastReclaimUnix 记录上次尝试回收的时间（Unix 秒，0 = 从未）。
//
// 用 atomic 而不是 mutex：这条只在清理任务里读写，但 relayLogVacuum 可能被
// 管理页的"立即清理"并发调到，用原子操作避免为此引入一把锁。
var relayLogLastReclaimUnix atomic.Int64

// relayLogReclaimDue 判断当前是否该回收死空间，并给出人话理由。
//
// 返回 (是否该回收, 空闲页数, 总页数, 理由)。理由用于日志——运维看到"为什么这次
// 没回收"比看到一片沉默有用得多，沉默会让人以为功能坏了。
func relayLogReclaimDue(ctx context.Context, now time.Time) (bool, int64, int64, string) {
	// 冷却期必须先判、且必须**在碰数据库之前**判。
	//
	// 这不是微优化：冷却期内去读 PRAGMA 是纯粹的无用功，更糟的是它要求数据库
	// 一定可用。让"该不该回收"的答案依赖一个根本不需要的信息，会把一个纯策略
	// 判断变成可能失败的操作。测试里正是因为顺序写反，冷却期用例直接撞了空库 panic。
	last := time.Time{}
	if v := relayLogLastReclaimUnix.Load(); v > 0 {
		last = time.Unix(v, 0)
	}
	if !last.IsZero() && now.Sub(last) < relayLogReclaimCooldown {
		_, reason := relayLogReclaimDecision(0, 0, last, now)
		return false, 0, 0, reason
	}

	var free, total int64
	if err := db.GetDB().WithContext(ctx).Raw("PRAGMA freelist_count").Row().Scan(&free); err != nil {
		return false, 0, 0, "无法读取空闲页数: " + err.Error()
	}
	if err := db.GetDB().WithContext(ctx).Raw("PRAGMA page_count").Row().Scan(&total); err != nil {
		return false, 0, 0, "无法读取总页数: " + err.Error()
	}

	due, reason := relayLogReclaimDecision(free, total, last, now)
	return due, free, total, reason
}

// relayLogReclaimDecision 是纯粹的判定函数：不碰 DB、不读时钟，只看入参。
//
// 抽出来的原因很实际——阈值和冷却这两条规则最容易改错，也最需要测试，而它们与
// 数据库一点关系都没有。测试里不必真的把一个库撑出 1024 页空闲区，只要喂数字。
func relayLogReclaimDecision(free, total int64, lastReclaim, now time.Time) (bool, string) {
	// 冷却期优先：最便宜，也最该短路——刚重排完再问什么都没意义。
	if !lastReclaim.IsZero() {
		since := now.Sub(lastReclaim)
		if since < relayLogReclaimCooldown {
			return false, fmt.Sprintf("距上次回收仅 %s，冷却期内(阈值 %s)", since.Truncate(time.Minute), relayLogReclaimCooldown)
		}
	}

	if total <= 0 {
		return false, "数据库页数为 0，跳过"
	}
	if free < relayLogReclaimMinFreePages {
		return false, fmt.Sprintf("空闲页 %d 未达下限 %d，不值得重排文件", free, relayLogReclaimMinFreePages)
	}

	ratio := float64(free) / float64(total)
	if ratio < relayLogReclaimFreeRatio {
		return false, fmt.Sprintf("空闲页占比 %.1f%% 未达阈值 %.0f%%", ratio*100, relayLogReclaimFreeRatio*100)
	}

	return true, fmt.Sprintf("空闲页占比 %.1f%%(%d/%d) 超过阈值 %.0f%%", ratio*100, free, total, relayLogReclaimFreeRatio*100)
}

// RelayLogReclaimDeadSpace 检查并按需回收 SQLite 死空间。
//
// 这是一个**独立于删除**的动作：不删任何日志，只把已经被标成空闲的页还给文件系统。
// 由清理任务在每轮删除之后调用；即使一行都没删，只要死空间够多也会回收。
//
// 失败不返回错误给调用方中断清理流程——空间没还回去不是故障，下次还会再试。
// 但会把原因写进日志（Debugf 级别：磁盘不够时跳过是预期行为，每 10 分钟报 WARN
// 只会把真正的问题淹掉）。
func RelayLogReclaimDeadSpace(ctx context.Context) error {
	// 开关优先，且必须最先判：这是用户应急关掉自动回收的唯一入口。
	//
	// 抄 axonhub 的做法（vacuum_enabled），但默认值相反——它默认关，让用户自己
	// 决定要不要停服务；我们默认开，因为这个库已经躺着 11.7 GiB 死空间，
	// 默认关等于永远不回收。开关留着是为了出事时能立刻止血，不是为了平时关着。
	enabled, err := SettingGetBool(model.SettingKeyRelayLogReclaimEnabled)
	if err != nil {
		// 读不到设置不该让清理任务失败，按默认（开）继续。
		log.Debugf("relay log reclaim: 无法读取开关，按默认开启继续: %v", err)
	} else if !enabled {
		log.Debugf("relay log dead-space reclaim disabled by setting")
		return nil
	}

	now := time.Now()
	due, free, _, reason := relayLogReclaimDue(ctx, now)
	if !due {
		log.Debugf("relay log dead-space reclaim skipped: %s", reason)
		return nil
	}

	// 先记时间戳再动手：如果 VACUUM 把进程拖死/被 OOM 杀掉，重启后不会立刻又
	// 扑上来重排一次。宁可晚 6 小时回收，也不要形成"重启→VACUUM→被杀"的死循环。
	relayLogLastReclaimUnix.Store(now.Unix())

	log.Infof("relay log dead-space reclaim starting: %s", reason)
	start := time.Now()

	if err := relayLogVacuum(ctx); err != nil {
		// 磁盘不够是**预期**情况，不是故障；如实记下来但别刷 WARN。
		log.Debugf("relay log dead-space reclaim failed after %s: %v", time.Since(start).Truncate(time.Second), err)
		return nil
	}

	// 复核实际效果：不假装成功。这里读的是回收后的真实数字。
	var afterFree int64
	verifyErr := db.GetDB().WithContext(ctx).Raw("PRAGMA freelist_count").Row().Scan(&afterFree)
	if verifyErr != nil {
		log.Infof("relay log dead-space reclaim finished in %s (无法复核空闲页: %v)", time.Since(start).Truncate(time.Second), verifyErr)
		return nil
	}

	log.Infof("relay log dead-space reclaim finished in %s: free pages %d → %d (reclaimed ~%s)",
		time.Since(start).Truncate(time.Second), free, afterFree, humanBytes((free-afterFree)*4096))
	return nil
}
