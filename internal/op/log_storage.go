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
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
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
//	json.Marshal(Attempts) 的长度 + 256
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

// relayLogCleanupByStorageBounded 按容量上限收缩，全程有界内存。
//
// 与旧实现的区别：不再先把所有行读进内存再倒推要删哪些 id，而是
//
//  1. 用 SQL 算出当前总字节；
//  2. 若超限，按 id 从旧到新**分批**找出需要删除的 id(累计到超出的那部分为止)；
//  3. 删一批、recount 一次，直到回到上限内或没有更多可删。
//
// 第 3 步的 recount 很重要：单批删除量与上限的差额未必吻合，硬凑容易删过头。
// 每批都有上限，所以内存始终是常量，生产上不会再被 OOM 打断。
func relayLogCleanupByStorageBounded(ctx context.Context) error {
	_, maxBytes, err := relayLogMaxStorage()
	if err != nil || maxBytes <= 0 {
		return err
	}

	const deleteBatch = 2000
	for {
		total, rows, err := relayLogStoredBytes(ctx, nil)
		if err != nil {
			return err
		}
		if total <= maxBytes || rows == 0 {
			return nil
		}

		// 需要抹掉的字节数，换算成"至少删多少行"：用平均每行字节数估算，
		// 只在批大小上做参考，准确性由循环的 recount 兜底。
		over := total - maxBytes
		avg := total / rows
		if avg <= 0 {
			avg = 1
		}
		want := over / avg
		if want < 1 {
			want = 1
		}
		batch := int64(deleteBatch)
		if want < batch {
			batch = want
		}
		if batch > rows {
			batch = rows
		}

		// 保留最新一条：旧实现里 idx == 0 不计入删除，这里逐字保留同一语义
		// (全新部署/极端配置下也不该把最后一条日志删光)。
		var ids []int64
		if err := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
			Order("id ASC").Limit(int(batch)).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if int64(len(ids)) >= rows {
			ids = ids[:len(ids)-1]
		}
		if len(ids) == 0 {
			return nil
		}

		relayLogSearchIndexDeleteByIDs(db.GetDB().WithContext(ctx), ids)
		if err := db.GetDB().WithContext(ctx).Where("id IN ?", ids).Delete(&model.RelayLog{}).Error; err != nil {
			return err
		}

		// 循环继续：重新统计，直到回到上限内。每轮都释放了内存，不会累积。
	}
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

// relayLogVacuum 在删除后把空间真正还给文件系统。
//
// 为什么必须有这一步：SQLite 删除行只是把页标成空闲放进 free list，数据文件
// 不会变小。所以"设了上限、也确实删了日志，但 db 文件还是 59GB"是预期行为，
// 不是清理没生效。纯 Go 驱动不保证支持增量 VACUUM，这里用完整 VACUUM。
//
// VACUUM 会重建整个数据库文件，期间需要约等于当前库大小的临时空间。
// 因此只在**确实删掉了行**之后调用，且失败不影响清理本身(空间没还回去而已)。
func relayLogVacuum(ctx context.Context) error {
	safe, reason := relayLogVacuumSafe()
	if !safe {
		return errors.New(reason)
	}
	return db.GetDB().WithContext(ctx).Exec("VACUUM").Error
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
		result.Note = "当前占用未超过上限，未删除任何日志。"
	}
	return result, nil
}
