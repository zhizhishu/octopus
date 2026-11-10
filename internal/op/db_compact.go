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
	FileBytes  int64  `json:"file_bytes"`
	MainBytes  int64  `json:"main_bytes"`
	WALBytes   int64  `json:"wal_bytes"`
	SHMBytes   int64  `json:"shm_bytes"`
	StatsValid bool   `json:"stats_valid"`
	ErrorCode  string `json:"error_code,omitempty"`
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
	// CheckpointStatus describes WAL release independently of completed vacuum work.
	CheckpointStatus    string `json:"checkpoint_status"` // truncated | not_needed | busy | skipped | error
	CheckpointErrorCode string `json:"checkpoint_error_code,omitempty"`
	StatsValid          bool   `json:"stats_valid"`
	ErrorCode           string `json:"error_code,omitempty"`
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
	statsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var pageSize, free, mode int64
	conn := db.GetDB().WithContext(statsCtx)
	var path string
	if err := conn.Raw("SELECT file FROM pragma_database_list WHERE name = 'main'").Row().Scan(&path); err != nil {
		info.ErrorCode = dbStatsErrorCode(err)
		return info
	}
	if path == "" {
		var pages int64
		if err := conn.Raw("PRAGMA page_count").Row().Scan(&pages); err != nil {
			info.ErrorCode = dbStatsErrorCode(err)
			return info
		}
		info.MainBytes = pages
	} else {
		for _, file := range []struct {
			suffix string
			value  *int64
		}{
			{"", &info.MainBytes}, {"-wal", &info.WALBytes}, {"-shm", &info.SHMBytes},
		} {
			st, err := os.Stat(path + file.suffix)
			if err != nil {
				if file.suffix != "" && os.IsNotExist(err) {
					continue
				}
				info.ErrorCode = "file_unavailable"
				return info
			}
			*file.value = st.Size()
		}
	}
	info.FileBytes = info.MainBytes + info.WALBytes + info.SHMBytes
	for _, query := range []struct {
		sql   string
		value *int64
	}{
		{"PRAGMA page_size", &pageSize}, {"PRAGMA freelist_count", &free}, {"PRAGMA auto_vacuum", &mode},
	} {
		if err := conn.Raw(query.sql).Row().Scan(query.value); err != nil {
			info.ErrorCode = dbStatsErrorCode(err)
			return info
		}
	}
	if pageSize <= 0 || free < 0 {
		info.ErrorCode = "invalid_stats"
		return info
	}
	info.FreeBytes = free * pageSize
	info.NeedsFull = mode != 2
	if path == "" {
		info.MainBytes *= pageSize
		info.FileBytes = info.MainBytes
	}
	info.StatsValid = true
	return info
}

func dbStatsErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case isSQLiteBusyErr(err):
		return "busy"
	default:
		return "database_unavailable"
	}
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
	before := DBCompactInfoGet(ctx)
	if !before.StatsValid {
		return DBCompactResult{}, errors.New("database size unavailable: " + before.ErrorCode)
	}
	mode, err := sqliteVacuumLocked(ctx)
	if err != nil {
		return DBCompactResult{}, err
	}
	// Vacuum completion and WAL release are separate outcomes.
	checkpoint, checkpointErr := sqliteWALCheckpointLocked(ctx, true)
	if checkpointErr != nil {
		log.Debugf("db compact: wal checkpoint failed: %v", checkpointErr)
	} else {
		logSQLiteWALCheckpoint("compact", checkpoint)
	}
	// 自动回收的冷却期也从这里重新计时：刚手动压过，没必要几分钟后又自动重排一次。
	relayLogLastReclaimUnix.Store(time.Now().Unix())

	after := DBCompactInfoGet(ctx)
	result := DBCompactResult{
		BeforeBytes: before.FileBytes,
		AfterBytes:  after.FileBytes,
		DurationMs:  time.Since(start).Milliseconds(),
		Mode:        mode,
		StatsValid:  after.StatsValid,
		ErrorCode:   after.ErrorCode,
	}
	switch {
	case checkpointErr != nil:
		result.CheckpointStatus = "error"
		result.CheckpointErrorCode = dbStatsErrorCode(checkpointErr)
	case checkpoint.Busy:
		result.CheckpointStatus = "busy"
	case checkpoint.Skipped == "no wal file" || checkpoint.Skipped == "no database file":
		result.CheckpointStatus = "not_needed"
	case checkpoint.Skipped != "" || checkpoint.Mode != "truncate":
		result.CheckpointStatus = "skipped"
	default:
		result.CheckpointStatus = "truncated"
	}
	if result.StatsValid && before.FileBytes > after.FileBytes {
		result.FreedBytes = before.FileBytes - after.FileBytes
	}
	log.Infof("db compact (%s) finished in %s: checkpoint=%s stats_valid=%v freed_bytes=%d", mode, time.Since(start).Truncate(time.Millisecond), result.CheckpointStatus, result.StatsValid, result.FreedBytes)
	return result, nil
}
