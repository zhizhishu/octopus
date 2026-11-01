package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var db *gorm.DB

func InitDB(dbType, dsn string, debug bool) error {
	var err error
	// SkipDefaultTransaction: GORM otherwise wraps every single Create/Update/Delete in
	// its own BEGIN...COMMIT. On SQLite (single-writer) with a large connection pool this
	// makes concurrent per-request writes (relay log / stats / ip / usage / session) all
	// issue BEGIN at once, which collides as "cannot start a transaction within a
	// transaction" and holds the write lock long enough to cascade into database-is-locked
	// (SQLITE_BUSY) across every log/stats write AND the log-list reads. Single statements
	// are already atomic, so the wrapper buys nothing here; skipping it turns each write
	// into one short bare statement. Explicit Transaction()/Begin() calls (admin CRUD) keep
	// their own transactions and are unaffected. Harmless for MySQL/Postgres.
	// CreateBatchSize caps how many rows GORM binds per INSERT on a batch Create. The
	// async relay-log flusher can hand Create() the whole pending queue (up to 500 rows);
	// RelayLog has ~60 columns, so an unbounded batch would bind ~30k parameters — perilously
	// close to SQLite's default 32766 bound-variable cap, i.e. one poison-scale flush away
	// from every full-queue write failing with "too many SQL variables" exactly when the DB
	// is already behind. Splitting at 100 rows/statement keeps every INSERT well under the cap
	// (and under MySQL's max_allowed_packet) with no behaviour change to callers.
	gormConfig := gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true, CreateBatchSize: 100}
	if debug {
		gormConfig.Logger = logger.Default.LogMode(logger.Info)
	}

	switch dbType {
	case "sqlite":
		db, err = initSQLite(dsn, &gormConfig)
	case "mysql":
		db, err = initMySQL(dsn, &gormConfig)
	case "postgres", "postgresql":
		db, err = initPostgres(dsn, &gormConfig)
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}

	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)
	sqlDB.SetConnMaxIdleTime(10 * time.Minute)

	if err := migrate.BeforeAutoMigrate(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.UserCheckIn{},
		&model.UserBalanceLog{},
		&model.UserAccessSeen{},
		// 客户端指纹：新表只走这份名单（不另写一次性迁移，理由见下面检测表的注释）。
		&model.UserClientFingerprint{},
		&model.Channel{},
		&model.ChannelKey{},
		&model.FingerprintProfile{},
		&model.AccessRouteProfile{},
		&model.AccessRouteRule{},
		&model.AccessRouteTarget{},
		&model.AccessBillingProfile{},
		&model.AccessBillingModelRule{},
		&model.AccessPlan{},
		&model.APIKeyAccessPlan{},
		&model.UserAccessPlan{},
		&model.LLMInfo{},
		&model.PriceSyncRun{},
		&model.APIKey{},
		&model.RedeemCode{},
		&model.Setting{},
		&model.StatsTotal{},
		&model.StatsDaily{},
		&model.StatsHourly{},
		&model.StatsModel{},
		&model.StatsChannel{},
		&model.StatsAPIKey{},
		&model.RelayLog{},
		&model.RelayLogImage{},
		// 🔴 日志搜索投影表与它的回填标记同样必须在这份名单里，理由与下面的检测表相同：
		// 只靠 migrate/014 建表，等于把"表在不在"押在一次性迁移的执行历史上。
		// 名单里有它，任何老库（014 记成已执行而表不在、表被删过、标记表是旧结构）
		// 每次启动都会先把结构对齐；表是空的由 014 发现并从头回填（分批、有预算、
		// 剩下的交给后台循环，见 migrate/014.go 与 op.RelayLogSearchBackfillStart）。
		&model.RelayLogSearchIndex{},
		&migrate.RelayLogSearchBackfillState{},
		// 🔴 检测任务/结果必须留在这份名单里，不能只靠 migrate/015 建表。
		//
		// 015 是一次性迁移：它建表时用的是**当时**的结构，跑完就记成"已执行"，
		// 之后再也不会跑第二遍。于是给 ModelAuditResult 加字段（attempt /
		// next_retry_at）时，新装的库和 CI 都正常（从零建表，字段齐），
		// 而已经在跑的生产库那张表还是旧结构 → 每次入队 INSERT 都报
		// "table model_audit_results has no column named attempt"，
		// 任务建出来了、结果行一条也写不进去，队列永远空转。
		// 生产上真栽过这一次（2026-10-27）。放在这里，每次启动都会对齐结构。
		&model.ModelAuditJob{},
		&model.ModelAuditResult{},
		// 分词器可信基线：同理只走这份名单，不另写一次性迁移。
		&model.TokenizerBaseline{},
		&model.ResponseSession{},
		&model.VideoTask{},
		&model.MidjourneyTask{},
		&model.ImageTask{},
		&model.BatchImageJob{},
		&model.BatchImageItem{},
		&migrate.MigrationRecord{},
	); err != nil {
		return err
	}
	// 搜索投影表的"已补齐"标记要在任何迁移/搜索读它之前核对一次: 表若是刚被上面的
	// AutoMigrate 重新建出来的空表, 旧标记的 complete=true 就是假的。失败只记日志——
	// 核对不了时 014 与后台回填还会再核对, 不该为此挡住启动。
	if _, err := migrate.ReconcileRelayLogSearchBackfillMarker(db); err != nil {
		log.Warnf("relay_log_search marker check failed: %v", err)
	}
	if err := migrate.AfterAutoMigrate(db); err != nil {
		return err
	}
	// Postgres: schema changes during migrations can invalidate cached prepared plans
	// (e.g. "cached plan must not change result type"). Clear them.
	if db.Dialector != nil && db.Dialector.Name() == "postgres" {
		db.Exec("DEALLOCATE ALL")
		db.Exec("DISCARD ALL")
	}
	return nil
}

