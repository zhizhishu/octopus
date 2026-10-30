package op

// 管理员手动「压缩数据库」。
//
// 背景：删日志只是把页标成空闲，SQLite 文件不会自己变小。自动回收
// (RelayLogReclaimDeadSpace) 有阈值和 6 小时冷却，管理员想"现在就把空间要回来"
// 时没有直接入口——只能去点"立即清理"，而那个按钮只在超上限或死空间够多时才回收。
//
// 做法照抄 axonhub(internal/server/gc/gc.go 的 runVacuum / RunVacuumNow)：
//   - 只对 SQLite 执行 VACUUM；
//   - MySQL / PostgreSQL 不做（axonhub 对 MySQL 直接跳过；PostgreSQL 有 autovacuum，
//     能真正缩小文件的 VACUUM FULL 会锁全表，不适合放成一个按钮），界面上隐藏按钮；
//   - 手动入口与自动回收共用同一把锁：任一 VACUUM 在跑时，另一路直接拒绝，不排队。
//
// 安全护栏沿用 relayLogVacuum：已是增量模式就只做 incremental_vacuum（便宜）；
// 否则完整 VACUUM 前检查磁盘可用空间 > 库大小 + 512MB，不够就拒绝。

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// dbVacuumMu 串行化所有会重排 SQLite 文件的动作（手动压缩、日志清理后的回收、
// 死空间自动回收）。只用 TryLock：VACUUM 期间库被独占，排队等下去只会让请求
// 卡更久，直接拒绝更诚实。
var dbVacuumMu sync.Mutex

// ErrDBCompactBusy 表示已有一次压缩/回收在进行。
var ErrDBCompactBusy = errors.New("数据库正在整理中，请稍后再试")

// ErrDBCompactUnsupported 表示当前数据库类型不提供手动压缩。
var ErrDBCompactUnsupported = errors.New("当前数据库类型不支持手动压缩（仅 SQLite）")

// DBCompactInfo 是压缩按钮旁边显示的只读信息。
type DBCompactInfo struct {
	Dialect   string `json:"dialect"`
	Supported bool   `json:"supported"`
	// FileBytes 是库文件在磁盘上的实际大小（主文件 + -wal + -shm）。
	FileBytes int64 `json:"file_bytes"`
	// FreeBytes 是库内空闲页的大小，即压缩后大约能还给磁盘的量。
	FreeBytes int64 `json:"free_bytes"`
	Running   bool  `json:"running"`
	// NeedsFull 为 true 表示库还不是增量回收模式，下一次压缩要完整 VACUUM（重建整个文件，
	// 库越大停顿越久）；false 时只做 incremental_vacuum，通常很快。
	NeedsFull bool `json:"needs_full"`
}

// DBCompactResult 是一次手动压缩的结果。
type DBCompactResult struct {
	BeforeBytes int64  `json:"before_bytes"`
	AfterBytes  int64  `json:"after_bytes"`
	FreedBytes  int64  `json:"freed_bytes"`
	DurationMs  int64  `json:"duration_ms"`
	Mode        string `json:"mode"` // incremental | full
}

func dbDialectName() string {
	conn := db.GetDB()
	if conn == nil || conn.Dialector == nil {
		return ""
	}
	return conn.Dialector.Name()
}

// dbCompactSupported 只认 SQLite。拆成纯函数是为了不起 MySQL/PG 也能测到分支。
func dbCompactSupported(dialect string) bool {
	return dialect == "sqlite"
}

// sqliteMainPath 返回主库文件路径；内存库/取不到时返回空串。
func sqliteMainPath(ctx context.Context) string {
	rows, err := db.GetDB().WithContext(ctx).Raw("PRAGMA database_list").Rows()
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return ""
		}
		if name == "main" {
			return file
		}
	}
	return ""
}

// sqliteFileBytes 读磁盘上的真实占用。WAL 模式下 VACUUM 的内容先写进 -wal，
// 只看主文件会漏掉这一块，所以三个文件一起算。读不到文件时退回页数 × 页大小。
func sqliteFileBytes(ctx context.Context) int64 {
	if path := sqliteMainPath(ctx); path != "" {
		var total int64
		found := false
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if st, err := os.Stat(path + suffix); err == nil {
				total += st.Size()
				found = true
			}
		}
		if found {
			return total
		}
	}
	var pageCount, pageSize int64
	_ = db.GetDB().WithContext(ctx).Raw("PRAGMA page_count").Row().Scan(&pageCount)
	_ = db.GetDB().WithContext(ctx).Raw("PRAGMA page_size").Row().Scan(&pageSize)
	return pageCount * pageSize
}

func sqliteFreeBytes(ctx context.Context) int64 {
	var free, pageSize int64
	if err := db.GetDB().WithContext(ctx).Raw("PRAGMA freelist_count").Row().Scan(&free); err != nil {
		return 0
	}
	if err := db.GetDB().WithContext(ctx).Raw("PRAGMA page_size").Row().Scan(&pageSize); err != nil {
		return 0
	}
	return free * pageSize
}

// DBCompactInfoGet 返回当前库的大小与可回收量。非 SQLite 只回 dialect + supported=false。
func DBCompactInfoGet(ctx context.Context) DBCompactInfo {
	dialect := dbDialectName()
	info := DBCompactInfo{Dialect: dialect, Supported: dbCompactSupported(dialect)}
	if !info.Supported {
		return info
	}
	if dbVacuumMu.TryLock() {
		dbVacuumMu.Unlock()
	} else {
		info.Running = true
	}
	info.FileBytes = sqliteFileBytes(ctx)
	info.FreeBytes = sqliteFreeBytes(ctx)
	var mode int64
	if err := db.GetDB().WithContext(ctx).Raw("PRAGMA auto_vacuum").Row().Scan(&mode); err != nil || mode != 2 {
		info.NeedsFull = true
	}
	return info
}

// DBCompactNow 立即压缩一次 SQLite 库，返回压缩前后的磁盘占用。
func DBCompactNow(ctx context.Context) (DBCompactResult, error) {
	if !dbCompactSupported(dbDialectName()) {
		return DBCompactResult{}, ErrDBCompactUnsupported
	}
	if !dbVacuumMu.TryLock() {
		return DBCompactResult{}, ErrDBCompactBusy
	}
	defer dbVacuumMu.Unlock()

	start := time.Now()
	before := sqliteFileBytes(ctx)
	mode, err := sqliteVacuumLocked(ctx)
	if err != nil {
		return DBCompactResult{}, err
	}
	// WAL 模式下重排后的内容还在 -wal 里，主文件要等检查点才截短，-wal 本身也不会自己变小。
	// 手动压缩的意义就是"现在看到变小"，所以这里做一次 TRUNCATE 检查点。失败不算压缩失败。
	if err := db.GetDB().WithContext(ctx).Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		log.Debugf("db compact: wal checkpoint failed: %v", err)
	}
	// 自动回收的冷却期也从这里重新计时：刚手动压过，没必要几分钟后又自动重排一次。
	relayLogLastReclaimUnix.Store(time.Now().Unix())

	after := sqliteFileBytes(ctx)
	result := DBCompactResult{
		BeforeBytes: before,
		AfterBytes:  after,
		FreedBytes:  before - after,
		DurationMs:  time.Since(start).Milliseconds(),
		Mode:        mode,
	}
	if result.FreedBytes < 0 {
		result.FreedBytes = 0
	}
	log.Infof("db compact (%s) finished in %s: %s → %s", mode, time.Since(start).Truncate(time.Millisecond), humanBytes(before), humanBytes(after))
	return result, nil
}
