package op

// SQLite WAL 周期检查点：让被撑大过的 -wal 文件能自己缩回去。
//
// 背景（生产实测）：data.db 22.8G，data.db-wal 长期停在 9.48 GB。WAL 只在两种情况下变小：
//   1. 连接设了 journal_size_limit，WAL 回绕时超出上限的部分被截掉——以前没设（默认不限）；
//   2. 有人跑 wal_checkpoint(TRUNCATE)——以前只有管理员手动「压缩数据库」会跑。
// 自动检查点（wal_autocheckpoint，默认 1000 页）永远是 PASSIVE：它会把帧搬回主库、让 WAL
// 从头复用，但从不截短文件。于是只要启动回填/迁移或日志全表扫描期间有长读者挡住回绕、
// 把 WAL 撑大一次，这块空间就永久占着。
//
// 做法（调研结论见 internal/op/sqlite_wal_checkpoint.md）：
//   - DSN 里设 journal_size_limit（db.SQLiteJournalSizeLimitBytes），平时靠它在回绕时截短；
//   - 这里每 5 分钟跑一次：先 PASSIVE（永不等待、永不挡写者），只有 -wal 文件超过阈值、
//     且 PASSIVE 已经把**全部**帧搬回主库时，才在借来的连接上把 busy_timeout 临时降到
//     200ms 再试 TRUNCATE。拿不到锁/有读者就放弃，下一轮再来。
//
// 为什么 TRUNCATE 之前必须先 PASSIVE 且要求"全部搬完"：TRUNCATE 与 RESTART 一样会先抢写锁，
// 然后在持有写锁的情况下把剩余帧拷回主库、再等读者离开。剩余帧如果有几个 GB，写锁就会
// 被按住几十秒，所有请求的日志/统计写入都会排队直到 busy_timeout 报错。PASSIVE 先把能搬
// 的搬完（不持写锁），TRUNCATE 那一步就只剩"等读者 + ftruncate"，写锁持有时间被 200ms
// 的 busy 预算封顶。这与 Litestream v0.5.15 的 passive-first 与 rqlite 的"短 busy 超时
// 再 TRUNCATE"是同一个思路。
//
// 与 VACUUM 互斥：持有 dbVacuumMu（TryLock，抢不到就跳过本轮），不会与手动压缩/自动回收重叠。
// 只对 SQLite 生效；MySQL/PostgreSQL 直接返回。

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

const (
	// sqliteWALCheckpointBusyTimeoutMs 是 TRUNCATE 那一步在借来的连接上使用的 busy_timeout。
	// TRUNCATE 等待期间持有写锁，请求方的写入会跟着等；200ms 是"读者通常能在这段时间里结束"
	// 与"请求写入多等 200ms 无感"的折中（rqlite 用 250ms，同一量级）。
	sqliteWALCheckpointBusyTimeoutMs = 200
)

// sqliteWALTruncateThresholdBytes：-wal 文件超过它才尝试 TRUNCATE，取 journal_size_limit 的两倍
// （256 MiB）。低于它的残余交给 journal_size_limit 在下一次回绕时截短；超过它说明 WAL 被
// 撑大且一直没缩回来（例如部署前就已是 9.5G），值得主动截一次。变量而非常量，仅为测试可调。
var sqliteWALTruncateThresholdBytes int64 = 2 * db.SQLiteJournalSizeLimitBytes

// sqliteWALCheckpointResult 是一次检查点的结果，主要给日志和测试用。
type sqliteWALCheckpointResult struct {
	// Skipped 非空表示本轮什么都没做以及原因（非 SQLite、正在 VACUUM、内存库、没有 -wal）。
	Skipped string
	// Mode 是实际执行到的最后一步："passive" 或 "truncate"。
	Mode string
	// Busy 表示想截短但没截成（有读者/写者，或 PASSIVE 没搬完），下一轮再试。
	Busy bool
	// LogFrames / CheckpointedFrames 是 PASSIVE 返回的 WAL 帧数与已搬回主库的帧数。
	LogFrames          int64
	CheckpointedFrames int64
	BeforeBytes        int64
	AfterBytes         int64
}

// SQLiteWALCheckpointTask 是周期任务入口（见 task.TaskSQLiteWALCheckpoint）。
func SQLiteWALCheckpointTask(ctx context.Context) {
	res, err := sqliteWALCheckpoint(ctx, false)
	if err != nil {
		log.Debugf("sqlite wal checkpoint failed: %v", err)
		return
	}
	logSQLiteWALCheckpoint("periodic", res)
}

// sqliteWALCheckpoint 抢 dbVacuumMu 后执行检查点；抢不到说明 VACUUM/压缩正在跑，直接跳过。
// forceTruncate=true 时只要 -wal 非空就尝试截短（回收/VACUUM 之后用），否则只在超过阈值时截短。
func sqliteWALCheckpoint(ctx context.Context, forceTruncate bool) (sqliteWALCheckpointResult, error) {
	if !dbCompactSupported(dbDialectName()) {
		return sqliteWALCheckpointResult{Skipped: "not sqlite"}, nil
	}
	if !dbVacuumMu.TryLock() {
		return sqliteWALCheckpointResult{Skipped: "vacuum running"}, nil
	}
	defer dbVacuumMu.Unlock()
	return sqliteWALCheckpointLocked(ctx, forceTruncate)
}