// SQLiteBusyTimeoutMs 是每条 SQLite 连接的默认 busy_timeout（DSN 里设置）。
// 后台 WAL 检查点会在借来的连接上临时调低它，用完必须恢复成这个值再还回连接池。
const SQLiteBusyTimeoutMs = 5000

// SQLiteJournalSizeLimitBytes 是 PRAGMA journal_size_limit 的取值：128 MiB。
//
// 为什么必须设：WAL 模式下 SQLite 检查点之后**不会**截短 -wal 文件，而是从头覆盖复用
// （覆盖比追加快）。默认值 -1 = 不限，所以 -wal 只要被撑大过一次（启动回填/迁移期间有
// 长读者挡住检查点回绕、日志全表扫描等），就会永远停在历史最高水位——生产上 data.db-wal
// 一直是 9.48 GB 就是这样来的。设了上限后，每次 WAL 回绕（reset）或提交时，SQLite 会
// 把超出上限的部分截掉。
//
// 为什么是 128 MiB（不是 0，也不是更大）：
//   - wal_autocheckpoint 保持默认 1000 页（4 KiB 页 ≈ 4 MiB），正常写入下 WAL 只有几 MiB，
//     128 MiB 对稳态没有任何影响；
//   - 设成 0 会让每次回绕都把文件截到最小，下一批写入再重新扩展文件——日志写入密集的
//     库上是白白的 ftruncate + 扩容 I/O；留 128 MiB 余量吸收一次批量回填/incremental_vacuum
//     的突发，不必反复伸缩；
//   - 同类项目的取值都在这个量级：PocketBase 200 MB，Litestream 的紧急截断阈值 ~500 MB；
//     本库日志体量大、磁盘曾被挤满过，取偏小的 128 MiB，把 WAL 的"常驻占用"压在主库的
//     千分之几以内。
//
// 注意它只在 WAL 回绕时生效：有长读者让 WAL 一直回绕不了时它也无能为力，
// 那种情况由 op 包的周期 WAL 检查点任务（TRUNCATE，短超时、忙就跳过）兜底。
const SQLiteJournalSizeLimitBytes = 128 << 20

func initSQLite(path string, config *gorm.Config) (*gorm.DB, error) {
	// The driver is glebarez/sqlite (modernc, pure-Go), NOT mattn/go-sqlite3. modernc only
	// honours the "_pragma=name(value)" DSN dialect — it silently ignores the mattn-style
	// "_journal_mode=WAL" / "_synchronous=NORMAL" keys (url.ParseQuery keeps them but the
	// driver's applyQueryParams only reads q["_pragma"]). This file used the mattn dialect,
	// so for a long time ONLY busy_timeout was actually in effect (the driver hard-codes that
	// one default) and WAL was NEVER on — the DB ran in rollback-journal mode where a writer
	// blocks every reader and vice-versa, serialising the whole request hot path. Switch to
	// the dialect the driver reads. (internal/migration/newapi already used the correct form.)
	//
	//   journal_mode(WAL): readers stop blocking the writer — the point of this fix.
	//   synchronous(NORMAL): safe under WAL, far fewer fsyncs than the FULL default.
	//   busy_timeout / cache_size / mmap_size: same intent as before, now actually applied.
	//
	// Deliberately NOT set: foreign_keys. This DB has run with FK enforcement OFF since day
	// one; turning it ON here could start rejecting writes against any pre-existing orphaned
	// row — a behaviour change unrelated to the perf fix, so it stays a separate, testable
	// decision. auto_vacuum / locking_mode dropped too: a DSN pragma cannot change auto_vacuum
	// without a full VACUUM, and locking_mode NORMAL is already the default (EXCLUSIVE would
	// defeat WAL's multi-reader benefit).
	//
	//   journal_size_limit: WAL 文件"缩回去"的上限，见 SQLiteJournalSizeLimitBytes 的注释。
	params := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		fmt.Sprintf("_pragma=busy_timeout(%d)", SQLiteBusyTimeoutMs),
		"_pragma=cache_size(10000)",
		"_pragma=mmap_size(268435456)",
		fmt.Sprintf("_pragma=journal_size_limit(%d)", SQLiteJournalSizeLimitBytes),
	}
	return gorm.Open(sqlite.Open(path+"?"+strings.Join(params, "&")), config)
}

func initMySQL(dsn string, config *gorm.Config) (*gorm.DB, error) {
	// DSN 格式: user:password@tcp(host:port)/dbname?charset=utf8mb4&parseTime=True&loc=Local
	if !strings.Contains(dsn, "?") {
		dsn += "?charset=utf8mb4&parseTime=True&loc=Local"
	}
	return gorm.Open(mysql.Open(dsn), config)
}

func initPostgres(dsn string, config *gorm.Config) (*gorm.DB, error) {
	// DSN 格式: host=localhost user=postgres password=xxx dbname=octopus port=5432 sslmode=disable
	return gorm.Open(postgres.Open(dsn), config)
}

func Close() error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func GetDB() *gorm.DB {
	return db
}