// sqliteWALCheckpointLocked 是检查点本体，调用方必须已持有 dbVacuumMu（或确知没有 VACUUM 在跑）。
func sqliteWALCheckpointLocked(ctx context.Context, forceTruncate bool) (res sqliteWALCheckpointResult, err error) {
	path := sqliteMainPath(ctx)
	if path == "" {
		res.Skipped = "no database file"
		return res, nil
	}
	walPath := path + "-wal"
	res.BeforeBytes = fileSizeOrZero(walPath)
	res.AfterBytes = res.BeforeBytes
	if res.BeforeBytes == 0 {
		res.Skipped = "no wal file"
		return res, nil
	}

	sqlDB, err := db.GetDB().DB()
	if err != nil {
		return res, err
	}
	// 钉住一条连接：busy_timeout 是连接级设置，调低和恢复必须落在同一条连接上。
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return res, err
	}
	lowered := false
	defer func() {
		if lowered {
			// 恢复默认 busy_timeout 再还回连接池。不用调用方的 ctx：它可能已超时，
			// 而"恢复失败还把短超时连接放回池子"会让之后的普通写入 200ms 就报 database is locked。
			restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, rerr := conn.ExecContext(restoreCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", db.SQLiteBusyTimeoutMs))
			cancel()
			if rerr != nil {
				// 恢复不了就把这条连接作废（ErrBadConn 会让 database/sql 关闭而不是复用它）。
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				log.Warnf("sqlite wal checkpoint: restore busy_timeout failed, connection discarded: %v", rerr)
			}
		}
		_ = conn.Close()
		res.AfterBytes = fileSizeOrZero(walPath)
	}()

	// 第一步 PASSIVE：不调用 busy handler、不等任何读者写者、不拿写锁，能搬多少搬多少。
	var busy int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").
		Scan(&busy, &res.LogFrames, &res.CheckpointedFrames); err != nil {
		return res, err
	}
	res.Mode = "passive"

	wantTruncate := res.BeforeBytes >= sqliteWALTruncateThresholdBytes || forceTruncate
	if !wantTruncate {
		return res, nil
	}
	// PASSIVE 没搬完：有读者还停在旧快照上。此时 TRUNCATE 要在持写锁的状态下搬剩下的帧并等
	// 那个读者——正是会把请求写入卡住的情形。不试，下一轮再来。
	if res.LogFrames < 0 || res.CheckpointedFrames < res.LogFrames {
		res.Busy = true
		return res, nil
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteWALCheckpointBusyTimeoutMs)); err != nil {
		return res, err
	}
	lowered = true
	var logFrames, ckptFrames int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
		Scan(&busy, &logFrames, &ckptFrames); err != nil {
		// SQLITE_BUSY 一般以 busy=1 的结果行返回；个别路径会以错误返回，同样视为"忙，下轮再试"。
		if isSQLiteBusyErr(err) {
			res.Mode = "truncate"
			res.Busy = true
			return res, nil
		}
		return res, err
	}
	res.Mode = "truncate"
	res.Busy = busy != 0
	return res, nil
}

// logSQLiteWALCheckpoint：平时 Debug；真把一个大 WAL 截小了才 Info，方便部署后确认 9.5G 缩回来了。
func logSQLiteWALCheckpoint(trigger string, res sqliteWALCheckpointResult) {
	if res.Skipped != "" {
		log.Debugf("sqlite wal checkpoint (%s) skipped: %s", trigger, res.Skipped)
		return
	}
	shrunk := res.BeforeBytes - res.AfterBytes
	if res.Mode == "truncate" && !res.Busy && shrunk >= db.SQLiteJournalSizeLimitBytes {
		log.Infof("sqlite wal checkpoint (%s): wal file %s → %s", trigger, humanBytes(res.BeforeBytes), humanBytes(res.AfterBytes))
		return
	}
	log.Debugf("sqlite wal checkpoint (%s) mode=%s busy=%v frames=%d/%d wal %s → %s",
		trigger, res.Mode, res.Busy, res.CheckpointedFrames, res.LogFrames, humanBytes(res.BeforeBytes), humanBytes(res.AfterBytes))
}

func fileSizeOrZero(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// isSQLiteBusyErr 判断是否是 SQLITE_BUSY / SQLITE_LOCKED。驱动错误类型不在本包的依赖面上，
// 按错误码文本判断即可（modernc 的错误串形如 "database is locked (5) (SQLITE_BUSY)"）。
func isSQLiteBusyErr(err error) bool {
	if err == nil {
		return false
	}
	var target interface{ Code() int }
	if errors.As(err, &target) {
		switch target.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return true
		}
	}
	msg := err.Error()
	for _, sub := range []string{"SQLITE_BUSY", "SQLITE_LOCKED", "database is locked"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
